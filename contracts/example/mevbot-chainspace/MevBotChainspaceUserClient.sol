// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";
import "../chainspace-utils/ChainspaceReason.sol";
import "./MevFlashLenderSimulator.sol";
import "./MevBotWalletSimulator.sol";
import "./MevPoolSimulator.sol";

/**
 * @title MevBotChainspaceUserClient
 * @dev 四参与方 2PC 编排：Flash(0) → Wallet(1) → Pool1(2) → Pool2(3)。模拟「借 A → Pool1 A→B → Pool2 B→A → 还贷」原子意图；
 *      非真实 MEV；UTXO 在 Wallet；池为 CPMM；与 amm-chainspace 相同「编排前 view 报价、三腿可乱序执行」假设。
 */
contract MevBotChainspaceUserClient {
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

    event MevBotChainspaceIntentStarted(
        bytes32 indexed txId,
        uint8 participantCount,
        address bot,
        uint256 borrowAmount,
        uint256 repayDue,
        uint256 aOut2
    );
    event MevBotChainspaceIntentFinalized(bytes32 indexed txId, bool success, uint8 finalReason);
    event PrepareLegSent(bytes32 indexed txId, uint8 indexed legIndex);

    constructor(
        address flash_,
        uint32 flashShard_,
        address wallet_,
        uint32 walletShard_,
        address pool1_,
        uint32 pool1Shard_,
        address pool2_,
        uint32 pool2Shard_
    ) {
        require(
            flash_ != address(0) && wallet_ != address(0) && pool1_ != address(0) && pool2_ != address(0),
            "MBCS: addr"
        );
        _parts.push(flash_);
        _pShards.push(flashShard_);
        _parts.push(wallet_);
        _pShards.push(walletShard_);
        _parts.push(pool1_);
        _pShards.push(pool1Shard_);
        _parts.push(pool2_);
        _pShards.push(pool2Shard_);
    }

    function participantCount() external view returns (uint256) {
        return _parts.length;
    }

    function participantAt(uint256 i) external view returns (address, uint32) {
        return (_parts[i], _pShards[i]);
    }

    function _flash() private view returns (MevFlashLenderSimulator) {
        require(_parts.length == 4, "MBCS: parts");
        return MevFlashLenderSimulator(_parts[0]);
    }

    function _wallet() private view returns (MevBotWalletSimulator) {
        require(_parts.length == 4, "MBCS: parts");
        return MevBotWalletSimulator(_parts[1]);
    }

    function _pool1() private view returns (MevPoolSimulator) {
        require(_parts.length == 4, "MBCS: parts");
        return MevPoolSimulator(_parts[2]);
    }

    function _pool2() private view returns (MevPoolSimulator) {
        require(_parts.length == 4, "MBCS: parts");
        return MevPoolSimulator(_parts[3]);
    }

    function _nextTxId() private returns (bytes32) {
        unchecked {
            _nonce++;
        }
        return keccak256(abi.encodePacked("MEV_CS", address(this), _nonce, block.number, msg.sender));
    }

    function _finalizeEarly(address bot, uint256 borrowAmount, uint256 repayDue, uint256 aOut2) private returns (bytes32 tid) {
        tid = _nextTxId();
        emit MevBotChainspaceIntentStarted(tid, 4, bot, borrowAmount, repayDue, aOut2);
        emit MevBotChainspaceIntentFinalized(tid, false, ChainspaceReason.BUSINESS_RULE);
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
        require(TwoPhaseLib.emitCrossShardRequest(targetShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, exec), "MBCS: xc");
    }

    function _openFourLegs(
        bytes32 txId,
        address user,
        uint256 borrowAmount,
        uint256 repayDue,
        uint256 aIn1,
        uint256 bOut1,
        uint256 bIn2,
        uint256 aOut2
    ) private {
        _wait[txId] = TxWait({total: 4, received: 0, mask: 0, anyFail: false, exists: true, aggFailReason: 0});
        emit MevBotChainspaceIntentStarted(txId, 4, user, borrowAmount, repayDue, aOut2);

        _emitPrepare(
            _pShards[0],
            _parts[0],
            abi.encodeWithSelector(MevFlashLenderSimulator.prepareLend.selector, txId, borrowAmount, repayDue)
        );
        emit PrepareLegSent(txId, 0);

        _emitPrepare(
            _pShards[1],
            _parts[1],
            abi.encodeWithSelector(
                MevBotWalletSimulator.prepareMevBot.selector,
                txId,
                user,
                borrowAmount,
                aIn1,
                bOut1,
                bIn2,
                aOut2,
                repayDue
            )
        );
        emit PrepareLegSent(txId, 1);

        _emitPrepare(
            _pShards[2],
            _parts[2],
            abi.encodeWithSelector(MevPoolSimulator.preparePoolSwapAForB.selector, txId, aIn1, uint256(0))
        );
        emit PrepareLegSent(txId, 2);

        _emitPrepare(
            _pShards[3],
            _parts[3],
            abi.encodeWithSelector(MevPoolSimulator.preparePoolSwapBForA.selector, txId, bIn2, uint256(0))
        );
        emit PrepareLegSent(txId, 3);
    }

    /// @param user 套利账户（与 Wallet 侧 note owner 一致）；`borrowAmount` 全部作为 Pool1 的 A in；`minProfitA` 为 `aOut2 - repayDue` 下限（可为 0）。
    function startFlashArb(address user, uint256 borrowAmount, uint256 minProfitA) external returns (bytes32 txId) {
        require(_parts.length == 4, "MBCS: parts");
        require(user != address(0), "MBCS: user");
        if (borrowAmount == 0) {
            return _finalizeEarly(user, 0, 0, 0);
        }

        uint256 repayDue = _flash().owedOnBorrow(borrowAmount);
        uint256 aIn1 = borrowAmount;
        uint256 bOut1 = _pool1().quoteSwapAForB(aIn1);
        if (bOut1 == 0) {
            return _finalizeEarly(user, borrowAmount, repayDue, 0);
        }
        uint256 bIn2 = bOut1;
        uint256 aOut2 = _pool2().quoteSwapBForA(bIn2);
        if (aOut2 < repayDue + minProfitA) {
            return _finalizeEarly(user, borrowAmount, repayDue, aOut2);
        }

        txId = _nextTxId();
        _openFourLegs(txId, user, borrowAmount, repayDue, aIn1, bOut1, bIn2, aOut2);
    }

    function startIntent(address user, bytes[] calldata calls) external returns (bytes32 txId) {
        require(user != address(0), "MBCS: user");
        require(calls.length == _parts.length && calls.length == 4, "MBCS: calls");
        txId = _nextTxId();
        _wait[txId] = TxWait({total: 4, received: 0, mask: 0, anyFail: false, exists: true, aggFailReason: 0});
        emit MevBotChainspaceIntentStarted(txId, 4, user, 0, 0, 0);
        for (uint256 i; i < _parts.length; i++) {
            _emitPrepare(_pShards[i], _parts[i], calls[i]);
            emit PrepareLegSent(txId, uint8(i));
        }
    }

    function onParticipantFinished(bytes32 txId, uint8 participantIndex, bool success, uint8 reason) external {
        TxWait storage w = _wait[txId];
        require(w.exists, "MBCS: tx");
        require(participantIndex < w.total, "MBCS: idx");
        uint256 bit = uint256(1) << uint256(participantIndex);
        require((w.mask & bit) == 0, "MBCS: dup");
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
            emit MevBotChainspaceIntentFinalized(txId, ok, fr);
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
