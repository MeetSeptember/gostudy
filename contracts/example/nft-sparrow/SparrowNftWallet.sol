// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowNftWallet
 * @dev 批处理语义同 `SparrowWallet`；余额由链下 `bootstrap-wallet-balances-from-csv -kind nft-sparrow-wallet` 灌入（与 `NftWallet2PC` 同形 `setBalance` / `setBalances`）。
 */
contract SparrowNftWallet {
    mapping(address => uint256) public balance;

    mapping(address => bytes32) public lockBuyer;
    mapping(bytes32 => PrepareLock) public prepareLocks;
    mapping(bytes32 => address) public prepareBatchUser;

    struct PrepareLock {
        address user;
        uint256 totalAmount;
        bool exists;
    }

    event PreparedBatchLines(bytes32 indexed batchId, address indexed user, bool[] lineOk);
    event Committed(bytes32 indexed batchId);
    event Aborted(bytes32 indexed batchId);

    constructor() {}

    function setBalance(address user, uint256 amount) external {
        require(user != address(0), "SNftW: zero");
        require(lockBuyer[user] == bytes32(0), "SNftW: locked");
        balance[user] = amount;
    }

    /// @dev 与 erc20 / nft-2pc-wallet bootstrap 对称，供链下分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "SNftW: zero");
            require(lockBuyer[u] == bytes32(0), "SNftW: locked");
            balance[u] = v;
        }
    }

    function prepareBatch(bytes32 batchId, address user, uint256[] calldata amounts)
        external
        returns (bool allOk, bool[] memory lineOk)
    {
        require(user != address(0), "SNftW: zero");
        require(amounts.length > 0, "SNftW: empty batch");
        require(lockBuyer[user] == bytes32(0) || lockBuyer[user] == batchId, "SNftW: buyer locked");
        require(!prepareLocks[batchId].exists, "SNftW: batch exists");

        if (lockBuyer[user] == bytes32(0)) {
            lockBuyer[user] = batchId;
        }
        prepareBatchUser[batchId] = user;

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
            prepareLocks[batchId] = PrepareLock({user: user, totalAmount: total, exists: true});
        }
        return (allOk, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        address user = prepareBatchUser[batchId];
        require(user != address(0), "SNftW: unknown batch");
        require(lockBuyer[user] == batchId, "SNftW: lock mismatch");
        if (prepareLocks[batchId].exists) {
            delete prepareLocks[batchId];
        }
        lockBuyer[user] = bytes32(0);
        delete prepareBatchUser[batchId];
        emit Committed(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        address user = prepareBatchUser[batchId];
        if (user == address(0)) {
            return;
        }
        require(lockBuyer[user] == batchId, "SNftW: lock mismatch");
        if (prepareLocks[batchId].exists) {
            PrepareLock memory lock = prepareLocks[batchId];
            balance[lock.user] += lock.totalAmount;
            delete prepareLocks[batchId];
        }
        lockBuyer[user] = bytes32(0);
        delete prepareBatchUser[batchId];
        emit Aborted(batchId);
    }
}
