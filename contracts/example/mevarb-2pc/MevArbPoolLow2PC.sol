// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title MevArbPoolLow2PC
 * @dev 低价池 A→B；初值与 `MevArbPoolLowMasterV2` 一致；公式同 `AmmPool2PC`。
 */
contract MevArbPoolLow2PC {
    uint256 public constant INITIAL_RESERVE_A = 10 ** 22;
    uint256 public constant INITIAL_RESERVE_B = 10 ** 24;

    uint256 public reserveA;
    uint256 public reserveB;
    bytes32 public lockedByTxId;

    struct PendingSwap {
        address user;
        uint256 amountInA;
        uint256 amountOutB;
        bool exists;
    }

    mapping(bytes32 => PendingSwap) public pendingSwaps;

    event PoolLowPrepared(bytes32 indexed txId, address indexed user, uint256 amountInA, uint256 amountOutB);
    event PoolLowCommitted(bytes32 indexed txId);
    event PoolLowAborted(bytes32 indexed txId);

    constructor() {
        reserveA = INITIAL_RESERVE_A;
        reserveB = INITIAL_RESERVE_B;
    }

    function setReserves(uint256 rA, uint256 rB) external {
        require(lockedByTxId == bytes32(0), "low: locked");
        require(rA > 0 && rB > 0, "low: reserves");
        reserveA = rA;
        reserveB = rB;
    }

    /// @return amountOutB 恒定乘积：outB = rb * dx / (ra + dx)
    function prepareSwapAToB(bytes32 txId, address user, uint256 amountInA, uint256 minOutB)
        external
        returns (uint256 amountOutB)
    {
        require(user != address(0), "low: user");
        require(amountInA > 0, "low: amountIn");
        require(!pendingSwaps[txId].exists, "low: prepared");
        require(lockedByTxId == bytes32(0) || lockedByTxId == txId, "low: lock");

        amountOutB = (reserveB * amountInA) / (reserveA + amountInA);
        require(amountOutB >= minOutB, "low: slippage");
        require(amountOutB > 0 && reserveB >= amountOutB, "low: liq");

        reserveB -= amountOutB;
        if (lockedByTxId == bytes32(0)) {
            lockedByTxId = txId;
        }
        pendingSwaps[txId] = PendingSwap({user: user, amountInA: amountInA, amountOutB: amountOutB, exists: true});
        emit PoolLowPrepared(txId, user, amountInA, amountOutB);
    }

    function commit(bytes32 txId) external {
        require(lockedByTxId == txId, "low: lock");
        PendingSwap memory p = pendingSwaps[txId];
        require(p.exists, "low: no prepare");
        reserveA += p.amountInA;
        delete pendingSwaps[txId];
        lockedByTxId = bytes32(0);
        emit PoolLowCommitted(txId);
    }

    function abort(bytes32 txId) external {
        PendingSwap memory p = pendingSwaps[txId];
        if (!p.exists) {
            return;
        }
        require(lockedByTxId == txId, "low: lock");
        reserveB += p.amountOutB;
        delete pendingSwaps[txId];
        lockedByTxId = bytes32(0);
        emit PoolLowAborted(txId);
    }
}
