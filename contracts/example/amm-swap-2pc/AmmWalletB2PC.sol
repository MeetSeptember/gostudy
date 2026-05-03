// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title AmmWalletB2PC
 * @dev 2PC 收币侧（代币 B）。初始余额由链下 `bootstrap-wallet-balances-from-csv -kind amm-2pc-wallet-b` 灌入。
 *
 *      协调者流程：`prepareLock` → `sealCredit`（写入 Pool 算出的 amountOut）→ `commit` / `abort`。
 *      亦可单笔 `prepareCredit`（已知 amount 时一次完成 lock+额度）。
 *
 *      `amount==0` 表示已 lock、尚未 seal；`commit` 要求已 seal（amount>0）。
 */
contract AmmWalletB2PC {
    struct PrepareCredit {
        address user;
        uint256 amount;
        bool exists;
    }

    mapping(address => uint256) public balance;
    mapping(address => bytes32) public accountLock;
    mapping(bytes32 => PrepareCredit) private _prepares;

    event PreparedLock(bytes32 indexed txId, address indexed user);
    event PreparedCredit(bytes32 indexed txId, address indexed user, uint256 amount);
    event Committed(bytes32 indexed txId);
    event Aborted(bytes32 indexed txId);

    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return keccak256(abi.encodePacked("amm.2pc.walletB.balance:", user));
    }

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "W2PC_B: zero");
        require(accountLock[user] == bytes32(0), "W2PC_B: locked");
        balance[user] = v;
    }

    /// @dev 与 WalletA / erc20 bootstrap 对称，供链下分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "W2PC_B: zero");
            require(accountLock[u] == bytes32(0), "W2PC_B: locked");
            balance[u] = v;
        }
    }

    function _requireAccount(bytes32 txId, address user) internal view {
        require(user != address(0), "W2PC_B: zero");
        require(accountLock[user] == bytes32(0) || accountLock[user] == txId, "W2PC_B: account locked");
    }

    function prepareLock(bytes32 txId, address user) external returns (bool) {
        require(!_prepares[txId].exists, "W2PC_B: prepared");
        _requireAccount(txId, user);

        accountLock[user] = txId;
        _prepares[txId] = PrepareCredit({user: user, amount: 0, exists: true});
        emit PreparedLock(txId, user);
        return true;
    }

    function sealCredit(bytes32 txId, address user, uint256 amount) external returns (bool) {
        require(amount > 0, "W2PC_B: amount");
        PrepareCredit storage p = _prepares[txId];
        require(p.exists, "W2PC_B: no lock");
        require(p.amount == 0, "W2PC_B: sealed");
        require(p.user == user, "W2PC_B: user");
        require(accountLock[user] == txId, "W2PC_B: lock");

        p.amount = amount;
        emit PreparedCredit(txId, user, amount);
        return true;
    }

    function prepareCredit(bytes32 txId, address user, uint256 amount) external returns (bool) {
        require(amount > 0, "W2PC_B: amount");
        require(!_prepares[txId].exists, "W2PC_B: prepared");
        _requireAccount(txId, user);

        accountLock[user] = txId;
        _prepares[txId] = PrepareCredit({user: user, amount: amount, exists: true});
        emit PreparedCredit(txId, user, amount);
        return true;
    }

    function commit(bytes32 txId) external {
        PrepareCredit memory p = _prepares[txId];
        require(p.exists, "W2PC_B: no prepare");
        require(p.amount > 0, "W2PC_B: not sealed");
        require(accountLock[p.user] == txId, "W2PC_B: lock");
        balance[p.user] += p.amount;
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit Committed(txId);
    }

    function abort(bytes32 txId) external {
        PrepareCredit memory p = _prepares[txId];
        if (!p.exists) {
            return;
        }
        require(accountLock[p.user] == txId, "W2PC_B: lock");
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit Aborted(txId);
    }
}
