// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title AmmWalletA2PC
 * @dev 2PC 付币侧（代币 A）：仅 `prepareDebit` / `commit` / `abort`。
 *      初始余额由链下 `bootstrap-wallet-balances-from-csv -kind amm-2pc-wallet-a` 灌入（`setBalance` / `setBalances`）。
 *
 *      语义：`balanceKey(user)` 前缀 `amm.2pc.walletA.balance:`（与 JOYUE 实验脚本对照用）。
 */
contract AmmWalletA2PC {
    struct PrepareDebit {
        address user;
        uint256 amount;
        bool exists;
    }

    mapping(address => uint256) public balance;
    mapping(address => bytes32) public accountLock;
    mapping(bytes32 => PrepareDebit) private _prepares;

    event PreparedDebit(bytes32 indexed txId, address indexed user, uint256 amount);
    event Committed(bytes32 indexed txId);
    event Aborted(bytes32 indexed txId);

    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return keccak256(abi.encodePacked("amm.2pc.walletA.balance:", user));
    }

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "W2PC_A: zero");
        require(accountLock[user] == bytes32(0), "W2PC_A: locked");
        balance[user] = v;
    }

    /// @dev 与 2PC erc20 / joyue Master 对称，供 bootstrap 分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "W2PC_A: zero");
            require(accountLock[u] == bytes32(0), "W2PC_A: locked");
            balance[u] = v;
        }
    }

    function _requireAccount(bytes32 txId, address user) internal view {
        require(user != address(0), "W2PC_A: zero");
        require(accountLock[user] == bytes32(0) || accountLock[user] == txId, "W2PC_A: account locked");
    }

    function prepareDebit(bytes32 txId, address user, uint256 amount) external returns (bool) {
        require(amount > 0, "W2PC_A: amount");
        require(!_prepares[txId].exists, "W2PC_A: prepared");
        _requireAccount(txId, user);
        require(balance[user] >= amount, "W2PC_A: insufficient");

        balance[user] -= amount;
        accountLock[user] = txId;
        _prepares[txId] = PrepareDebit({user: user, amount: amount, exists: true});
        emit PreparedDebit(txId, user, amount);
        return true;
    }

    function commit(bytes32 txId) external {
        PrepareDebit memory p = _prepares[txId];
        require(p.exists, "W2PC_A: no prepare");
        require(accountLock[p.user] == txId, "W2PC_A: lock");
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit Committed(txId);
    }

    function abort(bytes32 txId) external {
        PrepareDebit memory p = _prepares[txId];
        if (!p.exists) {
            return;
        }
        require(accountLock[p.user] == txId, "W2PC_A: lock");
        balance[p.user] += p.amount;
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit Aborted(txId);
    }
}
