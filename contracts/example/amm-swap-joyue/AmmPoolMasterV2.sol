// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title AmmPoolMasterV2
 * @dev JOYUE 简化 AMM：仅维护池内两种代币储备与版本；无常量乘积公式由 Agent 计算并编码为 Guard/Delta。
 *      储备 key 的 Guard 使用 STRICT（由 Agent 对读到的版本生成 checkEq）。
 */
contract AmmPoolMasterV2 is JoyueCoordinatorV2 {
    bytes32 public constant RESERVE_A_KEY = keccak256(abi.encodePacked("amm.joyue.pool.reserveA"));
    bytes32 public constant RESERVE_B_KEY = keccak256(abi.encodePacked("amm.joyue.pool.reserveB"));

    uint256 public constant INITIAL_RESERVE = 10 ** 24;

    constructor() {
        _setUint(RESERVE_A_KEY, INITIAL_RESERVE);
        _broadcastState(RESERVE_A_KEY);
        _setUint(RESERVE_B_KEY, INITIAL_RESERVE);
        _broadcastState(RESERVE_B_KEY);
    }

    /**
     * @dev RawRequest 占位：由 AmmSwapAgentV2 生成 Guard/Delta；参数与 Agent swap 入口一致。
     */
    function swapIntent(address user, uint256 amountIn, uint256 minAmountOut) external pure returns (address, uint256, uint256) {
        return (user, amountIn, minAmountOut);
    }

    function setReserveA(uint256 v) external {
        _setUint(RESERVE_A_KEY, v);
        _broadcastState(RESERVE_A_KEY);
    }

    function setReserveB(uint256 v) external {
        _setUint(RESERVE_B_KEY, v);
        _broadcastState(RESERVE_B_KEY);
    }
}
