// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "./ChainspaceWalletSimulator.sol";
import "../twophase/TwoPhaseLib.sol";
import "../chainspace-utils/ChainspaceReason.sol";
import "../chainspace-utils/ChainspaceRevert.sol";

/**
 * @title ChainspaceUserClient
 * @dev `transferLine(from,to,amount)`：显式 from/to + 内部随机选 `from` 的 objectId。`transferLine(uint256)` 已弃用（无链上固定地址表时无法安全随机对），请用显式路径或链下随机地址对。
 *      提交 `verifyAndCommitFor` **始终**经 `TwoPhaseLib.emitCrossShardRequest`（即使 `walletShardId` 与当前分片相同，也由预编译路径投递，不直接 `CALL` Simulator）。
 *      编排语义：每笔业务一个 `intentId`；各 leg 经 Executor 回调 `onWalletVerifyCallback`；**仅当所有 leg 的回调均到达且均为 `ok` 时**发出 `ChainspaceIntentFinalized(intentId, true, OK)`，任一 `ok=false` 则最终为 `false` 且 `finalReason` 由回调 `data` 中 `ChainspaceRevert(uint8)` 的 ABI revert 数据解码（与 `ChainspaceReason` 对齐），否则为 `OTHER`。
 *      当前实现为 **1 个 wallet leg**；后续可在同一 `intentId` 下追加多笔 `emitCrossShardRequest` 并令 `pendingLegs` 与注册次数一致。
 */
