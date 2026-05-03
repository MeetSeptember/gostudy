// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title MevArbPoolHighMasterV2
 * @dev 高价侧池（相对多 A）：B→A 换出的 A 较多，便于套利路径第二步。
 */
contract MevArbPoolHighMasterV2 is JoyueCoordinatorV2 {
    bytes32 public constant RESERVE_A_KEY = keccak256(abi.encodePacked("mevarb.joyue.poolHigh.reserveA"));
    bytes32 public constant RESERVE_B_KEY = keccak256(abi.encodePacked("mevarb.joyue.poolHigh.reserveB"));

    uint256 public constant INITIAL_RESERVE_A = 10 ** 24;
    uint256 public constant INITIAL_RESERVE_B = 10 ** 22;

    constructor() {
        _setUint(RESERVE_A_KEY, INITIAL_RESERVE_A);
        _broadcastState(RESERVE_A_KEY);
        _setUint(RESERVE_B_KEY, INITIAL_RESERVE_B);
        _broadcastState(RESERVE_B_KEY);
    }

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
