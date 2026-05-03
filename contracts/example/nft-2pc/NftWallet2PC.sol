// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title NftWallet2PC
 * @dev 2PC 付币侧：账户锁 + `prepareDebit` / `commit` / `abort`，对齐 `AmmWalletA2PC`；
 *      `balanceKey` 前缀 `nft.2pc.wallet.balance:`，与 `nft.joyue.wallet.balance:` 对照。
 *      初始余额由链下 `bootstrap-wallet-balances-from-csv -kind nft-2pc-wallet` 灌入。
 */
contract NftWallet2PC {
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
        return keccak256(abi.encodePacked("nft.2pc.wallet.balance:", user));
    }

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "NFT2PC_W: zero");
        require(accountLock[user] == bytes32(0), "NFT2PC_W: locked");
        balance[user] = v;
    }

    /// @dev 与 erc20 / amm-2pc-wallet-a bootstrap 对称，供链下分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "NFT2PC_W: zero");
            require(accountLock[u] == bytes32(0), "NFT2PC_W: locked");
            balance[u] = v;
        }
    }

    function _requireAccount(bytes32 txId, address user) internal view {
        require(user != address(0), "NFT2PC_W: zero");
        require(accountLock[user] == bytes32(0) || accountLock[user] == txId, "NFT2PC_W: account locked");
    }

    function prepareDebit(bytes32 txId, address user, uint256 amount) external returns (bool) {
        require(amount > 0, "NFT2PC_W: amount");
        require(!_prepares[txId].exists, "NFT2PC_W: prepared");
        _requireAccount(txId, user);
        require(balance[user] >= amount, "NFT2PC_W: insufficient");

        balance[user] -= amount;
        accountLock[user] = txId;
        _prepares[txId] = PrepareDebit({user: user, amount: amount, exists: true});
        emit PreparedDebit(txId, user, amount);
        return true;
    }

    function commit(bytes32 txId) external {
        PrepareDebit memory p = _prepares[txId];
        require(p.exists, "NFT2PC_W: no prepare");
        require(accountLock[p.user] == txId, "NFT2PC_W: lock");
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit Committed(txId);
    }

    function abort(bytes32 txId) external {
        PrepareDebit memory p = _prepares[txId];
        if (!p.exists) {
            return;
        }
        require(accountLock[p.user] == txId, "NFT2PC_W: lock");
        balance[p.user] += p.amount;
        delete _prepares[txId];
        accountLock[p.user] = bytes32(0);
        emit Aborted(txId);
    }
}
