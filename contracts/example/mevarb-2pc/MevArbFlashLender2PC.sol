// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbFlashLender2PC
 * @dev 2PC 参与者：`prepareLend` 从 `available` 划出 `borrowA`；`commit` 视为还贷加回；`abort` 全额退回。
 *      **账户锁** `accountLock[user]`：同一用户同时仅一笔在途 flash 2PC；不同用户可并行占用 `available`（与 `AmmWalletA2PC` 一致）。
 *      `setAvailable` 仅在无在途 prepare 时允许（`pendingLendCount == 0`）。
 */
contract MevArbFlashLender2PC {
    uint256 public constant INITIAL_AVAILABLE = 10 ** 30;

    uint256 public available;
    /// @dev 当前处于 prepare 未 commit/abort 的笔数，供 `setAvailable` 与运维判断。
    uint256 public pendingLendCount;

    mapping(address => bytes32) public accountLock;

    struct Pending {
        address user;
        uint256 borrowA;
        bool exists;
    }

    mapping(bytes32 => Pending) public pendingLends;

    event FlashPreparedLend(bytes32 indexed txId, address indexed user, uint256 borrowA);
    event FlashCommitted(bytes32 indexed txId);
    event FlashAborted(bytes32 indexed txId);

    constructor() {
        available = INITIAL_AVAILABLE;
    }

    function setAvailable(uint256 v) external {
        require(pendingLendCount == 0, "flash: pending");
        available = v;
    }

    function _requireAccount(bytes32 txId, address user) internal view {
        require(user != address(0), "flash: user");
        require(accountLock[user] == bytes32(0) || accountLock[user] == txId, "flash: account locked");
    }

    function prepareLend(bytes32 txId, address user, uint256 borrowA) external returns (bool) {
        require(borrowA > 0, "flash: borrow");
        require(available >= borrowA, "flash: liquidity");
        require(!pendingLends[txId].exists, "flash: prepared");
        _requireAccount(txId, user);

        available -= borrowA;
        accountLock[user] = txId;
        pendingLends[txId] = Pending({user: user, borrowA: borrowA, exists: true});
        unchecked {
            pendingLendCount++;
        }
        emit FlashPreparedLend(txId, user, borrowA);
        return true;
    }

    function commit(bytes32 txId) external {
        Pending memory p = pendingLends[txId];
        require(p.exists, "flash: no prepare");
        require(accountLock[p.user] == txId, "flash: lock");
        available += p.borrowA;
        delete pendingLends[txId];
        accountLock[p.user] = bytes32(0);
        unchecked {
            pendingLendCount--;
        }
        emit FlashCommitted(txId);
    }

    function abort(bytes32 txId) external {
        Pending memory p = pendingLends[txId];
        if (!p.exists) {
            return;
        }
        if (accountLock[p.user] != txId) {
            return;
        }
        available += p.borrowA;
        delete pendingLends[txId];
        accountLock[p.user] = bytes32(0);
        unchecked {
            pendingLendCount--;
        }
        emit FlashAborted(txId);
    }
}
