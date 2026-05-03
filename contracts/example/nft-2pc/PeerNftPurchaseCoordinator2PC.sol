// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

/**
 * @title PeerNftPurchaseCoordinator2PC
 * @dev 跨分片 NFT「份数」购买严格 2PC（NftStore2PC + NftWallet2PC），编排方式对齐 `PeerAmmSwapCoordinator2PC`：
 *
 *      阶段一（并行 prepare）：Store.prepare(txId, quantity)、Wallet.prepareDebit(txId, buyer, totalCost)。
 *      全成则阶段二对两端 `commit`，否则 `abort`。
 *
 *      `unitPrice` 与 Store 部署常量一致（constructor 传入），用于计算 totalCost = quantity * unitPrice。
 */
contract PeerNftPurchaseCoordinator2PC {
    address public immutable wallet;
    address public immutable store;
    uint32 public immutable shardWallet;
    uint32 public immutable shardStore;
    uint256 public immutable unitPrice;

    uint256 private _nonce;
    mapping(uint256 => bytes32) private _requestToTxId;
    mapping(uint256 => uint8) private _requestToParticipant;

    /// @dev 阶段一：0=store, 1=wallet debit
    mapping(bytes32 => bool[2]) private _phase1Votes;
    mapping(bytes32 => uint8) private _phase1Count;

    event PeerNftPurchase2PCStarted(bytes32 indexed txId, address indexed buyer, uint256 quantity, uint256 totalCost);
    event PeerNftPurchase2PCCommitted(bytes32 indexed txId);
    event PeerNftPurchase2PCAborted(bytes32 indexed txId);
    /// @dev reason: 1=阶段一未全成
    event PeerNftPurchase2PCAbortReason(bytes32 indexed txId, uint8 reason, bool storeOk, bool walletOk);

    constructor(address wallet_, address store_, uint32 shardWallet_, uint32 shardStore_, uint256 unitPrice_) {
        require(wallet_ != address(0) && store_ != address(0), "NFT2PC_C: addr");
        require(wallet_ != store_, "NFT2PC_C: same contract");
        require(unitPrice_ > 0, "NFT2PC_C: unitPrice");
        wallet = wallet_;
        store = store_;
        shardWallet = shardWallet_;
        shardStore = shardStore_;
        unitPrice = unitPrice_;
    }

    function _nextTxId() internal returns (bytes32) {
        bytes32 txId = keccak256(abi.encodePacked(block.timestamp, msg.sender, block.number, _nonce));
        _nonce++;
        return txId;
    }

    function startPurchase(address buyer, uint256 quantity) public {
        require(buyer != address(0), "NFT2PC_C: buyer");
        require(quantity > 0, "NFT2PC_C: quantity");

        uint256 totalCost = quantity * unitPrice;
        require(totalCost / quantity == unitPrice, "NFT2PC_C: overflow");

        bytes32 txId = _nextTxId();

        emit PeerNftPurchase2PCStarted(txId, buyer, quantity, totalCost);

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        bytes memory storeCalldata = abi.encodeWithSignature("prepare(bytes32,uint256)", txId, quantity);
        require(_emitPrepare(shardStore, store, selfShard, txId, 0, storeCalldata), "NFT2PC_C: emit store prepare");

        bytes memory walletCalldata =
            abi.encodeWithSignature("prepareDebit(bytes32,address,uint256)", txId, buyer, totalCost);
        require(_emitPrepare(shardWallet, wallet, selfShard, txId, 1, walletCalldata), "NFT2PC_C: emit wallet prepare");
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

    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata) external {
        bytes32 txId = _requestToTxId[requestId];
        uint8 idx = _requestToParticipant[requestId];
        delete _requestToTxId[requestId];
        delete _requestToParticipant[requestId];

        require(txId != bytes32(0), "NFT2PC_C: unknown request");
        require(idx <= 1, "NFT2PC_C: idx");

        _phase1Votes[txId][idx] = ok;
        _phase1Count[txId]++;

        if (_phase1Count[txId] != 2) {
            return;
        }

        bool sOk = _phase1Votes[txId][0];
        bool wOk = _phase1Votes[txId][1];
        bool allOk = sOk && wOk;
        delete _phase1Votes[txId];
        delete _phase1Count[txId];

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        if (!allOk) {
            emit PeerNftPurchase2PCAbortReason(txId, 1, sOk, wOk);
            _emitCommitOrAbort2(txId, false, selfShard);
            emit PeerNftPurchase2PCAborted(txId);
            return;
        }

        _emitCommitOrAbort2(txId, true, selfShard);
        emit PeerNftPurchase2PCCommitted(txId);
    }

    function _emitCommitOrAbort2(bytes32 txId, bool doCommit, uint32 sourceShardId) internal {
        bytes4 sel = doCommit ? bytes4(keccak256("commit(bytes32)")) : bytes4(keccak256("abort(bytes32)"));
        bytes memory calldataS = abi.encodeWithSelector(sel, txId);
        bytes memory calldataW = abi.encodeWithSelector(sel, txId);

        uint256 baseReqId = uint256(txId) % (2**120);

        bytes memory execS = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            baseReqId,
            store,
            calldataS
        );
        bytes memory execW = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            baseReqId + 1,
            wallet,
            calldataW
        );

        TwoPhaseLib.emitCrossShardRequest(shardStore, TwoPhaseLib.PRECOMPILE_EXECUTOR, execS);
        TwoPhaseLib.emitCrossShardRequest(shardWallet, TwoPhaseLib.PRECOMPILE_EXECUTOR, execW);
    }

    function onCommitResponse(uint256, bool, bytes calldata) external {}
}
