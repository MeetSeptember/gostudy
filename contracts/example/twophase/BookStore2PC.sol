// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title BookStore2PC
 * @dev 2PC 书籍库存 Participant：prepare 锁定、commit 扣减、abort 释放
 *      合约级锁：被某 tx 访问后独占，commit/abort 前其他 tx 访问 revert
 *      与 FruitStore2PC 结构相同，共享 Wallet/Points 时可触发锁竞争
 */
contract BookStore2PC {
    bytes32 public constant MATH = keccak256("math");
    bytes32 public constant HISTORY = keccak256("history");

    mapping(bytes32 => uint256) public stock;
    mapping(bytes32 => uint256) public price;
    mapping(bytes32 => uint256) public pointsPer;

    /// @dev 合约级锁：0 表示未锁定，非 0 表示被该 txId 独占
    bytes32 public lockedByTxId;

    struct PrepareLock {
        bytes32 bookType;
        uint256 quantity;
        bool exists;
    }
    mapping(bytes32 => PrepareLock) public prepareLocks;

    event Prepared(bytes32 indexed txId, bytes32 bookType, uint256 quantity);
    event Committed(bytes32 indexed txId);
    event Aborted(bytes32 indexed txId);

    constructor() {
        stock[MATH] = 50;
        stock[HISTORY] = 30;
        price[MATH] = 30;
        price[HISTORY] = 25;
        pointsPer[MATH] = 10;
        pointsPer[HISTORY] = 8;
    }

    function getPrice(bytes32 bookType) external view returns (uint256) {
        return price[bookType];
    }

    function getPointsPer(bytes32 bookType) external view returns (uint256) {
        return pointsPer[bookType];
    }

    function prepare(bytes32 txId, bytes32 bookType, uint256 quantity) external returns (bool) {
        require(lockedByTxId == bytes32(0) || lockedByTxId == txId, "2PC: contract locked");
        require(!prepareLocks[txId].exists, "2PC: txId already prepared");
        require(stock[bookType] >= quantity, "2PC: insufficient stock");

        if (lockedByTxId == bytes32(0)) {
            lockedByTxId = txId;
        }

        stock[bookType] -= quantity;
        prepareLocks[txId] = PrepareLock({ bookType: bookType, quantity: quantity, exists: true });
        emit Prepared(txId, bookType, quantity);
        return true;
    }

    function commit(bytes32 txId) external {
        require(lockedByTxId == txId, "2PC: lock not held by this tx");
        PrepareLock memory lock = prepareLocks[txId];
        require(lock.exists, "2PC: no prepare for txId");
        delete prepareLocks[txId];
        lockedByTxId = bytes32(0);
        emit Committed(txId);
    }

    function abort(bytes32 txId) external {
        if (!prepareLocks[txId].exists) {
            return; // 从未 prepare，no-op
        }
        require(lockedByTxId == txId, "2PC: lock not held by this tx");
        PrepareLock memory lock = prepareLocks[txId];
        stock[lock.bookType] += lock.quantity;
        delete prepareLocks[txId];
        lockedByTxId = bytes32(0);
        emit Aborted(txId);
    }
}
