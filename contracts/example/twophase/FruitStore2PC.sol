// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title FruitStore2PC
 * @dev 2PC 库存 Participant：prepare 锁定、commit 扣减、abort 释放
 *      合约级锁：被某 tx 访问后独占，commit/abort 前其他 tx 访问 revert
 *      与 JOYUE 的 FruitShopMasterV2 分离
 */
contract FruitStore2PC {
    bytes32 public constant APPLE = keccak256("apple");
    bytes32 public constant BANANA = keccak256("banana");

    mapping(bytes32 => uint256) public stock;       // fruitType => 数量
    mapping(bytes32 => uint256) public price;       // fruitType => 单价
    mapping(bytes32 => uint256) public pointsPer;   // fruitType => 每单位积分

    /// @dev 合约级锁：0 表示未锁定，非 0 表示被该 txId 独占
    bytes32 public lockedByTxId;

    struct PrepareLock {
        bytes32 fruitType;
        uint256 quantity;
        bool exists;
    }
    mapping(bytes32 => PrepareLock) public prepareLocks;  // txId => lock

    event Prepared(bytes32 indexed txId, bytes32 fruitType, uint256 quantity);
    event Committed(bytes32 indexed txId);
    event Aborted(bytes32 indexed txId);

    constructor() {
        stock[APPLE] = 50;
        stock[BANANA] = 8;
        price[APPLE] = 20;
        price[BANANA] = 10;
        pointsPer[APPLE] = 5;
        pointsPer[BANANA] = 1;
    }

    function getPrice(bytes32 fruitType) external view returns (uint256) {
        return price[fruitType];
    }

    function getPointsPer(bytes32 fruitType) external view returns (uint256) {
        return pointsPer[fruitType];
    }

    /**
     * @dev Prepare: 锁定库存，先获取合约锁
     */
    function prepare(bytes32 txId, bytes32 fruitType, uint256 quantity) external returns (bool) {
        require(lockedByTxId == bytes32(0) || lockedByTxId == txId, "2PC: contract locked");
        require(!prepareLocks[txId].exists, "2PC: txId already prepared");
        require(stock[fruitType] >= quantity, "2PC: insufficient stock");

        if (lockedByTxId == bytes32(0)) {
            lockedByTxId = txId;
        }

        stock[fruitType] -= quantity;
        prepareLocks[txId] = PrepareLock({ fruitType: fruitType, quantity: quantity, exists: true });
        emit Prepared(txId, fruitType, quantity);
        return true;
    }

    /**
     * @dev Commit: 确认扣减，清除锁定（库存已在 prepare 时扣减）
     */
    function commit(bytes32 txId) external {
        require(lockedByTxId == txId, "2PC: lock not held by this tx");
        PrepareLock memory lock = prepareLocks[txId];
        require(lock.exists, "2PC: no prepare for txId");
        delete prepareLocks[txId];
        lockedByTxId = bytes32(0);
        emit Committed(txId);
    }

    /**
     * @dev Abort: 释放锁定，恢复库存
     */
    function abort(bytes32 txId) external {
        if (!prepareLocks[txId].exists) {
            return; // 从未 prepare，no-op
        }
        require(lockedByTxId == txId, "2PC: lock not held by this tx");
        PrepareLock memory lock = prepareLocks[txId];
        stock[lock.fruitType] += lock.quantity;
        delete prepareLocks[txId];
        lockedByTxId = bytes32(0);
        emit Aborted(txId);
    }
}
