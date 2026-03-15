// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title WalletMasterV2
 * @dev Master-side authoritative state (demo) + batch intent processing via JoyueCoordinatorV2.
 * 
 * 使用新的 CoordinatorV2，所有核心逻辑都通过预编译合约执行。
 */
contract WalletMasterV2 is JoyueCoordinatorV2 {
    function balanceKey(address user) external pure returns (bytes32) {
        return JoyueLib.keyOfAddr("wallet.balance:", user);
    }

    // convenience admin setter for testing
    function setBalance(address user, uint256 v) external {
        bytes32 key = JoyueLib.keyOfAddr("wallet.balance:", user);
        _setUint(key, v);
        // 发出 StateBroadcast 事件，让 Agent 可以读取缓存
        _broadcastState(key);
    }
}