contract ChainspaceUserClient {
    ChainspaceWalletSimulator public immutable shard;
    /// @dev Simulator 所在分片 ID（跨分片请求目标分片）
    uint32 public immutable walletShardId;

    uint256 private _xcReqNonce;
    uint256 private _intentNonce;

    struct IntentState {
        uint256 pendingLegs;
        bool anyFailure;
        uint8 aggFailReason;
    }

    /// @dev 尚未收到足够回调的 intent
    mapping(bytes32 => IntentState) private _intents;
    /// @dev 单次跨分片请求的 requestId → 所属 intent（回调时扣减 pendingLegs）
    mapping(uint256 => bytes32) private _requestToIntent;

    constructor(address shardWallet, uint32 walletShardId_) {
        require(shardWallet != address(0), "CUC: shard");
        shard = ChainspaceWalletSimulator(shardWallet);
        walletShardId = walletShardId_;
    }

    /// @dev 从 `from` 名下伪随机起点环形查找第一张面值 >= minValue 的 ACTIVE note
    function _pickActiveNoteAtLeast(address from, uint256 minValue, uint256 salt)
        private
        view
        returns (bytes32 inputId, uint256 value, bool ok)
    {
        uint256 n = shard.countObjectsForOwner(from);
        if (n == 0 || minValue == 0) return (bytes32(0), 0, false);
        uint256 start = uint256(keccak256(abi.encodePacked(salt, from, block.prevrandao, block.number, address(this)))) % n;
        for (uint256 t = 0; t < n; t++) {
            bytes32 cand = shard.objectIdForOwnerAt(from, (start + t) % n);
            (address oowner, uint256 val, ChainspaceWalletSimulator.Status st) = shard.objects(cand);
            if (st == ChainspaceWalletSimulator.Status.ACTIVE && oowner == from && val >= minValue) {
                return (cand, val, true);
            }
        }
        return (bytes32(0), 0, false);
    }

    function _notePickSalt(address from, address to, uint256 amount) private view returns (uint256) {
        return uint256(keccak256(abi.encodePacked(from, to, amount, block.number, msg.sender, address(this))));
    }

    /// @dev 与 SparrowTransferWallet.transferLine 形参一致；内部随机选 `from` 的 objectId
    function transferLine(address from, address to, uint256 amount) external {
        require(amount > 0, "CUC: amount");
        require(from != address(0) && to != address(0), "CUC: zero");
        require(from != to, "CUC: self");
        (bytes32 inputId, uint256 totalValue, bool hit) =
            _pickActiveNoteAtLeast(from, amount, _notePickSalt(from, to, amount));
        require(hit, "CUC: no note");
        _commitVerify(from, inputId, to, amount, totalValue);
    }

    /// @dev 已弃用：请使用 `transferLine(address,address,uint256)` 或链下随机 from/to 后调显式接口。
    function transferLine(uint256) external pure {
        revert("CUC: use transferLine(from,to,amount)");
    }

    /// @dev 业务开始：`intentId` 供 joyue-trigger / 与 `ChainspaceIntentFinalized` 对齐的 tx_id
    event ChainspaceIntentStarted(
        bytes32 indexed intentId,
        uint8 totalLegs,
        bytes32 walletInputId,
        address from,
        address to,
        uint256 transferAmount
    );

    /// @dev 全部 leg 回调处理完毕后发出；success 为 true 当且仅当每笔回调 `ok` 均为 true；`finalReason` 与 `ChainspaceReason` 一致。
    event ChainspaceIntentFinalized(bytes32 indexed intentId, bool success, uint8 finalReason);

    event WalletVerifyCrossShardResult(uint256 indexed requestId, bool ok);

    function _nextIntentId() private returns (bytes32) {
        unchecked {
            _intentNonce++;
        }
        return keccak256(abi.encodePacked(block.chainid, address(this), _intentNonce, msg.sender, block.number));
    }

    function _commitVerify(address from, bytes32 inputId, address to, uint256 transferAmount, uint256 totalValue) private {
        uint256 changeAmount = totalValue - transferAmount;

        uint8 totalLegs = 1;
        bytes32 intentId = _nextIntentId();
        _intents[intentId] = IntentState({pendingLegs: uint256(totalLegs), anyFailure: false, aggFailReason: 0});

        emit ChainspaceIntentStarted(intentId, totalLegs, inputId, from, to, transferAmount);

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        unchecked {
            _xcReqNonce++;
        }
        uint256 requestId = uint256(keccak256(abi.encodePacked(from, inputId, to, _xcReqNonce, block.timestamp))) % (2 ** 128);
        _requestToIntent[requestId] = intentId;

        bytes memory targetCalldata = abi.encodeWithSelector(
            ChainspaceWalletSimulator.verifyAndCommitFor.selector,
            from,
            inputId,
            to,
            transferAmount,
            changeAmount
        );

        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            selfShard,
            address(this),
            this.onWalletVerifyCallback.selector,
            requestId,
            address(shard),
            targetCalldata
        );
        require(
            TwoPhaseLib.emitCrossShardRequest(walletShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata),
            "CUC: emit xc"
        );
    }

    /// @dev 单 leg 失败时 `data` 为 `revert ChainspaceRevert(uint8)` 的 `returnData`（Executor 透传）；成功路径不读 `data`。
    function onWalletVerifyCallback(uint256 requestId, bool ok, bytes calldata data) external {
        bytes32 intentId = _requestToIntent[requestId];
        if (intentId == bytes32(0)) {
            emit WalletVerifyCrossShardResult(requestId, ok);
            return;
        }
        delete _requestToIntent[requestId];

        IntentState storage st = _intents[intentId];
        require(st.pendingLegs != 0, "CUC: unknown intent");
        if (!ok) {
            st.anyFailure = true;
            st.aggFailReason = _mergeAggFailReason(st.aggFailReason, _decodeOptionalFailReason(data));
        }
        unchecked {
            st.pendingLegs--;
        }
        if (st.pendingLegs == 0) {
            bool success = !st.anyFailure;
            uint8 fr = success
                ? ChainspaceReason.OK
                : (st.aggFailReason == 0 ? ChainspaceReason.OTHER : st.aggFailReason);
            emit ChainspaceIntentFinalized(intentId, success, fr);
            delete _intents[intentId];
        }
    }

    /// @dev `data` 为 Executor 传入的 `returnData`（自定义 error 的 ABI：`selector || abi.encode(uint256(uint8))`）。
    ///      须用 `data[i:j]` 访问 payload：勿用 `calldataload(data.offset)`，在部分 ABI 布局下 `data.offset` 指向长度字会导致 selector 读错，锁冲突恒落为 `OTHER`。
    function _decodeOptionalFailReason(bytes calldata data) private pure returns (uint8) {
        if (data.length < 36) {
            return ChainspaceReason.OTHER;
        }
        bytes4 sel = bytes4(bytes32(data[0:32]));
        if (sel != ChainspaceRevert.selector) {
            return ChainspaceReason.OTHER;
        }
        uint256 r = uint256(bytes32(data[4:36]));
        if (r < 256) {
            return uint8(r);
        }
        return ChainspaceReason.OTHER;
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
