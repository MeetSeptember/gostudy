// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowNftStore
 * @dev Sparrow 批量子交易：全局 `remainingSupply` 逐行扣减；与 `SparrowFruitStore` 同形但无 fruitType。
 *      与 `nft-joyue` / `nft-2pc` 默认常量对齐：初始 500 份、单价 100。
 */
contract SparrowNftStore {
    uint256 public constant INITIAL_REMAINING_SUPPLY = 500;
    uint256 public constant UNIT_PRICE = 100;

    uint256 public remainingSupply;

    /// @dev 全局库存批锁：0 或当前 batchId
    bytes32 public globalLockBatch;

    mapping(bytes32 => PrepareLock) public prepareLocks;

    struct PrepareLock {
        uint256 quantity;
        bool exists;
    }

    event PreparedBatchLines(bytes32 indexed batchId, bool[] lineOk);
    event Committed(bytes32 indexed batchId);
    event Aborted(bytes32 indexed batchId);

    constructor() {
        remainingSupply = INITIAL_REMAINING_SUPPLY;
    }

    function unitPrice() external pure returns (uint256) {
        return UNIT_PRICE;
    }

    function prepareBatch(bytes32 batchId, uint256[] calldata quantities)
        external
        returns (bool allOk, bool[] memory lineOk)
    {
        require(quantities.length > 0, "SNftS: empty batch");
        require(globalLockBatch == bytes32(0) || globalLockBatch == batchId, "SNftS: supply locked");
        require(!prepareLocks[batchId].exists, "SNftS: batch exists");

        if (globalLockBatch == bytes32(0)) {
            globalLockBatch = batchId;
        }

        lineOk = new bool[](quantities.length);
        uint256 rem = remainingSupply;
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

        emit PreparedBatchLines(batchId, lineOk);

        if (allOk) {
            uint256 total = remainingSupply - rem;
            remainingSupply = rem;
            prepareLocks[batchId] = PrepareLock({quantity: total, exists: true});
        }
        return (allOk, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        require(globalLockBatch == batchId, "SNftS: lock mismatch");
        if (prepareLocks[batchId].exists) {
            delete prepareLocks[batchId];
        }
        globalLockBatch = bytes32(0);
        emit Committed(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        if (globalLockBatch != batchId) {
            return;
        }
        if (prepareLocks[batchId].exists) {
            PrepareLock memory lock = prepareLocks[batchId];
            remainingSupply += lock.quantity;
            delete prepareLocks[batchId];
        }
        globalLockBatch = bytes32(0);
        emit Aborted(batchId);
    }
}
