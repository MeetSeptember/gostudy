// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title NftStore2PC
 * @dev 2PC 全局「份数」库存 Participant：prepare 扣减 remainingSupply、commit 确认、abort 回滚。
 *      合约级锁 `lockedByTxId` 与 `FruitStore2PC` / `AmmPool2PC` 一致；与 JOYUE `NftJoyueSalesMasterV2` 语义对照。
 */
contract NftStore2PC {
    uint256 public constant INITIAL_REMAINING_SUPPLY = 500;
    uint256 public constant INITIAL_UNIT_PRICE = 100;

    uint256 public remainingSupply;
    uint256 public unitPrice;

    /// @dev 合约级锁：0 表示未锁定，非 0 表示被该 txId 独占
    bytes32 public lockedByTxId;

    struct PrepareLock {
        uint256 quantity;
        bool exists;
    }
    mapping(bytes32 => PrepareLock) public prepareLocks;

    event Prepared(bytes32 indexed txId, uint256 quantity);
    event Committed(bytes32 indexed txId);
    event Aborted(bytes32 indexed txId);

    constructor() {
        remainingSupply = INITIAL_REMAINING_SUPPLY;
        unitPrice = INITIAL_UNIT_PRICE;
    }

    /**
     * @dev Prepare：占合约锁并扣减库存（与 FruitStore2PC 一致）
     */
    function prepare(bytes32 txId, uint256 quantity) external returns (bool) {
        require(quantity > 0, "2PC: quantity");
        require(lockedByTxId == bytes32(0) || lockedByTxId == txId, "2PC: contract locked");
        require(!prepareLocks[txId].exists, "2PC: txId already prepared");
        require(remainingSupply >= quantity, "2PC: insufficient stock");

        if (lockedByTxId == bytes32(0)) {
            lockedByTxId = txId;
        }

        remainingSupply -= quantity;
        prepareLocks[txId] = PrepareLock({quantity: quantity, exists: true});
        emit Prepared(txId, quantity);
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
            return;
        }
        require(lockedByTxId == txId, "2PC: lock not held by this tx");
        PrepareLock memory lock = prepareLocks[txId];
        remainingSupply += lock.quantity;
        delete prepareLocks[txId];
        lockedByTxId = bytes32(0);
        emit Aborted(txId);
    }
}
