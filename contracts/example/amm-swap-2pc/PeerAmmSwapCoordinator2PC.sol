// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

/**
 * @title PeerAmmSwapCoordinator2PC
 * @dev 跨分片 AMM Swap 严格 2PC（Pool + WalletA + WalletB）：
 *
 *      阶段一（并行 prepare，三 Participant）：
 *        - Pool.prepareSwap
 *        - AmmWalletA2PC.prepareDebit（扣款并锁账户）
 *        - AmmWalletB2PC.prepareLock（仅锁账户，他人不可动）
 *      阶段二：全成则 WalletB.sealCredit(amountOut)；amountOut 优先取 Pool 回调 returnData，
 *      否则对本分片上的 `pool` 地址 staticcall `pendingSwaps(txId)`（不依赖 poolShard==getCurrentShardID，
 *      避免 0x6C 与构造参数不一致时读不到额）。
 *      阶段三：commit 或 abort 三端
 *
 *      user / amountIn 记在协调者本地；Pool 可与协调者异分片。
 *
 *      并发控制由 **AmmPool2PC** 的 `lockedByTxId` 承担：同一池在途仅一笔 prepare；协调者按 `txId` 分片维护阶段状态，无需协调者级全局锁。
 *      若 Pool 回调长期不到，池子锁由该笔的 commit/abort 释放；需链下治理或后续超时方案。
 *
 *      依赖 Relayer + 预编译 0x6D/0x74，与 PeerTransferCoordinator2PC 相同。
 */
