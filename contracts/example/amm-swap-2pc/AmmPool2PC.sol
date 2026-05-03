// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title AmmPool2PC
 * @dev 2PC Participant：恒定乘积 x*y=k，仅池内 reserveA/reserveB。
 *      prepare：按当前储备算出 amountOut，从 reserveB 扣除并记入 pending（锁定输出侧）；
 *      commit：reserveA += amountIn（与 WalletA 扣款语义对齐）；
 *      abort：恢复 reserveB。
 *
 *      与 JOYUE AmmPoolMasterV2 同公式、同初始储备：amountOut = reserveB * amountIn / (reserveA + amountIn)
 */
contract AmmPool2PC {
    /// @dev 与 `AmmPoolMasterV2.INITIAL_RESERVE` 一致
    uint256 public constant INITIAL_RESERVE = 10 ** 24;

    uint256 public reserveA;
    uint256 public reserveB;

    /// @dev 与 twophase Wallet2PC 类似：单笔 2PC 独占池
    bytes32 public lockedByTxId;

    struct PendingSwap {
        address user;
        uint256 amountIn;
        uint256 amountOut;
        bool exists;
    }

    mapping(bytes32 => PendingSwap) public pendingSwaps;

    event PoolPreparedSwap(bytes32 indexed txId, address indexed user, uint256 amountIn, uint256 amountOut);
    event PoolCommitted(bytes32 indexed txId);
    event PoolAborted(bytes32 indexed txId);

    constructor() {
        reserveA = INITIAL_RESERVE;
        reserveB = INITIAL_RESERVE;
    }

    /// @dev 压测用：与 JOYUE 池子同量级时可调
    function setReserves(uint256 rA, uint256 rB) external {
        require(lockedByTxId == bytes32(0), "pool: locked");
        require(rA > 0 && rB > 0, "pool: reserves");
        reserveA = rA;
        reserveB = rB;
    }

    /**
     * @return amountOut 供 Executor 回调 returnData 解码；协调者据此调 WalletB prepareCredit
     */
    function prepareSwap(bytes32 txId, address user, uint256 amountIn, uint256 minAmountOut)
        external
        returns (uint256 amountOut)
    {
        require(user != address(0), "pool: user");
        require(amountIn > 0, "pool: amountIn");
        require(!pendingSwaps[txId].exists, "pool: prepared");
        require(lockedByTxId == bytes32(0) || lockedByTxId == txId, "pool: locked");

        amountOut = (reserveB * amountIn) / (reserveA + amountIn);
        require(amountOut >= minAmountOut, "pool: slippage");
        require(amountOut > 0 && reserveB >= amountOut, "pool: liquidity");

        reserveB -= amountOut;
        if (lockedByTxId == bytes32(0)) {
            lockedByTxId = txId;
        }

        pendingSwaps[txId] = PendingSwap({user: user, amountIn: amountIn, amountOut: amountOut, exists: true});
        emit PoolPreparedSwap(txId, user, amountIn, amountOut);
    }

    function preparedAmountOut(bytes32 txId) external view returns (uint256) {
        PendingSwap memory p = pendingSwaps[txId];
        require(p.exists, "pool: no pending");
        return p.amountOut;
    }

    function commit(bytes32 txId) external {
        require(lockedByTxId == txId, "pool: lock");
        PendingSwap memory p = pendingSwaps[txId];
        require(p.exists, "pool: no prepare");

        reserveA += p.amountIn;
        delete pendingSwaps[txId];
        lockedByTxId = bytes32(0);
        emit PoolCommitted(txId);
    }

    function abort(bytes32 txId) external {
        PendingSwap memory p = pendingSwaps[txId];
        if (!p.exists) {
            return;
        }
        require(lockedByTxId == txId, "pool: lock");
        reserveB += p.amountOut;
        delete pendingSwaps[txId];
        lockedByTxId = bytes32(0);
        emit PoolAborted(txId);
    }
}
