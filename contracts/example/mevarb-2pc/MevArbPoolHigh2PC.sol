// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbPoolHigh2PC
 * @dev 高价池 B→A；初值与 `MevArbPoolHighMasterV2` 一致。`prepareSwapBToA`：outA = ra * dy / (rb + dy)。
 */
contract MevArbPoolHigh2PC {
    uint256 public constant INITIAL_RESERVE_A = 10 ** 24;
    uint256 public constant INITIAL_RESERVE_B = 10 ** 22;

    uint256 public reserveA;
    uint256 public reserveB;
    bytes32 public lockedByTxId;

    struct PendingSwap {
        address user;
        uint256 amountInB;
        uint256 amountOutA;
        bool exists;
    }

    mapping(bytes32 => PendingSwap) public pendingSwaps;

    event PoolHighPrepared(bytes32 indexed txId, address indexed user, uint256 amountInB, uint256 amountOutA);
    event PoolHighCommitted(bytes32 indexed txId);
    event PoolHighAborted(bytes32 indexed txId);

    constructor() {
        reserveA = INITIAL_RESERVE_A;
        reserveB = INITIAL_RESERVE_B;
    }

    function setReserves(uint256 rA, uint256 rB) external {
        require(lockedByTxId == bytes32(0), "high: locked");
        require(rA > 0 && rB > 0, "high: reserves");
        reserveA = rA;
        reserveB = rB;
    }

    /// @return amountOutA
    function prepareSwapBToA(bytes32 txId, address user, uint256 amountInB, uint256 minOutA)
        external
        returns (uint256 amountOutA)
    {
        require(user != address(0), "high: user");
        require(amountInB > 0, "high: amountIn");
        require(!pendingSwaps[txId].exists, "high: prepared");
        require(lockedByTxId == bytes32(0) || lockedByTxId == txId, "high: lock");

        amountOutA = (reserveA * amountInB) / (reserveB + amountInB);
        require(amountOutA >= minOutA, "high: slippage");
        require(amountOutA > 0 && reserveA >= amountOutA, "high: liq");

        reserveA -= amountOutA;
        if (lockedByTxId == bytes32(0)) {
            lockedByTxId = txId;
        }
        pendingSwaps[txId] = PendingSwap({user: user, amountInB: amountInB, amountOutA: amountOutA, exists: true});
        emit PoolHighPrepared(txId, user, amountInB, amountOutA);
    }

    function commit(bytes32 txId) external {
        require(lockedByTxId == txId, "high: lock");
        PendingSwap memory p = pendingSwaps[txId];
        require(p.exists, "high: no prepare");
        reserveB += p.amountInB;
        delete pendingSwaps[txId];
        lockedByTxId = bytes32(0);
        emit PoolHighCommitted(txId);
    }

    function abort(bytes32 txId) external {
        PendingSwap memory p = pendingSwaps[txId];
        if (!p.exists) {
            return;
        }
        require(lockedByTxId == txId, "high: lock");
        reserveA += p.amountOutA;
        delete pendingSwaps[txId];
        lockedByTxId = bytes32(0);
        emit PoolHighAborted(txId);
    }
}
