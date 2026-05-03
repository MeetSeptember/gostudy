// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

/**
 * @title PeerTransferCoordinator2PC
 * @dev 跨分片编排「单 Participant」转账 2PC：仅调 `PeerTransferWallet2PC` 的 `prepareTransfer` / `commit` / `abort`。
 *      与 `PeerNftPurchaseCoordinator2PC` 同形（`TwoPhaseLib` + `onPrepareResponse`），但阶段一只有 **一路** prepare。
 *
 *      事件与 `twopc-metrics` / `joyue-trigger` 对齐：`PeerTransfer2PCStarted` / `Committed` / `Aborted`。
 *      `onPrepareResponse`：Executor 的 `ok` 仅表示 **call 是否 revert**；`prepareTransfer` 另返回 `bool`，须解码 `retData`，仅 `true` 时 commit。
 *      `startTransfer(from,to,amount)` 由链下按 ERC20.csv 等喂入；无 `PeerSimulatedUsers` 白名单。
 */
contract PeerTransferCoordinator2PC {
    address public immutable wallet;
    uint32 public immutable shardWallet;

    uint256 private _nonce;
    mapping(uint256 => bytes32) private _requestToTxId;

    event PeerTransfer2PCStarted(bytes32 indexed txId, address from, address to, uint256 amount);
    event PeerTransfer2PCCommitted(bytes32 indexed txId);
    event PeerTransfer2PCAborted(bytes32 indexed txId);
    /// @dev reason: 1=Wallet 调用 revert（Executor `ok=false`）；2=`prepareTransfer` 返回 false；3=`retData` 过短无法解码为 bool
    event PeerTransfer2PCAbortReason(bytes32 indexed txId, uint8 reason, bool walletOk);

    constructor(address wallet_, uint32 shardWallet_) {
        require(wallet_ != address(0), "PT2PC_C: wallet");
        wallet = wallet_;
        shardWallet = shardWallet_;
    }

    function _nextTxId() internal returns (bytes32) {
        bytes32 txId = keccak256(abi.encodePacked(block.timestamp, msg.sender, block.number, _nonce));
        unchecked {
            _nonce++;
        }
        return txId;
    }

    function startTransfer(address from, address to, uint256 amount) public {
        require(amount > 0, "PT2PC_C: amount");
        require(from != to, "PT2PC_C: self");
        require(from != address(0) && to != address(0), "PT2PC_C: zero");

        bytes32 txId = _nextTxId();
        emit PeerTransfer2PCStarted(txId, from, to, amount);

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes memory walletCalldata =
            abi.encodeWithSignature("prepareTransfer(bytes32,address,address,uint256)", txId, from, to, amount);
        require(_emitPrepare(shardWallet, wallet, selfShard, txId, walletCalldata), "PT2PC_C: emit prepare");
    }

    function _emitPrepare(
        uint32 targetShardId,
        address targetAddr,
        uint32 sourceShardId,
        bytes32 txId,
        bytes memory targetCalldata
    ) internal returns (bool) {
        uint256 requestId = uint256(keccak256(abi.encodePacked(txId, uint8(0), block.timestamp))) % (2 ** 128);
        _requestToTxId[requestId] = txId;

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

    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata retData) external {
        bytes32 txId = _requestToTxId[requestId];
        delete _requestToTxId[requestId];
        require(txId != bytes32(0), "PT2PC_C: unknown request");

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        if (!ok) {
            emit PeerTransfer2PCAbortReason(txId, 1, false);
            _emitCommitOrAbort(txId, false, selfShard);
            emit PeerTransfer2PCAborted(txId);
            return;
        }

        if (retData.length < 32) {
            emit PeerTransfer2PCAbortReason(txId, 3, false);
            _emitCommitOrAbort(txId, false, selfShard);
            emit PeerTransfer2PCAborted(txId);
            return;
        }

        bool prepOk = abi.decode(retData, (bool));
        if (!prepOk) {
            emit PeerTransfer2PCAbortReason(txId, 2, false);
            _emitCommitOrAbort(txId, false, selfShard);
            emit PeerTransfer2PCAborted(txId);
            return;
        }

        _emitCommitOrAbort(txId, true, selfShard);
        emit PeerTransfer2PCCommitted(txId);
    }

    function _emitCommitOrAbort(bytes32 txId, bool doCommit, uint32 sourceShardId) internal {
        bytes4 sel = doCommit ? bytes4(keccak256("commit(bytes32)")) : bytes4(keccak256("abort(bytes32)"));
        bytes memory cd = abi.encodeWithSelector(sel, txId);
        uint256 baseReqId = uint256(txId) % (2 ** 120);

        bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            baseReqId,
            wallet,
            cd
        );
        TwoPhaseLib.emitCrossShardRequest(shardWallet, TwoPhaseLib.PRECOMPILE_EXECUTOR, exec);
    }

    function onCommitResponse(uint256, bool, bytes calldata) external {}
}
