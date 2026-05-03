// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title PeerTransferWallet2PC
 * @dev 单 Participant 转账账本：`prepareTransfer` 按地址序尝试锁定 `from`/`to`；若任一方已被 **其他** `txId` 占用，
 *      则 **释放本笔已获得的锁** 并 `return false`（不 revert），供 Coordinator 走 abort。
 *      锁定成功后若余额不足，同样释放双锁并 `false`。成功则从 `from` 扣 `amount` 至托管；`commit` 入账；`abort` 退回。
 *      无构造参数；部署后用 `cmd/bootstrap-wallet-balances-from-csv` 调 `setBalance` / `setBalances` 写入余额。无地址白名单（本地实验用）。
 */
contract PeerTransferWallet2PC {
    struct PrepareTransfer {
        address from;
        address to;
        uint256 amount;
        bool exists;
    }

    mapping(address => uint256) public balance;
    mapping(address => bytes32) public accountLock;
    mapping(bytes32 => PrepareTransfer) private _prepares;

    event PreparedTransfer(bytes32 indexed txId, address indexed from, address indexed to, uint256 amount);
    event Committed(bytes32 indexed txId);
    event Aborted(bytes32 indexed txId);

    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return keccak256(abi.encodePacked("peer.transfer.2pc.wallet.balance:", user));
    }

    function setBalance(address user, uint256 v) external {
        require(accountLock[user] == bytes32(0), "PTW: locked");
        require(user != address(0), "PTW: zero");
        balance[user] = v;
    }

    /// @dev 对多个地址写入同一 `v`；任一为零地址或仍被锁则整笔 revert。
    function setBalances(address[] calldata users, uint256 v) external {
        uint256 n = users.length;
        for (uint256 i = 0; i < n; i++) {
            address user = users[i];
            require(user != address(0), "PTW: zero");
            require(accountLock[user] == bytes32(0), "PTW: locked");
            balance[user] = v;
        }
    }

    function _releaseLocksHeldByTx(address from, address to, bytes32 txId) private {
        if (accountLock[from] == txId) {
            accountLock[from] = bytes32(0);
        }
        if (accountLock[to] == txId) {
            accountLock[to] = bytes32(0);
        }
    }

    /// @dev 按地址升序加锁；遇冲突则释放本笔已占用的锁并返回 false。
    function _tryLockTwoAccounts(bytes32 txId, address from, address to) private returns (bool) {
        (address first, address second) = uint160(from) < uint160(to) ? (from, to) : (to, from);

        if (accountLock[first] != bytes32(0) && accountLock[first] != txId) {
            return false;
        }
        accountLock[first] = txId;

        if (accountLock[second] != bytes32(0) && accountLock[second] != txId) {
            accountLock[first] = bytes32(0);
            return false;
        }
        accountLock[second] = txId;
        return true;
    }

    function prepareTransfer(bytes32 txId, address from, address to, uint256 amount) external returns (bool) {
        require(amount > 0, "PTW: amount");
        require(from != to, "PTW: self");
        require(from != address(0) && to != address(0), "PTW: zero");
        require(!_prepares[txId].exists, "PTW: prepared");

        if (!_tryLockTwoAccounts(txId, from, to)) {
            return false;
        }

        if (balance[from] < amount) {
            _releaseLocksHeldByTx(from, to, txId);
            return false;
        }

        unchecked {
            balance[from] -= amount;
        }
        _prepares[txId] = PrepareTransfer({from: from, to: to, amount: amount, exists: true});
        emit PreparedTransfer(txId, from, to, amount);
        return true;
    }

    function commit(bytes32 txId) external {
        PrepareTransfer memory p = _prepares[txId];
        require(p.exists, "PTW: no prepare");
        require(accountLock[p.from] == txId && accountLock[p.to] == txId, "PTW: lock");

        uint256 toNew = balance[p.to] + p.amount;
        require(toNew >= balance[p.to], "PTW: overflow");

        delete _prepares[txId];
        _releaseLocksHeldByTx(p.from, p.to, txId);
        balance[p.to] = toNew;
        emit Committed(txId);
    }

    function abort(bytes32 txId) external {
        PrepareTransfer memory p = _prepares[txId];
        if (!p.exists) {
            return;
        }
        if (accountLock[p.from] != txId || accountLock[p.to] != txId) {
            return;
        }
        unchecked {
            balance[p.from] += p.amount;
        }
        delete _prepares[txId];
        _releaseLocksHeldByTx(p.from, p.to, txId);
        emit Aborted(txId);
    }
}
