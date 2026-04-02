// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowFruitStore
 * @dev Sparrow 批量子交易：按水果类型合并扣库存；逐行检查库存，全成功再写锁与扣减
 */
contract SparrowFruitStore {
    bytes32 public constant APPLE = keccak256("apple");
    bytes32 public constant BANANA = keccak256("banana");

    mapping(bytes32 => uint256) public stock;
    mapping(bytes32 => uint256) public price;
    mapping(bytes32 => uint256) public pointsPer;

    mapping(bytes32 => bytes32) public lockFruit;
    mapping(bytes32 => PrepareLock) public prepareLocks;
    mapping(bytes32 => bytes32) public prepareBatchFruit;

    struct PrepareLock {
        bytes32 fruitType;
        uint256 quantity;
        bool exists;
    }

    event PreparedBatchLines(bytes32 indexed batchId, bytes32 indexed fruitType, bool[] lineOk);
    event Committed(bytes32 indexed batchId);
    event Aborted(bytes32 indexed batchId);

    constructor() {
        stock[APPLE] = 20;
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

    /// @param quantities 本批内该水果每笔扣库数量（顺序与协调者波内该 fruitType 的行一致）；逐行扣减模拟，任一行失败则整批不写状态。
    function prepareBatch(bytes32 batchId, bytes32 fruitType, uint256[] calldata quantities)
    external
    returns (bool allOk, bool[] memory lineOk)
    {
        require(quantities.length > 0, "SparrowS: empty batch");
        require(lockFruit[fruitType] == bytes32(0) || lockFruit[fruitType] == batchId, "SparrowS: fruit locked");
        require(!prepareLocks[batchId].exists, "SparrowS: batch exists");

        if (lockFruit[fruitType] == bytes32(0)) {
            lockFruit[fruitType] = batchId;
        }
        prepareBatchFruit[batchId] = fruitType;

        lineOk = new bool[](quantities.length);
        uint256 rem = stock[fruitType];
        allOk = true;
        for (uint256 i = 0; i < quantities.length; i++) {
            uint256 q = quantities[i];
            if (rem >= q) {
                unchecked {
                    rem -= q;
                }
                lineOk[i] = true;
            } else {
                lineOk[i] = false;
                allOk = false;
            }
        }

        emit PreparedBatchLines(batchId, fruitType, lineOk);

        if (allOk) {
            uint256 total = stock[fruitType] - rem;
            stock[fruitType] = rem;
            prepareLocks[batchId] = PrepareLock({ fruitType: fruitType, quantity: total, exists: true });
        }
        return (allOk, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        bytes32 fruitType = prepareBatchFruit[batchId];
        require(fruitType != bytes32(0), "SparrowS: unknown batch");
        require(lockFruit[fruitType] == batchId, "SparrowS: lock mismatch");
        if (prepareLocks[batchId].exists) {
            delete prepareLocks[batchId];
        }
        lockFruit[fruitType] = bytes32(0);
        delete prepareBatchFruit[batchId];
        emit Committed(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        bytes32 fruitType = prepareBatchFruit[batchId];
        if (fruitType == bytes32(0)) {
            return;
        }
        require(lockFruit[fruitType] == batchId, "SparrowS: lock mismatch");
        if (prepareLocks[batchId].exists) {
            PrepareLock memory lock = prepareLocks[batchId];
            stock[lock.fruitType] += lock.quantity;
            delete prepareLocks[batchId];
        }
        lockFruit[fruitType] = bytes32(0);
        delete prepareBatchFruit[batchId];
        emit Aborted(batchId);
    }
}
