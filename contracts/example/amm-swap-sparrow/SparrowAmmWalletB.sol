// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowAmmWalletB
 * @dev Sparrow AMM 收币侧（分片 B）：仅 `prepareCreditBatch` / `commitBatch` / `abortBatch`，
 *      prepare 只预留额度、commit 才真正入账；与 `SparrowTransferWallet.prepareCreditBatch` 一致，
 *      与 2PC 的 `AmmWalletB2PC` 角色对齐。B 侧余额可由 swap commit 增加；压测一般只需 bootstrap WalletA。
 */
contract SparrowAmmWalletB {
    mapping(address => uint256) public balance;
    mapping(address => bytes32) public accountLock;
    mapping(bytes32 => CreditPrepareLock) public creditPrepareLocks;
    mapping(bytes32 => address) public prepareBatchTo;

    struct CreditPrepareLock {
        address user;
        uint256 pendingAmount;
        bool exists;
    }

    event PreparedCreditBatchLines(bytes32 indexed batchId, address indexed user, bool[] lineOk);
    event Committed(bytes32 indexed batchId);
    event Aborted(bytes32 indexed batchId);

    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return keccak256(abi.encodePacked("amm.sparrow.walletB.balance:", user));
    }

    function setBalance(address user, uint256 amount) external {
        require(user != address(0), "SAW_B: zero");
        require(accountLock[user] == bytes32(0), "SAW_B: locked");
        balance[user] = amount;
    }

    /// @dev 与 WalletA / 2PC B 对称，供按需链下灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "SAW_B: zero");
            require(accountLock[u] == bytes32(0), "SAW_B: locked");
            balance[u] = v;
        }
    }

    function prepareCreditBatch(bytes32 batchId, address user, uint256[] calldata amounts)
        external
        returns (bool allOk, bool[] memory lineOk)
    {
        require(amounts.length > 0, "SAW_B: empty batch");
        require(user != address(0), "SAW_B: zero");
        require(accountLock[user] == bytes32(0) || accountLock[user] == batchId, "SAW_B: locked");
        require(!creditPrepareLocks[batchId].exists, "SAW_B: batch exists");

        if (accountLock[user] == bytes32(0)) {
            accountLock[user] = batchId;
        }
        prepareBatchTo[batchId] = user;

        lineOk = new bool[](amounts.length);
        uint256 cur = balance[user];
        uint256 sim = cur;
        allOk = true;
        for (uint256 i = 0; i < amounts.length; i++) {
            uint256 a = amounts[i];
            if (a > type(uint256).max - sim) {
                lineOk[i] = false;
                allOk = false;
            } else {
                unchecked {
                    sim += a;
                }
                lineOk[i] = true;
            }
        }

        emit PreparedCreditBatchLines(batchId, user, lineOk);

        if (allOk) {
            uint256 pending = sim - cur;
            creditPrepareLocks[batchId] = CreditPrepareLock({user: user, pendingAmount: pending, exists: true});
        }
        return (allOk, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        address user = prepareBatchTo[batchId];
        require(user != address(0), "SAW_B: unknown batch");
        require(accountLock[user] == batchId, "SAW_B: lock mismatch");
        if (creditPrepareLocks[batchId].exists) {
            CreditPrepareLock memory lock = creditPrepareLocks[batchId];
            balance[lock.user] += lock.pendingAmount;
            delete creditPrepareLocks[batchId];
        }
        accountLock[user] = bytes32(0);
        delete prepareBatchTo[batchId];
        emit Committed(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        address user = prepareBatchTo[batchId];
        if (user == address(0)) {
            return;
        }
        require(accountLock[user] == batchId, "SAW_B: lock mismatch");
        if (creditPrepareLocks[batchId].exists) {
            delete creditPrepareLocks[batchId];
        }
        accountLock[user] = bytes32(0);
        delete prepareBatchTo[batchId];
        emit Aborted(batchId);
    }
}
