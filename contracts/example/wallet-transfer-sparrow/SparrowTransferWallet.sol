// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowTransferWallet
 * @dev 2PL：`prepareTransferWave` 扩锁 + 内存模拟（不落账），持锁至 `commitTransferWave` 或 `abortTransferWave`。
 *      - prepare 返回 `abi.encode(uint8 mode, uint256 batchId, bool[] lineOk)`；`mode==1` 或 `batchId==0` 表示锁失败，无 pending。
 *      - commit 返回 `abi.encode(uint8 mode, bool[] lineOk)`，仅对 `prepOk` 行落账，释锁并删 pending。
 *      `abortTransferWave` 仅释锁删 pending。`precheckLine` / `transferLine` / `precheckWave` 同前。
 *
 *      初始余额为空；部署后由 `cmd/bootstrap-wallet-balances-from-csv -kind sparrow` 调 `setBalances` / `setBalance` 写入（与 2PC 钱包工具对称）。
 */
contract SparrowTransferWallet {
    mapping(address => uint256) public balance;
    mapping(address => bool) private _locked;

    uint8 public constant EXEC_MODE_LINE_RESULTS = 0;
    uint8 public constant EXEC_MODE_LOCK_FAIL = 1;

    uint256 private _batchNonce;

    struct PendingBatch {
        bool active;
        address[] participants;
        address[] froms;
        address[] tos;
        uint256[] amounts;
        bool[] prepOk;
    }

    mapping(uint256 => PendingBatch) private _pending;

    constructor() {}

    function setBalance(address user, uint256 amount) external {
        require(user != address(0), "STWallet: zero");
        require(!_locked[user], "STWallet: locked");
        balance[user] = amount;
    }

    /// @dev 与 `PeerTransferWallet2PC.setBalances` 对称，供 bootstrap 分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "STWallet: zero");
            require(!_locked[u], "STWallet: locked");
            balance[u] = v;
        }
    }

    function transferLine(address from, address to, uint256 amount) external {
        require(!_locked[from] && !_locked[to], "STWallet: locked");
        require(_tryTransferLine(from, to, amount), "STWallet: transfer");
    }

    function precheckLine(address from, address to, uint256 amount) external view returns (bool) {
        if (from == to || amount == 0) return false;
        if (from == address(0) || to == address(0)) return false;
        if (balance[from] < amount) return false;
        uint256 toNew = balance[to] + amount;
        if (toNew < balance[to]) return false;
        return true;
    }

    function precheckWave(
        address[] calldata froms,
        address[] calldata tos,
        uint256[] calldata amounts
    ) external view {
        uint256 n = froms.length;
        require(tos.length == n && amounts.length == n, "STWallet: length");

        (address[] memory uniq, uint256 nu) = _uniqueParticipants(froms, tos, n);
        uint256[] memory sim = new uint256[](nu);
        for (uint256 i = 0; i < nu; i++) {
            sim[i] = balance[uniq[i]];
        }

        for (uint256 i = 0; i < n; i++) {
            _precheckLine(uniq, nu, sim, froms[i], tos[i], amounts[i]);
        }
    }

    /**
     * @return `abi.encode(uint8 mode, uint256 batchId, bool[] lineOk)`。持锁至 commit/abort。
     */
    function prepareTransferWave(
        address[] calldata froms,
        address[] calldata tos,
        uint256[] calldata amounts
    ) external returns (bytes memory) {
        uint256 n = froms.length;
        if (tos.length != n || amounts.length != n) {
            return abi.encode(EXEC_MODE_LOCK_FAIL, uint256(0), _allFalse(n));
        }

        (address[] memory participants, uint256 m) = _uniqueParticipantsSorted(froms, tos, n);

        for (uint256 i = 0; i < m; i++) {
            if (_locked[participants[i]]) {
                return abi.encode(EXEC_MODE_LOCK_FAIL, uint256(0), _allFalse(n));
            }
        }

        for (uint256 i = 0; i < m; i++) {
            _locked[participants[i]] = true;
        }

        bool[] memory lineOk = _simulateOrderedLines(froms, tos, amounts, n);

        uint256 batchId = ++_batchNonce;
        _storePending(batchId, participants, froms, tos, amounts, lineOk);

        return abi.encode(EXEC_MODE_LINE_RESULTS, batchId, lineOk);
    }

    /**
     * @return `abi.encode(uint8 mode, bool[] lineOk)`，`lineOk[i]` 为 commit 时实际是否落账成功。
     */
    function commitTransferWave(uint256 batchId) external returns (bytes memory) {
        PendingBatch storage p = _pending[batchId];
        require(p.active, "STWallet: no batch");

        uint256 n = p.froms.length;
        bool[] memory committed = new bool[](n);

        for (uint256 i = 0; i < n; i++) {
            if (!p.prepOk[i]) {
                committed[i] = false;
                continue;
            }
            committed[i] = _tryTransferLine(p.froms[i], p.tos[i], p.amounts[i]);
        }

        _releaseAndClear(batchId, p);

        return abi.encode(EXEC_MODE_LINE_RESULTS, committed);
    }

    function abortTransferWave(uint256 batchId) external {
        PendingBatch storage p = _pending[batchId];
        require(p.active, "STWallet: no batch");
        _releaseAndClear(batchId, p);
    }

    function _releaseAndClear(uint256 batchId, PendingBatch storage p) private {
        uint256 m = p.participants.length;
        for (uint256 k = 0; k < m; k++) {
            _locked[p.participants[k]] = false;
        }
        delete _pending[batchId];
    }

    function _storePending(
        uint256 batchId,
        address[] memory participants,
        address[] calldata froms,
        address[] calldata tos,
        uint256[] calldata amounts,
        bool[] memory lineOk
    ) private {
        PendingBatch storage p = _pending[batchId];
        p.active = true;
        uint256 n = froms.length;
        p.participants = new address[](participants.length);
        for (uint256 i = 0; i < participants.length; i++) {
            p.participants[i] = participants[i];
        }
        p.froms = new address[](n);
        p.tos = new address[](n);
        p.amounts = new uint256[](n);
        p.prepOk = new bool[](n);
        for (uint256 i = 0; i < n; i++) {
            p.froms[i] = froms[i];
            p.tos[i] = tos[i];
            p.amounts[i] = amounts[i];
            p.prepOk[i] = lineOk[i];
        }
    }

    function _simulateOrderedLines(
        address[] calldata froms,
        address[] calldata tos,
        uint256[] calldata amounts,
        uint256 n
    ) private view returns (bool[] memory lineOk) {
        lineOk = new bool[](n);
        (address[] memory uniq, uint256 nu) = _uniqueParticipants(froms, tos, n);
        uint256[] memory sim = new uint256[](nu);
        for (uint256 i = 0; i < nu; i++) {
            sim[i] = balance[uniq[i]];
        }

        for (uint256 i = 0; i < n; i++) {
            lineOk[i] = _simLine(uniq, nu, sim, froms[i], tos[i], amounts[i]);
        }
    }

    function _simLine(
        address[] memory uniq,
        uint256 nu,
        uint256[] memory sim,
        address from,
        address to,
        uint256 amount
    ) private pure returns (bool) {
        if (from == to || amount == 0) return false;
        if (from == address(0) || to == address(0)) return false;
        (bool fiOk, uint256 fi) = _indexOf(uniq, nu, from);
        (bool tiOk, uint256 ti) = _indexOf(uniq, nu, to);
        if (!fiOk || !tiOk) return false;
        if (sim[fi] < amount) return false;
        uint256 toNew = sim[ti] + amount;
        if (toNew < sim[ti]) return false;
        unchecked {
            sim[fi] -= amount;
        }
        sim[ti] = toNew;
        return true;
    }

    function _tryTransferLine(address from, address to, uint256 amount) private returns (bool) {
        if (from == to || amount == 0) return false;
        if (from == address(0) || to == address(0)) return false;
        if (balance[from] < amount) return false;
        uint256 toNew = balance[to] + amount;
        if (toNew < balance[to]) return false;
        unchecked {
            balance[from] -= amount;
        }
        balance[to] = toNew;
        return true;
    }

    function _allFalse(uint256 n) private pure returns (bool[] memory a) {
        a = new bool[](n);
    }

    function _precheckLine(
        address[] memory uniq,
        uint256 nu,
        uint256[] memory sim,
        address from,
        address to,
        uint256 amount
    ) private pure {
        require(from != to, "STWallet: self");
        require(amount > 0, "STWallet: amount");
        require(from != address(0) && to != address(0), "STWallet: zero");
        (bool fiOk, uint256 fi) = _indexOf(uniq, nu, from);
        (bool tiOk, uint256 ti) = _indexOf(uniq, nu, to);
        require(fiOk && tiOk, "STWallet: internal");
        require(sim[fi] >= amount, "STWallet: insufficient");
        uint256 toNew = sim[ti] + amount;
        require(toNew >= sim[ti], "STWallet: overflow");
        unchecked {
            sim[fi] -= amount;
        }
        sim[ti] = toNew;
    }

    function _indexOf(
        address[] memory arr,
        uint256 len,
        address a
    ) private pure returns (bool, uint256) {
        for (uint256 i = 0; i < len; i++) {
            if (arr[i] == a) {
                return (true, i);
            }
        }
        return (false, 0);
    }

    function _uniqueParticipants(
        address[] calldata froms,
        address[] calldata tos,
        uint256 n
    ) private pure returns (address[] memory uniq, uint256 nu) {
        address[] memory tmp = new address[](2 * n);
        nu = 0;
        for (uint256 i = 0; i < n; i++) {
            nu = _pushUnique(tmp, nu, froms[i]);
            nu = _pushUnique(tmp, nu, tos[i]);
        }
        uniq = new address[](nu);
        for (uint256 i = 0; i < nu; i++) {
            uniq[i] = tmp[i];
        }
    }

    function _pushUnique(address[] memory tmp, uint256 nu, address a) private pure returns (uint256) {
        for (uint256 i = 0; i < nu; i++) {
            if (tmp[i] == a) {
                return nu;
            }
        }
        tmp[nu] = a;
        return nu + 1;
    }

    function _uniqueParticipantsSorted(
        address[] calldata froms,
        address[] calldata tos,
        uint256 n
    ) private pure returns (address[] memory sorted, uint256 m) {
        (address[] memory uniq, uint256 nu) = _uniqueParticipants(froms, tos, n);
        sorted = new address[](nu);
        for (uint256 i = 0; i < nu; i++) {
            sorted[i] = uniq[i];
        }
        for (uint256 i = 0; i < nu; i++) {
            for (uint256 j = i + 1; j < nu; j++) {
                if (uint160(sorted[i]) > uint160(sorted[j])) {
                    (sorted[i], sorted[j]) = (sorted[j], sorted[i]);
                }
            }
        }
        m = nu;
    }
}
