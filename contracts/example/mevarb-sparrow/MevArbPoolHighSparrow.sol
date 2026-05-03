// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbPoolHighSparrow
 * @dev B→A；`prepareReadWriteBatch` 与 `SparrowAmmPool` 同签名；链上可观测性仅 `PoolHighBatchCommitted` / `PoolHighBatchAborted`。
 */
contract MevArbPoolHighSparrow {
    uint256 public constant INITIAL_RESERVE_A = 10 ** 24;
    uint256 public constant INITIAL_RESERVE_B = 10 ** 22;

    uint256 public poolVersion;
    uint256 public reserveA;
    uint256 public reserveB;

    bytes32 public lockedBatchId;
    uint256 private _pendingReserveA;
    uint256 private _pendingReserveB;

    uint256 private _poolVersionBeforePrepare;

    event PoolHighBatchCommitted(bytes32 indexed batchId);
    event PoolHighBatchAborted(bytes32 indexed batchId);

    constructor() {
        reserveA = INITIAL_RESERVE_A;
        reserveB = INITIAL_RESERVE_B;
        poolVersion = 1;
    }

    function setReserves(uint256 rA, uint256 rB) external {
        require(lockedBatchId == bytes32(0), "high: locked");
        require(rA > 0 && rB > 0, "high: reserves");
        reserveA = rA;
        reserveB = rB;
    }

    function _quoteCore(uint256 amountInB, uint256 minOutA, address user)
        private
        view
        returns (bool ok, uint256 amountOutA, uint256 newRA, uint256 newRB)
    {
        uint256 rA = reserveA;
        uint256 rB = reserveB;
        if (amountInB == 0 || user == address(0)) {
            return (false, 0, rA, rB);
        }
        if (lockedBatchId != bytes32(0)) {
            return (false, 0, rA, rB);
        }
        uint256 outA = (rA * amountInB) / (rB + amountInB);
        if (outA < minOutA || outA == 0 || rA < outA) {
            return (false, 0, rA, rB);
        }
        unchecked {
            newRA = rA - outA;
            newRB = rB + amountInB;
        }
        return (true, outA, newRA, newRB);
    }

    function quoteSwapBToA(uint256 amountInB, uint256 minOutA, address user)
        external
        view
        returns (bool ok, uint256 amountOutA, uint256 newReserveA, uint256 newReserveB, uint256 versionRead)
    {
        versionRead = poolVersion;
        (ok, amountOutA, newReserveA, newReserveB) = _quoteCore(amountInB, minOutA, user);
    }

    function prepareReadWriteBatch(
        bytes32 batchId,
        uint256[] calldata newReserveAs,
        uint256[] calldata newReserveBs,
        uint256[] calldata clientVersions
    ) external returns (bool batchPrepared, bool[] memory lineOk) {
        uint256 n = newReserveAs.length;
        require(n > 0, "high: empty");
        require(newReserveBs.length == n && clientVersions.length == n, "high: len");
        require(lockedBatchId == bytes32(0), "high: locked");

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
        require(lockedBatchId == batchId, "high: lock");

        reserveA = _pendingReserveA;
        reserveB = _pendingReserveB;
        _pendingReserveA = 0;
        _pendingReserveB = 0;
        lockedBatchId = bytes32(0);
        _poolVersionBeforePrepare = 0;

        emit PoolHighBatchCommitted(batchId);
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
        emit PoolHighBatchAborted(batchId);
    }
}
