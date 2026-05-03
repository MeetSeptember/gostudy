// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";
import "./ChainspaceReason.sol";

/**
 * @title TwoPCPeerBase
 * @dev 可扩展「多参与方」2PC 信令基类：本地 prepare 成功后向所有其它参与方广播 `peerPrepareOk(txId, myIndex)`；
 *      任一方 `prepare` 锁失败则广播 `peerPrepareFailed(txId)`，已 prepare 的参与方必须 `abort` 本地并结束。
 *      当 `localPrepared && peerOkMask` 覆盖所有其它 index 时调用 `_commitLocal(txId)`；若 `globalFail` 则 `_abortLocal(txId)`。
 *      子类在到达终态时调用 `_notifyUser(txId, success, reason)`（恰一次）；`reason` 见 `ChainspaceReason`。
 */
abstract contract TwoPCPeerBase {
    uint8 public immutable MY_INDEX;
    uint8 public immutable TOTAL_PARTICIPANTS;

    /// @dev 可部署后再 `setUserClient`（跨分片部署时常需先部署 Simulator 再部署 User）
    address public userClient;
    uint32 public userShard;
    bool public userClientFixed;

    address[] internal _peerAddrs;
    uint32[] internal _peerShards;

    uint256 private _xcNonce;

    struct PeerProgress {
        bool globalFail;
        bool localPrepared;
        bool terminal;
        uint256 peerOkMask;
    }

    mapping(bytes32 => PeerProgress) internal _peer;
    /// @dev 任一方 `prepare` 锁失败并已广播 `peerPrepareFailed` 后置位；尚未本地 prepare 的一方读到后应直接 `return false`（避免死等 peerOk）
    mapping(bytes32 => bool) internal _txPrepareNacked;

    event PeerPrepareOkSent(bytes32 indexed txId, uint8 fromIndex);
    event PeerPrepareFailedSent(bytes32 indexed txId);
    event TwoPCCommitted(bytes32 indexed txId);
    event TwoPCAborted(bytes32 indexed txId);

    constructor(uint8 myIndex_, uint8 totalParticipants_, address[] memory peerAddrs_, uint32[] memory peerShards_) {
        require(totalParticipants_ >= 2 && totalParticipants_ <= 8, "TP: N");
        require(myIndex_ < totalParticipants_, "TP: idx");
        require(peerAddrs_.length == peerShards_.length, "TP: len");
        require(peerAddrs_.length + 1 == uint256(totalParticipants_), "TP: peers");
        MY_INDEX = myIndex_;
        TOTAL_PARTICIPANTS = totalParticipants_;
        for (uint256 i; i < peerAddrs_.length; i++) {
            require(peerAddrs_[i] != address(0), "TP: peer");
            _peerAddrs.push(peerAddrs_[i]);
            _peerShards.push(peerShards_[i]);
        }
    }

    function setUserClient(address u, uint32 s) external {
        require(!userClientFixed && u != address(0), "TP: user");
        userClient = u;
        userShard = s;
        userClientFixed = true;
    }

    function _othersMask() internal view returns (uint256 m) {
        m = (uint256(1) << uint256(TOTAL_PARTICIPANTS)) - 1;
        m ^= uint256(1) << uint256(MY_INDEX);
    }

    function onPeerXcCallback(uint256, bool, bytes calldata) external virtual {
        // Executor 回调占位（peer→peer 跨分片可指向本函数）
    }

    function _broadcastPrepareOk(bytes32 txId) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes memory targetCalldata = abi.encodeWithSelector(this.peerPrepareOk.selector, txId, MY_INDEX);
        unchecked {
            _xcNonce++;
        }
        for (uint256 i; i < _peerAddrs.length; i++) {
            uint256 requestId = uint256(keccak256(abi.encodePacked(txId, MY_INDEX, i, _xcNonce, block.timestamp))) % (2 ** 128);
            bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
                selfShard,
                address(this),
                this.onPeerXcCallback.selector,
                requestId,
                _peerAddrs[i],
                targetCalldata
            );
            TwoPhaseLib.emitCrossShardRequest(_peerShards[i], TwoPhaseLib.PRECOMPILE_EXECUTOR, exec);
        }
        emit PeerPrepareOkSent(txId, MY_INDEX);
    }

    function _broadcastPrepareFailed(bytes32 txId) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes memory targetCalldata = abi.encodeWithSelector(this.peerPrepareFailed.selector, txId);
        unchecked {
            _xcNonce++;
        }
        for (uint256 i; i < _peerAddrs.length; i++) {
            uint256 requestId = uint256(keccak256(abi.encodePacked("FAIL", txId, MY_INDEX, i, _xcNonce))) % (2 ** 128);
            bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
                selfShard,
                address(this),
                this.onPeerXcCallback.selector,
                requestId,
                _peerAddrs[i],
                targetCalldata
            );
            TwoPhaseLib.emitCrossShardRequest(_peerShards[i], TwoPhaseLib.PRECOMPILE_EXECUTOR, exec);
        }
        emit PeerPrepareFailedSent(txId);
    }

    function peerPrepareOk(bytes32 txId, uint8 fromIndex) external virtual {
        require(fromIndex < TOTAL_PARTICIPANTS && fromIndex != MY_INDEX, "TP: from");
        PeerProgress storage p = _peer[txId];
        if (p.terminal) return;
        p.peerOkMask |= (uint256(1) << uint256(fromIndex));
        _tryDrive(txId);
    }

    function peerPrepareFailed(bytes32 txId) external virtual {
        PeerProgress storage p = _peer[txId];
        if (p.terminal) return;
        if (!p.localPrepared) {
            _txPrepareNacked[txId] = true;
        }
        p.globalFail = true;
        _tryDrive(txId);
    }

    function _tryDrive(bytes32 txId) internal {
        PeerProgress storage p = _peer[txId];
        if (p.terminal) return;

        if (p.globalFail) {
            if (p.localPrepared) {
                _abortLocal(txId);
            }
            p.terminal = true;
            emit TwoPCAborted(txId);
            _notifyUser(txId, false, ChainspaceReason.FOLLOWER_ABORT);
            return;
        }

        if (!p.localPrepared) return;
        uint256 need = _othersMask();
        if ((p.peerOkMask & need) != need) return;

        bool ok = _commitLocal(txId);
        p.terminal = true;
        if (ok) {
            emit TwoPCCommitted(txId);
            _notifyUser(txId, true, ChainspaceReason.OK);
        } else {
            emit TwoPCAborted(txId);
            _notifyUser(txId, false, ChainspaceReason.OTHER);
        }
    }

    /// @dev prepare 成功路径：置 localPrepared 后广播 ok 并尝试提交
    function _markPreparedAndSync(bytes32 txId) internal {
        PeerProgress storage p = _peer[txId];
        require(!p.terminal && !p.localPrepared, "TP: state");
        p.localPrepared = true;
        _broadcastPrepareOk(txId);
        _tryDrive(txId);
    }

    /// @dev prepare 锁失败：通知所有 peer，本地终态失败；`reason` 默认 `OTHER`。
    function _failPrepareAndNotifyPeers(bytes32 txId) internal {
        _failPrepareAndNotifyPeers(txId, ChainspaceReason.OTHER);
    }

    /// @dev 同 `_failPrepareAndNotifyPeers(txId)`，但显式 `reason`（如 `LOCK_CONFLICT` / `BUSINESS_RULE`）。
    function _failPrepareAndNotifyPeers(bytes32 txId, uint8 reason) internal {
        PeerProgress storage p = _peer[txId];
        if (p.terminal) return;
        _txPrepareNacked[txId] = true;
        p.globalFail = true;
        _broadcastPrepareFailed(txId);
        p.terminal = true;
        emit TwoPCAborted(txId);
        _notifyUser(txId, false, reason);
    }

    function _notifyUser(bytes32 txId, bool success, uint8 reason) internal virtual {
        if (!userClientFixed || userClient == address(0)) return;
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        unchecked {
            _xcNonce++;
        }
        uint256 requestId = uint256(keccak256(abi.encodePacked("USER", txId, MY_INDEX, _xcNonce))) % (2 ** 128);
        bytes memory targetCalldata = abi.encodeWithSelector(
            INftPurchaseChainspaceUserClient.onParticipantFinished.selector,
            txId,
            MY_INDEX,
            success,
            reason
        );
        bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
            selfShard,
            address(this),
            this.onPeerXcCallback.selector,
            requestId,
            userClient,
            targetCalldata
        );
        TwoPhaseLib.emitCrossShardRequest(userShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, exec);
    }

    function _abortLocal(bytes32 txId) internal virtual;

    function _commitLocal(bytes32 txId) internal virtual returns (bool ok);
}

interface INftPurchaseChainspaceUserClient {
    function onParticipantFinished(bytes32 txId, uint8 participantIndex, bool success, uint8 reason) external;
}
