// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowAmmWalletA
 * @dev Sparrow AMM 付币侧（分片 A）：仅 `prepareBatch` / `commitBatch` / `abortBatch`，语义与
 *      `SparrowTransferWallet.prepareBatch` 一致，与 2PC 的 `AmmWalletA2PC` 角色对齐。
 *      初始余额由链下 `bootstrap-wallet-balances-from-csv -kind amm-sparrow-wallet-a` 灌入（`setBalance` / `setBalances`）。
 */
contract SparrowAmmWalletA {
    mapping(address => uint256) public balance;
    mapping(address => bytes32) public accountLock;
    mapping(bytes32 => DebitPrepareLock) public debitPrepareLocks;
    mapping(bytes32 => address) public prepareBatchFrom;

    struct DebitPrepareLock {
        address user;
        uint256 totalAmount;
        bool exists;
    }

    event PreparedBatchLines(bytes32 indexed batchId, address indexed user, bool[] lineOk);
    event Committed(bytes32 indexed batchId);
    event Aborted(bytes32 indexed batchId);

    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return keccak256(abi.encodePacked("amm.sparrow.walletA.balance:", user));
    }

    function setBalance(address user, uint256 amount) external {
        require(user != address(0), "SAW_A: zero");
        require(accountLock[user] == bytes32(0), "SAW_A: locked");
        balance[user] = amount;
    }

    /// @dev 与 2PC AMM / erc20 bootstrap 对称，供链下分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "SAW_A: zero");
            require(accountLock[u] == bytes32(0), "SAW_A: locked");
            balance[u] = v;
        }
    }

    function prepareBatch(bytes32 batchId, address user, uint256[] calldata amounts)
        external
        returns (bool allOk, bool[] memory lineOk)
    {
        require(amounts.length > 0, "SAW_A: empty batch");
        require(user != address(0), "SAW_A: zero");
        require(accountLock[user] == bytes32(0) || accountLock[user] == batchId, "SAW_A: locked");
        require(!debitPrepareLocks[batchId].exists, "SAW_A: batch exists");

        if (accountLock[user] == bytes32(0)) {
            accountLock[user] = batchId;
        }
        prepareBatchFrom[batchId] = user;

        lineOk = new bool[](amounts.length);
        uint256 rem = balance[user];
        allOk = true;
        for (uint256 i = 0; i < amounts.length; i++) {
            uint256 a = amounts[i];
            if (rem >= a) {
                unchecked {
                    rem -= a;
                }
                lineOk[i] = true;
            } else {
                lineOk[i] = false;
                allOk = false;
            }
        }

        emit PreparedBatchLines(batchId, user, lineOk);

        if (allOk) {
            uint256 total = balance[user] - rem;
            balance[user] = rem;
            debitPrepareLocks[batchId] = DebitPrepareLock({user: user, totalAmount: total, exists: true});
        }
        return (allOk, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        address user = prepareBatchFrom[batchId];
        require(user != address(0), "SAW_A: unknown batch");
        require(accountLock[user] == batchId, "SAW_A: lock mismatch");
        if (debitPrepareLocks[batchId].exists) {
            delete debitPrepareLocks[batchId];
        }
        accountLock[user] = bytes32(0);
        delete prepareBatchFrom[batchId];
        emit Committed(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        address user = prepareBatchFrom[batchId];
        if (user == address(0)) {
            return;
        }
        require(accountLock[user] == batchId, "SAW_A: lock mismatch");
        if (debitPrepareLocks[batchId].exists) {
            DebitPrepareLock memory lock = debitPrepareLocks[batchId];
            balance[lock.user] += lock.totalAmount;
            delete debitPrepareLocks[batchId];
        }
        accountLock[user] = bytes32(0);
        delete prepareBatchFrom[batchId];
        emit Aborted(batchId);
    }
}
