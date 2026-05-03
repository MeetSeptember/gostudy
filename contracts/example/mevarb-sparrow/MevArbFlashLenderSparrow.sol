// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbFlashLenderSparrow
 * @dev Sparrow 批：`prepareLendBatch` 返回 `(bool allOk, bool[] lineOk)`，与 `SparrowAmmWalletA` 同形；`commitBatch` / `abortBatch`。
 *      批内 **用户不得重复**（与单用户单锁语义一致）。另提供 `quoteLend` 供协调者 view。
 */
contract MevArbFlashLenderSparrow {
    uint256 public constant INITIAL_AVAILABLE = 10 ** 30;

    uint256 public available;
    uint256 public pendingLendCount;

    mapping(address => bytes32) public accountLock;

    struct Pending {
        address user;
        uint256 borrowA;
        bool exists;
    }

    mapping(bytes32 => Pending) public pendingLends;
    mapping(bytes32 => bytes32[]) private _batchTxIds;

    event FlashBatchCommitted(bytes32 indexed batchId);
    event FlashBatchAborted(bytes32 indexed batchId);

    constructor() {
        available = INITIAL_AVAILABLE;
    }

    function setAvailable(uint256 v) external {
        require(pendingLendCount == 0, "flash: pending");
        available = v;
    }

    function quoteLend(bytes32 txId, address user, uint256 borrowA) external view returns (bool ok) {
        if (borrowA == 0 || available < borrowA || pendingLends[txId].exists) {
            return false;
        }
        if (user == address(0)) {
            return false;
        }
        if (accountLock[user] != bytes32(0) && accountLock[user] != txId) {
            return false;
        }
        return true;
    }

    function prepareLendBatch(
        bytes32 batchId,
        bytes32[] calldata txIds,
        address[] calldata users,
        uint256[] calldata borrowAs
    ) external returns (bool allOk, bool[] memory lineOk) {
        uint256 n = txIds.length;
        require(n > 0, "flash: empty");
        require(users.length == n && borrowAs.length == n, "flash: len");
        require(_batchTxIds[batchId].length == 0, "flash: batch exists");

        for (uint256 a = 0; a < n; a++) {
            for (uint256 b = a + 1; b < n; b++) {
                require(users[a] != users[b], "flash: dup user");
            }
        }

        lineOk = new bool[](n);
        allOk = true;
        uint256 rem = available;

        for (uint256 i = 0; i < n; i++) {
            bytes32 tid = txIds[i];
            address u = users[i];
            uint256 b = borrowAs[i];
            bool line = (b > 0 && u != address(0) && !pendingLends[tid].exists && rem >= b && accountLock[u] == bytes32(0));
            if (!line) {
                lineOk[i] = false;
                allOk = false;
            } else {
                lineOk[i] = true;
                unchecked {
                    rem -= b;
                }
            }
        }

        if (!allOk) {
            return (false, lineOk);
        }

        available = rem;
        for (uint256 j = 0; j < n; j++) {
            accountLock[users[j]] = batchId;
            pendingLends[txIds[j]] = Pending({user: users[j], borrowA: borrowAs[j], exists: true});
            unchecked {
                pendingLendCount++;
            }
            _batchTxIds[batchId].push(txIds[j]);
        }
        return (true, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        bytes32[] storage tids = _batchTxIds[batchId];
        require(tids.length > 0, "flash: unknown batch");
        uint256 tot = 0;
        for (uint256 i = 0; i < tids.length; i++) {
            bytes32 tid = tids[i];
            Pending memory p = pendingLends[tid];
            require(p.exists, "flash: no prepare");
            require(accountLock[p.user] == batchId, "flash: lock");
            unchecked {
                tot += p.borrowA;
            }
            delete pendingLends[tid];
            accountLock[p.user] = bytes32(0);
            unchecked {
                pendingLendCount--;
            }
        }
        available += tot;
        delete _batchTxIds[batchId];
        emit FlashBatchCommitted(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        bytes32[] storage tids = _batchTxIds[batchId];
        if (tids.length == 0) {
            return;
        }
        uint256 tot = 0;
        for (uint256 i = 0; i < tids.length; i++) {
            bytes32 tid = tids[i];
            Pending memory p = pendingLends[tid];
            if (!p.exists) {
                continue;
            }
            if (accountLock[p.user] != batchId) {
                continue;
            }
            unchecked {
                tot += p.borrowA;
            }
            delete pendingLends[tid];
            accountLock[p.user] = bytes32(0);
            unchecked {
                pendingLendCount--;
            }
        }
        available += tot;
        delete _batchTxIds[batchId];
        emit FlashBatchAborted(batchId);
    }
}
