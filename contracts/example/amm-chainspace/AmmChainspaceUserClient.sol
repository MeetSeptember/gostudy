// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";
import "../chainspace-utils/ChainspaceReason.sol";
import "./AmmWalletASimulator.sol";
import "./AmmWalletBSimulator.sol";
import "./AmmPoolSimulator.sol";

/**
 * @title AmmChainspaceUserClient
 * @dev 三参与方 2PC：`WalletA`(index0) 扣 A、`WalletB`(index1) 记 B 出库、`AmmPool`(index2) 调储备；与 nft-chainspace 编排一致。
 *      公式：`amountOut = amountIn * resB / (resA + amountIn)`（池侧与两腿使用 UserClient 计算的同一 `amountOut`）。
 *      WalletA 腿在链上仍传 `objectIds[]` 给 `prepareDebitA`；`startSwap` 内由本合约按 owner 列表贪心自动选币至 `sum >= amountIn`。
 *      构造顺序：`_parts[0]=WalletA`、`_parts[1]=WalletB`、`_parts[2]=Pool`（须与各 Simulator `MY_INDEX` 一致）。
 */
contract AmmChainspaceUserClient {
    address[] private _parts;
    uint32[] private _pShards;
    uint256 private _nonce;
    uint256 private _xcReq;

    struct TxWait {
        uint8 total;
        uint8 received;
        uint256 mask;
        bool anyFail;
        bool exists;
        uint8 aggFailReason;
    }

    mapping(bytes32 => TxWait) private _wait;

    struct ThreeLegCtx {
        bytes32 txId;
        address user;
        uint256 amountIn;
        uint256 amountOut;
        uint256 minOut;
        bytes32[] objectIds;
    }

    event AmmChainspaceIntentStarted(
        bytes32 indexed txId,
        uint8 participantCount,
        address user,
        uint256 amountIn,
        uint256 amountOut,
        uint256 minOut
    );
    event AmmChainspaceIntentFinalized(bytes32 indexed txId, bool success, uint8 finalReason);
    event PrepareLegSent(bytes32 indexed txId, uint8 indexed legIndex);

    constructor(
        address walletA_,
        uint32 walletAShard_,
        address walletB_,
        uint32 walletBShard_,
        address pool_,
        uint32 poolShard_
    ) {
        require(walletA_ != address(0) && walletB_ != address(0) && pool_ != address(0), "ACS: addr");
        _parts.push(walletA_);
        _pShards.push(walletAShard_);
        _parts.push(walletB_);
        _pShards.push(walletBShard_);
        _parts.push(pool_);
        _pShards.push(poolShard_);
    }

    function participantCount() external view returns (uint256) {
        return _parts.length;
    }

    function participantAt(uint256 i) external view returns (address, uint32) {
        return (_parts[i], _pShards[i]);
    }

    function _walletA() private view returns (AmmWalletASimulator) {
        require(_parts.length == 3, "ACS: parts");
        return AmmWalletASimulator(_parts[0]);
    }

    function _walletB() private view returns (AmmWalletBSimulator) {
        require(_parts.length == 3, "ACS: parts");
        return AmmWalletBSimulator(_parts[1]);
    }

    function _pool() private view returns (AmmPoolSimulator) {
        require(_parts.length == 3, "ACS: parts");
        return AmmPoolSimulator(_parts[2]);
    }

    function _tryQuote(uint256 amountIn) private view returns (bool ok, uint256 amountOut) {
        if (amountIn == 0) return (false, 0);
        amountOut = _pool().quoteSwapAForB(amountIn);
        if (amountOut == 0) return (false, 0);
        return (true, amountOut);
    }

    function _finalizeEarly(address user_, uint256 amountIn_, uint256 amountOut_, uint256 minOut_) private returns (bytes32 tid_) {
        tid_ = _nextTxId();
        emit AmmChainspaceIntentStarted(tid_, 3, user_, amountIn_, amountOut_, minOut_);
        emit AmmChainspaceIntentFinalized(tid_, false, ChainspaceReason.BUSINESS_RULE);
    }

    function _nextTxId() private returns (bytes32) {
        unchecked {
            _nonce++;
        }
        return keccak256(abi.encodePacked("AMM_CS", address(this), _nonce, block.number, msg.sender));
    }

    function onPrepareLaunched(uint256, bool, bytes calldata) external {}

    function _emitPrepare(uint32 targetShard, address target, bytes memory targetCalldata) private {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        unchecked {
            _xcReq++;
        }
        uint256 requestId = uint256(keccak256(abi.encodePacked("PRE", _xcReq, targetShard, target))) % (2 ** 128);
        bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
            selfShard,
            address(this),
            this.onPrepareLaunched.selector,
            requestId,
            target,
            targetCalldata
        );
        require(TwoPhaseLib.emitCrossShardRequest(targetShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, exec), "ACS: xc");
    }

    /// @dev 与 WalletA `sumDebitObjects` 一致：非空、无重复、均为 user 的 ACTIVE，且 `sum >= amountIn`。
    function _debitObjectsOk(address user, uint256 amountIn, bytes32[] memory objectIds) private view returns (bool) {
        if (objectIds.length == 0) return false;
        (uint256 s, bool ok) = _walletA().sumDebitObjects(user, objectIds);
        return ok && s >= amountIn;
    }

    /// @dev 按 `objectIdForOwnerAt` 顺序贪心选取 ACTIVE object，直到累计面值 `>= amountIn`。
    function _pickObjectIdsForDebit(address user, uint256 amountIn) private view returns (bool ok, bytes32[] memory ids) {
        AmmWalletASimulator wa = _walletA();
        uint256 n = wa.countObjectsForOwner(user);
        bytes32[] memory tmp = new bytes32[](n);
        uint256 sum;
        uint256 cnt;
        for (uint256 i; i < n && sum < amountIn; i++) {
            bytes32 oid = wa.objectIdForOwnerAt(user, i);
            (address ow, uint256 v, AmmWalletASimulator.Status st) = wa.objects(oid);
            if (ow != user || st != AmmWalletASimulator.Status.ACTIVE) {
                continue;
            }
            sum += v;
            tmp[cnt++] = oid;
        }
        if (sum < amountIn) {
            return (false, new bytes32[](0));
        }
        ids = new bytes32[](cnt);
        for (uint256 j; j < cnt; j++) {
            ids[j] = tmp[j];
        }
        return (true, ids);
    }

    function _openThreeLegs(ThreeLegCtx memory c) private {
        _wait[c.txId] = TxWait({total: 3, received: 0, mask: 0, anyFail: false, exists: true, aggFailReason: 0});
        emit AmmChainspaceIntentStarted(c.txId, 3, c.user, c.amountIn, c.amountOut, c.minOut);

        _emitPrepare(
            _pShards[0],
            _parts[0],
            abi.encodeWithSelector(
                AmmWalletASimulator.prepareDebitA.selector,
                c.txId,
                c.user,
                c.amountIn,
                c.objectIds
            )
        );
        emit PrepareLegSent(c.txId, 0);
        _emitPrepare(
            _pShards[1],
            _parts[1],
            abi.encodeWithSelector(AmmWalletBSimulator.prepareCreditB.selector, c.txId, c.user, c.amountOut)
        );
        emit PrepareLegSent(c.txId, 1);
        _emitPrepare(
            _pShards[2],
            _parts[2],
            abi.encodeWithSelector(AmmPoolSimulator.preparePoolSwap.selector, c.txId, c.amountIn, c.minOut)
        );
        emit PrepareLegSent(c.txId, 2);
    }

    /// @dev 在已通过报价与 `minOut` 的前提下：校验 `sumDebitObjects`、金库，分配 `txId` 并打开三腿。
    function _finishSwapWithPickedInputs(
        address user,
        uint256 amountIn,
        uint256 minOut,
        uint256 amountOut,
        bytes32[] memory objectIds
    ) private returns (bytes32 txId) {
        if (!_debitObjectsOk(user, amountIn, objectIds)) {
            return _finalizeEarly(user, amountIn, amountOut, minOut);
        }
        if (_walletB().vaultB() < amountOut) {
            return _finalizeEarly(user, amountIn, amountOut, minOut);
        }
        txId = _nextTxId();
        ThreeLegCtx memory c;
        c.txId = txId;
        c.user = user;
        c.amountIn = amountIn;
        c.amountOut = amountOut;
        c.minOut = minOut;
        c.objectIds = objectIds;
        _openThreeLegs(c);
    }

    /// @dev `objectIds` 由本合约 `_pickObjectIdsForDebit` 自动选取。
    function startSwap(address user, uint256 amountIn, uint256 minOut) external returns (bytes32 txId) {
        require(_parts.length == 3, "ACS: parts");
        require(user != address(0), "ACS: user");
        (bool qOk, uint256 amountOut) = _tryQuote(amountIn);
        if (!qOk) {
            return _finalizeEarly(user, amountIn, 0, minOut);
        }
        if (amountOut < minOut) {
            return _finalizeEarly(user, amountIn, amountOut, minOut);
        }
        (bool picked, bytes32[] memory objectIds) = _pickObjectIdsForDebit(user, amountIn);
        if (!picked) {
            return _finalizeEarly(user, amountIn, amountOut, minOut);
        }
        return _finishSwapWithPickedInputs(user, amountIn, minOut, amountOut, objectIds);
    }

    /// @dev 第一腿 calldata 须为 `prepareDebitA(txId, user, amountIn, objectIds)` ABI 编码（与编排器发出的 WalletA 腿一致）。
    function startIntent(bytes[] calldata calls) external returns (bytes32 txId) {
        require(calls.length == _parts.length && calls.length == 3, "ACS: calls");
        txId = _nextTxId();
        _wait[txId] = TxWait({total: 3, received: 0, mask: 0, anyFail: false, exists: true, aggFailReason: 0});
        emit AmmChainspaceIntentStarted(txId, 3, msg.sender, 0, 0, 0);

        for (uint256 i = 0; i < _parts.length; i++) {
            _emitPrepare(_pShards[i], _parts[i], calls[i]);
            emit PrepareLegSent(txId, uint8(i));
        }
    }

    function onParticipantFinished(bytes32 txId, uint8 participantIndex, bool success, uint8 reason) external {
        TxWait storage w = _wait[txId];
        require(w.exists, "ACS: tx");
        require(participantIndex < w.total, "ACS: idx");
        uint256 bit = uint256(1) << uint256(participantIndex);
        require((w.mask & bit) == 0, "ACS: dup");
        w.mask |= bit;
        w.received++;
        if (!success) {
            w.anyFail = true;
            w.aggFailReason = _mergeAggFailReason(w.aggFailReason, reason);
        }
        if (w.received == w.total) {
            bool ok = !w.anyFail;
            uint8 fr = ok
                ? ChainspaceReason.OK
                : (w.aggFailReason == 0 ? ChainspaceReason.OTHER : w.aggFailReason);
            emit AmmChainspaceIntentFinalized(txId, ok, fr);
            delete _wait[txId];
        }
    }

    function _mergeAggFailReason(uint8 cur, uint8 inc) private pure returns (uint8) {
        return _legReasonPriority(inc) > _legReasonPriority(cur) ? inc : cur;
    }

    function _legReasonPriority(uint8 r) private pure returns (uint256) {
        if (r == ChainspaceReason.LOCK_CONFLICT) return 100;
        if (r == ChainspaceReason.BUSINESS_RULE) return 80;
        if (r == ChainspaceReason.FOLLOWER_ABORT) return 50;
        if (r == ChainspaceReason.OTHER) return 20;
        return 0;
    }
}
