// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

/**
 * @title PeerMevArbCoordinator2PC
 * @dev 跨分片 2PC 简化 MEV：Flash 借 A → 低价池 A→B → 高价池 B→A → 还贷 → 利润入账。
 *
 *      波次 1（并行）：`MevArbFlashLender2PC.prepareLend`、`MevArbPoolLow2PC.prepareSwapAToB`
 *      → 从低价池 prepare **回调 `ret`** 取 `outLow`；**不**对 poolHigh 做 staticcall 预览，跨分片直接发起 `prepareSwapBToA(..., minOutA = borrowA + minNetProfit)`，由高价池链上校验。
 *      波次 2：`MevArbPoolHigh2PC.prepareSwapBToA(outLow, minOutA, ...)`，失败则 abort；成功则从 **回调 `ret`** 取 `outHigh`。
 *      波次 3：`MevArbProfitWallet2PC.prepareLock`
 *      波次 4：`sealCredit` 写入净利润
 *      最后：四端 `commit`；任阶段失败则对已 prepare 的端 `abort`。
 *
 *      事件名 `PeerMevArb2PCStarted/Committed/Aborted` 供 `twopc-metrics` / `joyue-trigger` 解析（与 AMM 2PC 同形 topic 布局）。
 */
contract PeerMevArbCoordinator2PC {
    address public immutable flash;
    address public immutable poolLow;
    address public immutable poolHigh;
    address public immutable profit;
    uint32 public immutable shardFlash;
    uint32 public immutable shardLow;
    uint32 public immutable shardHigh;
    uint32 public immutable shardProfit;

    uint256 private _nonce;
    uint256 private _reqNonce;

    mapping(uint256 => bytes32) private _requestToTxId;
    mapping(uint256 => uint8) private _requestToParticipant;

    mapping(bytes32 => bool[2]) private _wave1Votes;
    mapping(bytes32 => uint8) private _wave1Count;
    /// @dev 波次 1 低价池回调解出的 amountOutB；与 poolLow 异分片时不能靠 staticcall 读 pendingSwaps
    mapping(bytes32 => uint256) private _wave1OutLow;

    mapping(bytes32 => address) private _arbUser;
    mapping(bytes32 => uint256) private _borrowA;
    mapping(bytes32 => uint256) private _minNetProfitA;
    mapping(bytes32 => uint256) private _netProfit;

    event PeerMevArb2PCStarted(bytes32 indexed txId, address indexed user, uint256 borrowA, uint256 minNetProfitA);
    event PeerMevArb2PCCommitted(bytes32 indexed txId);
    event PeerMevArb2PCAborted(bytes32 indexed txId);
    /// @dev reason: 1=波次1未全成 2=outLow 缺失 3=高价池 prepare 失败或 outHigh 不足 4=利润 lock 失败 5=seal 发出失败 6=seal 执行失败
    event PeerMevArb2PCAbortReason(bytes32 indexed txId, uint8 reason, bool flashOk, bool lowOk, bool highOk);

    constructor(
        address flash_,
        address poolLow_,
        address poolHigh_,
        address profit_,
        uint32 shardFlash_,
        uint32 shardLow_,
        uint32 shardHigh_,
        uint32 shardProfit_
    ) {
        require(flash_ != address(0) && poolLow_ != address(0) && poolHigh_ != address(0) && profit_ != address(0), "MEV2PC: addr");
        flash = flash_;
        poolLow = poolLow_;
        poolHigh = poolHigh_;
        profit = profit_;
        shardFlash = shardFlash_;
        shardLow = shardLow_;
        shardHigh = shardHigh_;
        shardProfit = shardProfit_;
    }

    function _nextTxId() internal returns (bytes32) {
        bytes32 txId = keccak256(abi.encodePacked(block.timestamp, msg.sender, block.number, _nonce));
        _nonce++;
        return txId;
    }

    function startArb(address user, uint256 borrowA, uint256 minNetProfitA) public {
        require(borrowA > 0, "MEV2PC: borrow");
        require(user != address(0), "MEV2PC: user");

        bytes32 txId = _nextTxId();
        _arbUser[txId] = user;
        _borrowA[txId] = borrowA;
        _minNetProfitA[txId] = minNetProfitA;

        emit PeerMevArb2PCStarted(txId, user, borrowA, minNetProfitA);

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        bytes memory cFlash = abi.encodeWithSignature("prepareLend(bytes32,address,uint256)", txId, user, borrowA);
        require(_emitPrepare(shardFlash, flash, selfShard, txId, 0, cFlash), "MEV2PC: flash prepare");

        bytes memory cLow =
            abi.encodeWithSignature("prepareSwapAToB(bytes32,address,uint256,uint256)", txId, user, borrowA, 1);
        require(_emitPrepare(shardLow, poolLow, selfShard, txId, 1, cLow), "MEV2PC: low prepare");
    }

    function _emitPrepare(
        uint32 targetShardId,
        address targetAddr,
        uint32 sourceShardId,
        bytes32 txId,
        uint8 participantIndex,
        bytes memory targetCalldata
    ) internal returns (bool) {
        unchecked {
            _reqNonce++;
        }
        uint256 requestId = uint256(keccak256(abi.encodePacked(txId, participantIndex, _reqNonce, block.timestamp)))
            % (2 ** 128);
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
        require(txId != bytes32(0), "MEV2PC: unknown req");

        if (idx == 4) {
            _handleSealResponse(txId, ok);
            return;
        }

        if (idx <= 1) {
            if (idx == 1 && ok && ret.length >= 32) {
                _wave1OutLow[txId] = abi.decode(ret, (uint256));
            }
            _wave1Votes[txId][idx] = ok;
            _wave1Count[txId]++;
            if (_wave1Count[txId] != 2) {
                return;
            }

            bool fOk = _wave1Votes[txId][0];
            bool lOk = _wave1Votes[txId][1];
            delete _wave1Votes[txId];
            delete _wave1Count[txId];

            uint32 selfShard = TwoPhaseLib.getCurrentShardID();
            address user = _arbUser[txId];
            uint256 borrowA = _borrowA[txId];
            uint256 minNet = _minNetProfitA[txId];

            if (!fOk || !lOk) {
                emit PeerMevArb2PCAbortReason(txId, 1, fOk, lOk, false);
                _abortFlashLow(txId, selfShard);
                _clearArb(txId);
                emit PeerMevArb2PCAborted(txId);
                return;
            }

            uint256 outLow = _wave1OutLow[txId];
            delete _wave1OutLow[txId];
            if (outLow == 0) {
                emit PeerMevArb2PCAbortReason(txId, 2, true, true, false);
                _abortFlashLow(txId, selfShard);
                _clearArb(txId);
                emit PeerMevArb2PCAborted(txId);
                return;
            }

            uint256 minAOut = borrowA + minNet;
            if (!_emitPrepareHigh(txId, selfShard, user, outLow, minAOut)) {
                emit PeerMevArb2PCAbortReason(txId, 3, true, true, false);
                _abortFlashLow(txId, selfShard);
                _clearArb(txId);
                emit PeerMevArb2PCAborted(txId);
            }
            return;
        }

        if (idx == 2) {
            uint256 outHigh = 0;
            if (ok && ret.length >= 32) {
                outHigh = abi.decode(ret, (uint256));
            }

            uint32 selfShard = TwoPhaseLib.getCurrentShardID();
            address user = _arbUser[txId];
            uint256 borrowA = _borrowA[txId];
            uint256 minNet = _minNetProfitA[txId];

            if (!ok || outHigh < borrowA + minNet) {
                emit PeerMevArb2PCAbortReason(txId, 3, true, true, ok);
                _abortFlashLowHigh(txId, selfShard);
                _clearArb(txId);
                emit PeerMevArb2PCAborted(txId);
                return;
            }

            unchecked {
                _netProfit[txId] = outHigh - borrowA;
            }

            if (!_emitPrepareProfitLock(txId, selfShard, user)) {
                emit PeerMevArb2PCAbortReason(txId, 4, true, true, true);
                _abortFlashLowHigh(txId, selfShard);
                _clearArb(txId);
                emit PeerMevArb2PCAborted(txId);
            }
            return;
        }

        if (idx == 3) {
            uint32 selfShard = TwoPhaseLib.getCurrentShardID();
            address user = _arbUser[txId];
            uint256 netP = _netProfit[txId];

            if (!ok) {
                emit PeerMevArb2PCAbortReason(txId, 4, true, true, true);
                _abortFlashLowHigh(txId, selfShard);
                _abortProfit(txId, selfShard);
                _clearArb(txId);
                emit PeerMevArb2PCAborted(txId);
                return;
            }

            if (!_emitPrepareProfitSeal(txId, selfShard, user, netP)) {
                emit PeerMevArb2PCAbortReason(txId, 5, true, true, true);
                _abortFlashLowHigh(txId, selfShard);
                _abortProfit(txId, selfShard);
                _clearArb(txId);
                emit PeerMevArb2PCAborted(txId);
                return;
            }
            return;
        }
    }

    function _emitPrepareHigh(
        bytes32 txId,
        uint32 selfShard,
        address user,
        uint256 outLow,
        uint256 minAOut
    ) private returns (bool) {
        uint256 reqId = _nextReqId(txId, 2);
        bytes memory cHigh =
            abi.encodeWithSignature("prepareSwapBToA(bytes32,address,uint256,uint256)", txId, user, outLow, minAOut);
        return TwoPhaseLib.emitCrossShardRequest(
            shardHigh,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            TwoPhaseLib.buildExecutorCalldata(
                selfShard, address(this), this.onPrepareResponse.selector, reqId, poolHigh, cHigh
            )
        );
    }

    function _emitPrepareProfitLock(bytes32 txId, uint32 selfShard, address user) private returns (bool) {
        uint256 reqId = _nextReqId(txId, 3);
        bytes memory cLock = abi.encodeWithSignature("prepareLock(bytes32,address)", txId, user);
        return TwoPhaseLib.emitCrossShardRequest(
            shardProfit,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            TwoPhaseLib.buildExecutorCalldata(
                selfShard, address(this), this.onPrepareResponse.selector, reqId, profit, cLock
            )
        );
    }

    function _emitPrepareProfitSeal(bytes32 txId, uint32 selfShard, address user, uint256 netP) private returns (bool) {
        uint256 reqId = _nextReqId(txId, 4);
        bytes memory cSeal = abi.encodeWithSignature("sealCredit(bytes32,address,uint256)", txId, user, netP);
        return TwoPhaseLib.emitCrossShardRequest(
            shardProfit,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            TwoPhaseLib.buildExecutorCalldata(
                selfShard, address(this), this.onPrepareResponse.selector, reqId, profit, cSeal
            )
        );
    }

    function _nextReqId(bytes32 txId, uint8 idx) private returns (uint256 requestId) {
        unchecked {
            _reqNonce++;
        }
        requestId = uint256(keccak256(abi.encodePacked(txId, idx, _reqNonce, block.timestamp))) % (2 ** 128);
        _requestToTxId[requestId] = txId;
        _requestToParticipant[requestId] = idx;
    }

    function _clearArb(bytes32 txId) private {
        delete _arbUser[txId];
        delete _borrowA[txId];
        delete _minNetProfitA[txId];
        delete _netProfit[txId];
        delete _wave1OutLow[txId];
    }

    function _handleSealResponse(bytes32 txId, bool ok) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        if (ok) {
            _emitCommit4(txId, true, selfShard);
            emit PeerMevArb2PCCommitted(txId);
        } else {
            emit PeerMevArb2PCAbortReason(txId, 6, true, true, true);
            _abortFlashLowHigh(txId, selfShard);
            _abortProfit(txId, selfShard);
            emit PeerMevArb2PCAborted(txId);
        }
        _clearArb(txId);
    }

    function _abortFlashLow(bytes32 txId, uint32 sourceShardId) private {
        _emitAbort2(txId, sourceShardId, flash, shardFlash, poolLow, shardLow);
    }

    function _abortFlashLowHigh(bytes32 txId, uint32 sourceShardId) private {
        _emitAbort2(txId, sourceShardId, flash, shardFlash, poolLow, shardLow);
        _emitAbort1(txId, sourceShardId, poolHigh, shardHigh);
    }

    function _abortProfit(bytes32 txId, uint32 sourceShardId) private {
        _emitAbort1(txId, sourceShardId, profit, shardProfit);
    }

    function _emitAbort1(bytes32 txId, uint32 sourceShardId, address target, uint32 targetShard) private {
        bytes memory calldata_ = abi.encodeWithSignature("abort(bytes32)", txId);
        bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId, address(this), this.onCommitResponse.selector, _dummyReqId(), target, calldata_
        );
        TwoPhaseLib.emitCrossShardRequest(targetShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, exec);
    }

    function _emitAbort2(
        bytes32 txId,
        uint32 sourceShardId,
        address t0,
        uint32 s0,
        address t1,
        uint32 s1
    ) private {
        _emitAbort1(txId, sourceShardId, t0, s0);
        _emitAbort1(txId, sourceShardId, t1, s1);
    }

    function _dummyReqId() private returns (uint256) {
        unchecked {
            _reqNonce++;
        }
        return uint256(keccak256(abi.encodePacked(_reqNonce, block.timestamp))) % (2 ** 127);
    }

    function _emitCommit4(bytes32 txId, bool doCommit, uint32 sourceShardId) private {
        bytes4 sel = doCommit ? bytes4(keccak256("commit(bytes32)")) : bytes4(keccak256("abort(bytes32)"));
        bytes memory cd = abi.encodeWithSelector(sel, txId);
        uint256 b = uint256(txId) % (2 ** 120);

        TwoPhaseLib.emitCrossShardRequest(
            shardFlash,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            TwoPhaseLib.buildExecutorCalldata(sourceShardId, address(this), this.onCommitResponse.selector, b + 0, flash, cd)
        );
        TwoPhaseLib.emitCrossShardRequest(
            shardLow,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            TwoPhaseLib.buildExecutorCalldata(sourceShardId, address(this), this.onCommitResponse.selector, b + 1, poolLow, cd)
        );
        TwoPhaseLib.emitCrossShardRequest(
            shardHigh,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            TwoPhaseLib.buildExecutorCalldata(sourceShardId, address(this), this.onCommitResponse.selector, b + 2, poolHigh, cd)
        );
        TwoPhaseLib.emitCrossShardRequest(
            shardProfit,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            TwoPhaseLib.buildExecutorCalldata(sourceShardId, address(this), this.onCommitResponse.selector, b + 3, profit, cd)
        );
    }

    function onCommitResponse(uint256, bool, bytes calldata) external {}
}