contract PeerAmmSwapCoordinator2PC {
    address public immutable walletA;
    address public immutable walletB;
    address public immutable pool;
    uint32 public immutable shardA;
    uint32 public immutable shardB;
    uint32 public immutable poolShard;

    uint256 private _nonce;
    mapping(uint256 => bytes32) private _requestToTxId;
    mapping(uint256 => uint8) private _requestToParticipant;

    /// @dev 阶段一投票：0=pool, 1=walletA debit, 2=walletB lock
    mapping(bytes32 => bool[3]) private _phase1Votes;
    mapping(bytes32 => uint8) private _phase1Count;
    mapping(bytes32 => uint256) private _poolAmountOut;

    mapping(bytes32 => address) private _swapUser;
    mapping(bytes32 => uint256) private _swapAmountIn;

    event PeerAmmSwap2PCStarted(bytes32 indexed txId, address indexed user, uint256 amountIn, uint256 minAmountOut);
    event PeerAmmSwap2PCCommitted(bytes32 indexed txId);
    event PeerAmmSwap2PCAborted(bytes32 indexed txId);
    /// @dev reason: 1=阶段一未全成 2=阶段一全成但 amountOut 仍为 0 3=seal 跨分片发出失败 4=seal 执行失败
    event PeerAmmSwap2PCAbortReason(bytes32 indexed txId, uint8 reason, bool poolOk, bool walletAOk, bool walletBOk);

    constructor(
        address walletA_,
        address walletB_,
        address pool_,
        uint32 shardA_,
        uint32 shardB_,
        uint32 poolShard_
    ) {
        require(walletA_ != address(0) && walletB_ != address(0) && pool_ != address(0), "AMM2PC: addr");
        require(walletA_ != walletB_, "AMM2PC: same wallet");
        walletA = walletA_;
        walletB = walletB_;
        pool = pool_;
        shardA = shardA_;
        shardB = shardB_;
        poolShard = poolShard_;
    }

    /// @dev 读 Pool 上 pending 的 amountOut（协调者所在分片上 `pool` 地址的存储；Pool 与协调者同分片部署时有效）
    function _readPoolPendingAmountOut(bytes32 txId) private view returns (uint256) {
        (bool scOk, bytes memory data) = pool.staticcall(abi.encodeWithSignature("pendingSwaps(bytes32)", txId));
        if (!scOk || data.length < 128) {
            return 0;
        }
        (, , uint256 ao, bool ex) = abi.decode(data, (address, uint256, uint256, bool));
        if (ex && ao > 0) {
            return ao;
        }
        return 0;
    }

    /// @dev Pool prepare 成功后：先解码 returnData，再读 pendingSwaps（适配 ret 为空或 ABI 与预期不符）
    function _capturePoolAmountOut(bytes32 txId, bool poolOk, bytes calldata ret) internal view returns (uint256) {
        if (!poolOk) {
            return 0;
        }
        if (ret.length >= 32) {
            uint256 fromRet = abi.decode(ret, (uint256));
            if (fromRet > 0) {
                return fromRet;
            }
        }
        return _readPoolPendingAmountOut(txId);
    }

    function _nextTxId() internal returns (bytes32) {
        bytes32 txId = keccak256(abi.encodePacked(block.timestamp, msg.sender, block.number, _nonce));
        _nonce++;
        return txId;
    }

    function startSwap(address user, uint256 amountIn, uint256 minAmountOut) public {
        require(amountIn > 0, "AMM2PC: amountIn");
        require(user != address(0), "AMM2PC: user");

        bytes32 txId = _nextTxId();
        _swapUser[txId] = user;
        _swapAmountIn[txId] = amountIn;

        emit PeerAmmSwap2PCStarted(txId, user, amountIn, minAmountOut);

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        bytes memory poolCalldata = abi.encodeWithSignature(
            "prepareSwap(bytes32,address,uint256,uint256)",
            txId,
            user,
            amountIn,
            minAmountOut
        );
        require(_emitPrepare(poolShard, pool, selfShard, txId, 0, poolCalldata), "AMM2PC: emit pool prepare");

        bytes memory debitCalldata =
            abi.encodeWithSignature("prepareDebit(bytes32,address,uint256)", txId, user, amountIn);
        require(_emitPrepare(shardA, walletA, selfShard, txId, 1, debitCalldata), "AMM2PC: emit walletA prepare");

        bytes memory lockCalldata = abi.encodeWithSignature("prepareLock(bytes32,address)", txId, user);
        require(_emitPrepare(shardB, walletB, selfShard, txId, 2, lockCalldata), "AMM2PC: emit walletB prepare");
    }

    function _emitPrepare(
        uint32 targetShardId,
        address targetAddr,
        uint32 sourceShardId,
        bytes32 txId,
        uint8 participantIndex,
        bytes memory targetCalldata
    ) internal returns (bool) {
        uint256 requestId = uint256(keccak256(abi.encodePacked(txId, participantIndex, block.timestamp))) % (2**128);
        _requestToTxId[requestId] = txId;
        _requestToParticipant[requestId] = participantIndex;

        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onPrepareResponse.selector,
            requestId,
            targetAddr,
            targetCalldata
        );

        return TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata);
    }

    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata ret) external {
        bytes32 txId = _requestToTxId[requestId];
        uint8 idx = _requestToParticipant[requestId];
        delete _requestToTxId[requestId];
        delete _requestToParticipant[requestId];

        require(txId != bytes32(0), "AMM2PC: unknown request");

        // 阶段二：WalletB sealCredit
        if (idx == 3) {
            _handleSealResponse(txId, ok);
            return;
        }

        require(idx <= 2, "AMM2PC: idx");

        if (idx == 0) {
            uint256 outAmt = _capturePoolAmountOut(txId, ok, ret);
            if (outAmt > 0) {
                _poolAmountOut[txId] = outAmt;
            }
        }

        _phase1Votes[txId][idx] = ok;
        _phase1Count[txId]++;

        if (_phase1Count[txId] != 3) {
            return;
        }

        bool pOk = _phase1Votes[txId][0];
        bool aOk = _phase1Votes[txId][1];
        bool bOk = _phase1Votes[txId][2];
        bool allOk = pOk && aOk && bOk;
        delete _phase1Votes[txId];
        delete _phase1Count[txId];

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        address user = _swapUser[txId];
        uint256 amountOut = _poolAmountOut[txId];
        delete _poolAmountOut[txId];

        if (!allOk) {
            emit PeerAmmSwap2PCAbortReason(txId, 1, pOk, aOk, bOk);
            delete _swapUser[txId];
            delete _swapAmountIn[txId];
            _emitCommitOrAbort3(txId, false, selfShard);
            emit PeerAmmSwap2PCAborted(txId);
            return;
        }

        if (amountOut == 0) {
            amountOut = _readPoolPendingAmountOut(txId);
        }

        if (amountOut == 0) {
            emit PeerAmmSwap2PCAbortReason(txId, 2, pOk, aOk, bOk);
            delete _swapUser[txId];
            delete _swapAmountIn[txId];
            _emitCommitOrAbort3(txId, false, selfShard);
            emit PeerAmmSwap2PCAborted(txId);
            return;
        }

        bytes memory sealCalldata =
            abi.encodeWithSignature("sealCredit(bytes32,address,uint256)", txId, user, amountOut);
        if (!_emitPrepare(shardB, walletB, selfShard, txId, 3, sealCalldata)) {
            emit PeerAmmSwap2PCAbortReason(txId, 3, true, true, true);
            delete _swapUser[txId];
            delete _swapAmountIn[txId];
            _emitCommitOrAbort3(txId, false, selfShard);
            emit PeerAmmSwap2PCAborted(txId);
        }
    }

    function _handleSealResponse(bytes32 txId, bool ok) internal {
        delete _swapUser[txId];
        delete _swapAmountIn[txId];

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        if (ok) {
            _emitCommitOrAbort3(txId, true, selfShard);
            emit PeerAmmSwap2PCCommitted(txId);
        } else {
            emit PeerAmmSwap2PCAbortReason(txId, 4, true, true, true);
            _emitCommitOrAbort3(txId, false, selfShard);
            emit PeerAmmSwap2PCAborted(txId);
        }
    }

    function _emitCommitOrAbort3(bytes32 txId, bool doCommit, uint32 sourceShardId) internal {
        bytes4 sel = doCommit ? bytes4(keccak256("commit(bytes32)")) : bytes4(keccak256("abort(bytes32)"));
        bytes memory calldataP = abi.encodeWithSelector(sel, txId);
        bytes memory calldataA = abi.encodeWithSelector(sel, txId);
        bytes memory calldataB = abi.encodeWithSelector(sel, txId);

        uint256 baseReqId = uint256(txId) % (2**120);

        bytes memory execP = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            baseReqId,
            pool,
            calldataP
        );
        bytes memory execA = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            baseReqId + 1,
            walletA,
            calldataA
        );
        bytes memory execB = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            baseReqId + 2,
            walletB,
            calldataB
        );

        TwoPhaseLib.emitCrossShardRequest(poolShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, execP);
        TwoPhaseLib.emitCrossShardRequest(shardA, TwoPhaseLib.PRECOMPILE_EXECUTOR, execA);
        TwoPhaseLib.emitCrossShardRequest(shardB, TwoPhaseLib.PRECOMPILE_EXECUTOR, execB);
    }

    function onCommitResponse(uint256, bool, bytes calldata) external {}
}
