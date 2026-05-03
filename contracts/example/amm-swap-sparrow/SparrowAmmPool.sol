// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowAmmPool
 * @dev Pool 分片合约（实验）：无 `msg.sender` 鉴权。`quoteSwap` 只读；prepare 走 `prepareReadWriteBatch`，
 *      **信任**传入的 `newReserveA/B`，不在此重算校验。首行通过则 `batchId` 锁池、写 pending、`ammVersion++`；
 *      后续行顺序隐式为 index，仅发 `PoolPrepareItem(..., false)`。
 */
contract SparrowAmmPool {
    uint256 public constant INITIAL_RESERVE = 10 ** 24;

    uint256 public ammVersion;

    uint256 public reserveA;
    uint256 public reserveB;

    bytes32 public lockedBatchId;
    uint256 private _pendingReserveA;
    uint256 private _pendingReserveB;

    uint256 private _ammVersionBeforePrepare;

    event PoolPrepareItem(bytes32 indexed batchId, uint256 itemIndex, bool success);
    event PoolBatchCommitted(bytes32 indexed batchId);
    event PoolBatchAborted(bytes32 indexed batchId);

    constructor() {
        reserveA = INITIAL_RESERVE;
        reserveB = INITIAL_RESERVE;
        ammVersion = 1;
    }

    function setReserves(uint256 rA, uint256 rB) external {
        require(lockedBatchId == bytes32(0), "SAP: locked");
        require(rA > 0 && rB > 0, "SAP: reserves");
        reserveA = rA;
        reserveB = rB;
    }

    function _quoteCore(uint256 amountIn, uint256 minOut, address user)
        private
        view
        returns (bool ok, uint256 amountOut, uint256 newReserveA, uint256 newReserveB)
    {
        uint256 rA = reserveA;
        uint256 rB = reserveB;

        if (amountIn == 0 || user == address(0)) {
            return (false, 0, rA, rB);
        }

        uint256 outAmt = (rB * amountIn) / (rA + amountIn);
        if (outAmt < minOut || outAmt == 0 || rB < outAmt) {
            return (false, 0, rA, rB);
        }

        newReserveA = rA + amountIn;
        unchecked {
            newReserveB = rB - outAmt;
        }
        return (true, outAmt, newReserveA, newReserveB);
    }

    /// @notice 只读权威储备与版本；不修改状态。
    function quoteSwap(uint256 amountIn, uint256 minOut, address user)
        external
        view
        returns (bool ok, uint256 amountOut, uint256 newReserveA, uint256 newReserveB, uint256 versionRead)
    {
        versionRead = ammVersion;
        (ok, amountOut, newReserveA, newReserveB) = _quoteCore(amountIn, minOut, user);
    }

    function _emitPoolTailPrepareFalse(bytes32 batchId, uint256 n) private {
        for (uint256 i = 1; i < n; i++) {
            emit PoolPrepareItem(batchId, i, false);
        }
    }

    /**
     * @notice 协调者跨分片传入读写集提案；仅校验首行 `clientVersion` 与当前 `ammVersion`（0 表示入口快照）。
     * @return batchPrepared 首行是否成功锁池；`lineOk[0]` 与其一致，其余行为 false。
     */
    function prepareReadWriteBatch(
        bytes32 batchId,
        uint256[] calldata newReserveAs,
        uint256[] calldata newReserveBs,
        uint256[] calldata clientVersions
    ) external returns (bool batchPrepared, bool[] memory lineOk) {
        uint256 n = newReserveAs.length;
        require(n > 0, "SAP: empty");
        require(newReserveBs.length == n && clientVersions.length == n, "SAP: len");
        require(lockedBatchId == bytes32(0), "SAP: locked");

        lineOk = new bool[](n);
        uint256 verSnap = ammVersion;
        uint256 cv0 = clientVersions[0] == 0 ? verSnap : clientVersions[0];

        if (cv0 != ammVersion) {
            emit PoolPrepareItem(batchId, 0, false);
            _emitPoolTailPrepareFalse(batchId, n);
            return (false, lineOk);
        }

        lockedBatchId = batchId;
        _pendingReserveA = newReserveAs[0];
        _pendingReserveB = newReserveBs[0];
        _ammVersionBeforePrepare = ammVersion;
        unchecked {
            ammVersion += 1;
        }
        lineOk[0] = true;
        batchPrepared = true;
        emit PoolPrepareItem(batchId, 0, true);

        for (uint256 i = 1; i < n; i++) {
            lineOk[i] = false;
            emit PoolPrepareItem(batchId, i, false);
        }
        return (batchPrepared, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        require(lockedBatchId == batchId, "SAP: lock");

        reserveA = _pendingReserveA;
        reserveB = _pendingReserveB;
        _pendingReserveA = 0;
        _pendingReserveB = 0;
        lockedBatchId = bytes32(0);
        _ammVersionBeforePrepare = 0;

        emit PoolBatchCommitted(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        if (lockedBatchId != batchId) {
            return;
        }
        if (_ammVersionBeforePrepare != 0) {
            ammVersion = _ammVersionBeforePrepare;
            _ammVersionBeforePrepare = 0;
        }
        _pendingReserveA = 0;
        _pendingReserveB = 0;
        lockedBatchId = bytes32(0);
        emit PoolBatchAborted(batchId);
    }
}
