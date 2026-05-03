// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbPoolLowSparrow
 * @dev `prepareReadWriteBatch` 与 `SparrowAmmPool` 同签名；链上可观测性仅 `PoolLowBatchCommitted` / `PoolLowBatchAborted`（与协调者 `SparrowWave*` 配合即可）。
 */
contract MevArbPoolLowSparrow {
    uint256 public constant INITIAL_RESERVE_A = 10 ** 22;
    uint256 public constant INITIAL_RESERVE_B = 10 ** 24;

    uint256 public poolVersion;
    uint256 public reserveA;
    uint256 public reserveB;

    bytes32 public lockedBatchId;
    uint256 private _pendingReserveA;
    uint256 private _pendingReserveB;

    uint256 private _poolVersionBeforePrepare;

    event PoolLowBatchCommitted(bytes32 indexed batchId);
    event PoolLowBatchAborted(bytes32 indexed batchId);

    constructor() {
        reserveA = INITIAL_RESERVE_A;
        reserveB = INITIAL_RESERVE_B;
        poolVersion = 1;
    }

    function setReserves(uint256 rA, uint256 rB) external {
        require(lockedBatchId == bytes32(0), "low: locked");
        require(rA > 0 && rB > 0, "low: reserves");
        reserveA = rA;
        reserveB = rB;
    }

    function _quoteCore(uint256 amountInA, uint256 minOutB, address user)
        private
        view
        returns (bool ok, uint256 amountOutB, uint256 newRA, uint256 newRB)
    {
        uint256 rA = reserveA;
        uint256 rB = reserveB;
        if (amountInA == 0 || user == address(0)) {
            return (false, 0, rA, rB);
        }
        if (lockedBatchId != bytes32(0)) {
            return (false, 0, rA, rB);
        }
        uint256 outB = (rB * amountInA) / (rA + amountInA);
        if (outB < minOutB || outB == 0 || rB < outB) {
            return (false, 0, rA, rB);
        }
        newRA = rA + amountInA;
        unchecked {
            newRB = rB - outB;
        }
        return (true, outB, newRA, newRB);
    }

    function quoteSwapAToB(uint256 amountInA, uint256 minOutB, address user)
        external
        view
        returns (bool ok, uint256 amountOutB, uint256 newReserveA, uint256 newReserveB, uint256 versionRead)
    {
        versionRead = poolVersion;
        (ok, amountOutB, newReserveA, newReserveB) = _quoteCore(amountInA, minOutB, user);
    }

    function prepareReadWriteBatch(
        bytes32 batchId,
        uint256[] calldata newReserveAs,
        uint256[] calldata newReserveBs,
        uint256[] calldata clientVersions
    ) external returns (bool batchPrepared, bool[] memory lineOk) {
        uint256 n = newReserveAs.length;
        require(n > 0, "low: empty");
        require(newReserveBs.length == n && clientVersions.length == n, "low: len");
        require(lockedBatchId == bytes32(0), "low: locked");

        lineOk = new bool[](n);
        uint256 verSnap = poolVersion;
        uint256 cv0 = clientVersions[0] == 0 ? verSnap : clientVersions[0];

        if (cv0 != poolVersion) {
            for (uint256 i = 1; i < n; i++) {
                lineOk[i] = false;
            }
            return (false, lineOk);
        }

        lockedBatchId = batchId;
        _pendingReserveA = newReserveAs[0];
        _pendingReserveB = newReserveBs[0];
        _poolVersionBeforePrepare = poolVersion;
        unchecked {
            poolVersion += 1;
        }
        lineOk[0] = true;
        batchPrepared = true;

        for (uint256 i = 1; i < n; i++) {
            lineOk[i] = false;
        }
        return (batchPrepared, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        require(lockedBatchId == batchId, "low: lock");

        reserveA = _pendingReserveA;
        reserveB = _pendingReserveB;
        _pendingReserveA = 0;
        _pendingReserveB = 0;
        lockedBatchId = bytes32(0);
        _poolVersionBeforePrepare = 0;

        emit PoolLowBatchCommitted(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        if (lockedBatchId != batchId) {
            return;
        }
        if (_poolVersionBeforePrepare != 0) {
            poolVersion = _poolVersionBeforePrepare;
            _poolVersionBeforePrepare = 0;
        }
        _pendingReserveA = 0;
        _pendingReserveB = 0;
        lockedBatchId = bytes32(0);
        emit PoolLowBatchAborted(batchId);
    }
}
