// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title MevArbFlashLenderMasterV2
 * @dev **独立贷款池 Master**：仅维护 `mevarb.joyue.flash.available`，不承担套利编排、不含 RawRequest 占位。
 *      借/还由 `MevArbBotAgentV2` 在意图 Delta 中读写本合约状态；与 `MevArbBotMasterV2` 分离。
 */
contract MevArbFlashLenderMasterV2 is JoyueCoordinatorV2 {
    bytes32 public constant FLASH_AVAILABLE_KEY = keccak256(abi.encodePacked("mevarb.joyue.flash.available"));

    uint256 public constant INITIAL_AVAILABLE = 10 ** 30;

    constructor() {
        _setUint(FLASH_AVAILABLE_KEY, INITIAL_AVAILABLE);
        _broadcastState(FLASH_AVAILABLE_KEY);
    }

    function setAvailable(uint256 v) external {
        _setUint(FLASH_AVAILABLE_KEY, v);
        _broadcastState(FLASH_AVAILABLE_KEY);
    }
}
