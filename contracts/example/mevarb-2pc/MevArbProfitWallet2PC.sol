// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbProfitWallet2PC
 * @dev 仅累计净利润（A 单位）；流程同 `AmmWalletB2PC`：prepareLock → sealCredit → commit / abort。
 *      初始利润由链下 `bootstrap-wallet-balances-from-csv -kind mevarb-2pc-profit-wallet` 可选灌入（与 erc20 同形 `setBalance` / `setBalances`）。
 */
contract MevArbProfitWallet2PC {
    mapping(address => uint256) public profitBalance;
    mapping(address => bytes32) public accountLock;
    mapping(bytes32 => PrepareCredit) private _prepares;

    struct PrepareCredit {
        address user;
        uint256 amount;
        bool exists;
    }

    event ProfitPreparedLock(bytes32 indexed txId, address indexed user);
    event ProfitPreparedCredit(bytes32 indexed txId, address indexed user, uint256 amount);
    event ProfitCommitted(bytes32 indexed txId);
    event ProfitAborted(bytes32 indexed txId);

    constructor() {}

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "profit: zero");
        require(accountLock[user] == bytes32(0), "profit: locked");
        profitBalance[user] = v;
    }

    /// @dev 与 PeerTransferWallet2PC.setBalances 对称，供 bootstrap 分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "profit: zero");
            require(accountLock[u] == bytes32(0), "profit: locked");
            profitBalance[u] = v;
        }
    }

    function _requireAccount(bytes32 txId, address user) internal view {
        require(user != address(0), "profit: zero");
        require(accountLock[user] == bytes32(0) || accountLock[user] == txId, "profit: lock");
    }

    function prepareLock(bytes32 txId, address user) external returns (bool) {
        require(!_prepares[txId].exists, "profit: prepared");
        _requireAccount(txId, user);
        accountLock[user] = txId;
        _prepares[txId] = PrepareCredit({user: user, amount: 0, exists: true});
        emit ProfitPreparedLock(txId, user);
        return true;
    }

    function sealCredit(bytes32 txId, address user, uint256 amount) external returns (bool) {
        require(amount > 0, "profit: amount");
        PrepareCredit storage p = _prepares[txId];
        require(p.exists, "profit: no lock");
        require(p.amount == 0, "profit: sealed");
        require(p.user == user, "profit: user");
        require(accountLock[user] == txId, "profit: lock");
        p.amount = amount;
        emit ProfitPreparedCredit(txId, user, amount);
        return true;
    }

    function commit(bytes32 txId) external {
        PrepareCredit memory p = _prepares[txId];
        require(p.exists, "profit: no prepare");
        require(p.amount > 0, "profit: not sealed");
        require(accountLock[p.user] == txId, "profit: lock");
        profitBalance[p.user] += p.amount;
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit ProfitCommitted(txId);
    }

    function abort(bytes32 txId) external {
        PrepareCredit memory p = _prepares[txId];
        if (!p.exists) {
            return;
        }
        require(accountLock[p.user] == txId, "profit: lock");
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit ProfitAborted(txId);
    }
}
