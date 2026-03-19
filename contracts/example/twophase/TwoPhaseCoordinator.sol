// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "./TwoPhaseLib.sol";

/**
 * @title TwoPhaseCoordinator
 * @dev 2PC 协调者：buyFruit + buyBook，共享 Wallet/Points，可触发锁竞争
 *
 * 部署要求：Coordinator 与 FruitStore、BookStore 需在同一分片（shard 0）
 */
contract TwoPhaseCoordinator {
    address public fruitStore;
    address public bookStore;
    address public wallet;
    address public points;
    uint32 public fruitStoreShardId;
    uint32 public bookStoreShardId;
    uint32 public walletShardId;
    uint32 public pointsShardId;

    uint256 private _requestNonce;
    mapping(uint256 => bytes32) private _requestToTxId;
    mapping(uint256 => uint8) private _requestToParticipant;
    mapping(bytes32 => bool[3]) private _prepareVotes;
    mapping(bytes32 => uint8) private _prepareCount;
    mapping(bytes32 => address) private _storeForTx;   // txId => 当前交易的 Store（Fruit 或 Book）
    mapping(bytes32 => uint32) private _storeShardForTx;

    event TwoPCStarted(bytes32 indexed txId, bytes32 itemType, uint256 quantity, address buyer);
    event TwoPCCommitted(bytes32 indexed txId);
    event TwoPCAborted(bytes32 indexed txId);

    constructor(
        address _fruitStore,
        address _bookStore,
        address _wallet,
        address _points,
        uint32 _fruitStoreShardId,
        uint32 _bookStoreShardId,
        uint32 _walletShardId,
        uint32 _pointsShardId
    ) {
        fruitStore = _fruitStore;
        bookStore = _bookStore;
        wallet = _wallet;
        points = _points;
        fruitStoreShardId = _fruitStoreShardId;
        bookStoreShardId = _bookStoreShardId;
        walletShardId = _walletShardId;
        pointsShardId = _pointsShardId;
    }

    function buyFruit(string memory fruitName, uint256 quantity) external {
        buyFruit(keccak256(bytes(fruitName)), quantity);
    }

    function buyFruit(bytes32 fruitType, uint256 quantity) public {
        bytes32 txId = _nextTxId();
        _storeForTx[txId] = fruitStore;
        _storeShardForTx[txId] = fruitStoreShardId;

        emit TwoPCStarted(txId, fruitType, quantity, msg.sender);

        uint256 price = _getFruitPrice(fruitType) * quantity;
        uint256 reward = _getFruitPointsPer(fruitType) * quantity;

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes memory storeCalldata = abi.encodeWithSignature("prepare(bytes32,bytes32,uint256)", txId, fruitType, quantity);
        _emitPrepare(fruitStoreShardId, fruitStore, selfShard, txId, 0, storeCalldata);
        _emitPrepare(walletShardId, wallet, selfShard, txId, 1, abi.encodeWithSignature("prepare(bytes32,address,uint256)", txId, msg.sender, price));
        _emitPrepare(pointsShardId, points, selfShard, txId, 2, abi.encodeWithSignature("prepare(bytes32,address,uint256)", txId, msg.sender, reward));
    }

    function buyBook(string memory bookName, uint256 quantity) external {
        buyBook(keccak256(bytes(bookName)), quantity);
    }

    function buyBook(bytes32 bookType, uint256 quantity) public {
        bytes32 txId = _nextTxId();
        _storeForTx[txId] = bookStore;
        _storeShardForTx[txId] = bookStoreShardId;

        emit TwoPCStarted(txId, bookType, quantity, msg.sender);

        uint256 price = _getBookPrice(bookType) * quantity;
        uint256 reward = _getBookPointsPer(bookType) * quantity;

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes memory storeCalldata = abi.encodeWithSignature("prepare(bytes32,bytes32,uint256)", txId, bookType, quantity);
        _emitPrepare(bookStoreShardId, bookStore, selfShard, txId, 0, storeCalldata);
        _emitPrepare(walletShardId, wallet, selfShard, txId, 1, abi.encodeWithSignature("prepare(bytes32,address,uint256)", txId, msg.sender, price));
        _emitPrepare(pointsShardId, points, selfShard, txId, 2, abi.encodeWithSignature("prepare(bytes32,address,uint256)", txId, msg.sender, reward));
    }

    function _nextTxId() internal returns (bytes32) {
        bytes32 txId = keccak256(abi.encodePacked(block.timestamp, msg.sender, block.number, _requestNonce));
        _requestNonce++;
        return txId;
    }

    function _getFruitPrice(bytes32 fruitType) internal view returns (uint256) {
        (bool ok, bytes memory ret) = fruitStore.staticcall(abi.encodeWithSignature("getPrice(bytes32)", fruitType));
        require(ok && ret.length >= 32, "2PC: getFruitPrice failed");
        return abi.decode(ret, (uint256));
    }

    function _getFruitPointsPer(bytes32 fruitType) internal view returns (uint256) {
        (bool ok, bytes memory ret) = fruitStore.staticcall(abi.encodeWithSignature("getPointsPer(bytes32)", fruitType));
        require(ok && ret.length >= 32, "2PC: getFruitPointsPer failed");
        return abi.decode(ret, (uint256));
    }

    function _getBookPrice(bytes32 bookType) internal view returns (uint256) {
        (bool ok, bytes memory ret) = bookStore.staticcall(abi.encodeWithSignature("getPrice(bytes32)", bookType));
        require(ok && ret.length >= 32, "2PC: getBookPrice failed");
        return abi.decode(ret, (uint256));
    }

    function _getBookPointsPer(bytes32 bookType) internal view returns (uint256) {
        (bool ok, bytes memory ret) = bookStore.staticcall(abi.encodeWithSignature("getPointsPer(bytes32)", bookType));
        require(ok && ret.length >= 32, "2PC: getBookPointsPer failed");
        return abi.decode(ret, (uint256));
    }

    function _emitPrepare(
        uint32 targetShardId,
        address targetAddr,
        uint32 sourceShardId,
        bytes32 txId,
        uint8 participantIndex,
        bytes memory targetCalldata
    ) internal {
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

        require(
            TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata),
            "2PC: emit prepare failed"
        );
    }

    /**
     * @dev Executor 回调：prepare 阶段响应（格式必须匹配 Executor 的 callbackSelector(requestId, ok, returnData)）
     */
    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata) external {
        bytes32 txId = _requestToTxId[requestId];
        uint8 idx = _requestToParticipant[requestId];
        delete _requestToTxId[requestId];
        delete _requestToParticipant[requestId];

        require(txId != bytes32(0), "2PC: unknown requestId");

        _prepareVotes[txId][idx] = ok;
        _prepareCount[txId]++;

        if (_prepareCount[txId] == 3) {
            _finalize2PC(txId);
        }
    }

    function _finalize2PC(bytes32 txId) internal {
        bool allOk = _prepareVotes[txId][0] && _prepareVotes[txId][1] && _prepareVotes[txId][2];
        delete _prepareVotes[txId];
        delete _prepareCount[txId];

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        if (allOk) {
            _emitCommitOrAbort(txId, true, selfShard);
            emit TwoPCCommitted(txId);
        } else {
            _emitCommitOrAbort(txId, false, selfShard);
            emit TwoPCAborted(txId);
        }
    }

    function _emitCommitOrAbort(bytes32 txId, bool doCommit, uint32 sourceShardId) internal {
        address store = _storeForTx[txId];
        uint32 storeShard = _storeShardForTx[txId];
        delete _storeForTx[txId];
        delete _storeShardForTx[txId];

        bytes4 sel = doCommit ? bytes4(keccak256("commit(bytes32)")) : bytes4(keccak256("abort(bytes32)"));
        bytes memory calldataStore = abi.encodeWithSelector(sel, txId);
        bytes memory calldataW = abi.encodeWithSelector(sel, txId);
        bytes memory calldataP = abi.encodeWithSelector(sel, txId);

        uint256 baseReqId = uint256(txId) % (2**120);
        bytes memory execStore = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId, address(this), this.onCommitResponse.selector, baseReqId,
            store, calldataStore
        );
        bytes memory execW = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId, address(this), this.onCommitResponse.selector, baseReqId + 1,
            wallet, calldataW
        );
        bytes memory execP = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId, address(this), this.onCommitResponse.selector, baseReqId + 2,
            points, calldataP
        );

        TwoPhaseLib.emitCrossShardRequest(storeShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, execStore);
        TwoPhaseLib.emitCrossShardRequest(walletShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, execW);
        TwoPhaseLib.emitCrossShardRequest(pointsShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, execP);
    }

    /// @dev Executor 回调：commit/abort 完成（无操作，仅满足 Executor 回调格式）
    function onCommitResponse(uint256, bool, bytes calldata) external {}
}
