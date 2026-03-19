// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title Wallet2PC
 * @dev 2PC 余额 Participant：prepare 锁定、commit 扣减、abort 释放
 *      合约级锁：被某 tx 访问后独占，commit/abort 前其他 tx 访问 revert
 *      与 JOYUE 的 WalletMasterV2 分离
 */
contract Wallet2PC {
    mapping(address => uint256) public balance;

    /// @dev 合约级锁：0 表示未锁定，非 0 表示被该 txId 独占
    bytes32 public lockedByTxId;

    struct PrepareLock {
        address user;
        uint256 amount;
        bool exists;
    }
    mapping(bytes32 => PrepareLock) public prepareLocks;

    event Prepared(bytes32 indexed txId, address user, uint256 amount);
    event Committed(bytes32 indexed txId);
    event Aborted(bytes32 indexed txId);

    function setBalance(address user, uint256 amount) external {
        balance[user] = amount;
    }

    /**
     * @dev Prepare: 锁定余额，先获取合约锁
     */
    function prepare(bytes32 txId, address user, uint256 amount) external returns (bool) {
        require(lockedByTxId == bytes32(0) || lockedByTxId == txId, "2PC: contract locked");
        require(!prepareLocks[txId].exists, "2PC: txId already prepared");
        require(balance[user] >= amount, "2PC: insufficient balance");

        if (lockedByTxId == bytes32(0)) {
            lockedByTxId = txId;
        }

        balance[user] -= amount;
        prepareLocks[txId] = PrepareLock({ user: user, amount: amount, exists: true });
        emit Prepared(txId, user, amount);
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
            return; // 从未 prepare，no-op（Coordinator 统一 abort 时部分 Participant 可能未 prepare）
        }
        require(lockedByTxId == txId, "2PC: lock not held by this tx");
        PrepareLock memory lock = prepareLocks[txId];
        balance[lock.user] += lock.amount;
        delete prepareLocks[txId];
        lockedByTxId = bytes32(0);
        emit Aborted(txId);
    }
}
