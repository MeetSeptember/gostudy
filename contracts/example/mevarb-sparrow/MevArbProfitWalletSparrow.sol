// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbProfitWalletSparrow
 * @dev Sparrow 批：`prepareLockBatch`（一次传入锁定 + 承诺入账金额，**commit 前**不改 `profitBalance`）/ `commitBatch` / `abortBatch`。
 *      可选 `setBalance` / `setBalances`（与 erc20 bootstrap 同 ABI）；`setProfit` 为 `setBalance` 别名。
 */
contract MevArbProfitWalletSparrow {
    mapping(address => uint256) public profitBalance;
    mapping(address => bytes32) public accountLock;
    mapping(bytes32 => PrepareCredit) private _prepares;
    mapping(bytes32 => bytes32[]) private _batchTxIds;

    struct PrepareCredit {
        address user;
        uint256 amount;
        bool exists;
    }

    event ProfitBatchCommitted(bytes32 indexed batchId);
    event ProfitBatchAborted(bytes32 indexed batchId);

    constructor() {}

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "profit: zero");
        require(accountLock[user] == bytes32(0), "profit: locked");
        profitBalance[user] = v;
    }

    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "profit: zero");
            require(accountLock[u] == bytes32(0), "profit: locked");
            profitBalance[u] = v;
        }
    }

    /// @dev 与历史脚本兼容；语义同 `setBalance`。
    function setProfit(address user, uint256 v) external {
        require(user != address(0), "profit: zero");
        require(accountLock[user] == bytes32(0), "profit: locked");
        profitBalance[user] = v;
    }

    /**
     * @notice 锁账户并写入每条 pending 的入账金额；**仅** `commitBatch` 时 `profitBalance += amount`；`abortBatch` 清 pending，不动余额。
     */
    function prepareLockBatch(
        bytes32 batchId,
        bytes32[] calldata txIds,
        address[] calldata users,
        uint256[] calldata amounts
    ) external returns (bool allOk, bool[] memory lineOk) {
        uint256 n = txIds.length;
        require(n > 0, "profit: empty");
        require(users.length == n && amounts.length == n, "profit: len");
        require(_batchTxIds[batchId].length == 0, "profit: batch exists");

        for (uint256 a = 0; a < n; a++) {
            for (uint256 b = a + 1; b < n; b++) {
                require(txIds[a] != txIds[b], "profit: dup tx");
            }
        }

        lineOk = new bool[](n);
        allOk = true;

        for (uint256 i = 0; i < n; i++) {
            bytes32 tid = txIds[i];
            address u = users[i];
            uint256 amt = amounts[i];
            if (amt == 0 || u == address(0) || _prepares[tid].exists) {
                lineOk[i] = false;
                allOk = false;
                continue;
            }
            if (accountLock[u] != bytes32(0) && accountLock[u] != batchId) {
                lineOk[i] = false;
                allOk = false;
                continue;
            }
            lineOk[i] = true;
        }

        if (!allOk) {
            return (false, lineOk);
        }

        for (uint256 j = 0; j < n; j++) {
            address u = users[j];
            bytes32 tid = txIds[j];
            uint256 amt = amounts[j];
            accountLock[u] = batchId;
            _prepares[tid] = PrepareCredit({user: u, amount: amt, exists: true});
            _batchTxIds[batchId].push(tid);
        }

        return (true, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        bytes32[] storage tids = _batchTxIds[batchId];
        uint256 n = tids.length;
        require(n > 0, "profit: unknown batch");

        address[] memory usersMem = new address[](n);
        for (uint256 i = 0; i < n; i++) {
            bytes32 tid = tids[i];
            PrepareCredit memory p = _prepares[tid];
            require(p.exists && p.amount > 0, "profit: not prepared");
            require(accountLock[p.user] == batchId, "profit: lock");
            usersMem[i] = p.user;
            profitBalance[p.user] += p.amount;
            delete _prepares[tid];
        }
        for (uint256 j = 0; j < n; j++) {
            accountLock[usersMem[j]] = bytes32(0);
        }
        delete _batchTxIds[batchId];
        emit ProfitBatchCommitted(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        bytes32[] storage tids = _batchTxIds[batchId];
        uint256 n = tids.length;
        if (n == 0) {
            return;
        }
        address[] memory usersMem = new address[](n);
        uint256 ulen = 0;
        for (uint256 i = 0; i < n; i++) {
            bytes32 tid = tids[i];
            PrepareCredit memory p = _prepares[tid];
            if (!p.exists || accountLock[p.user] != batchId) {
                continue;
            }
            usersMem[ulen] = p.user;
            unchecked {
                ulen++;
            }
            delete _prepares[tid];
        }
        for (uint256 j = 0; j < ulen; j++) {
            accountLock[usersMem[j]] = bytes32(0);
        }
        delete _batchTxIds[batchId];
        emit ProfitBatchAborted(batchId);
    }
}
