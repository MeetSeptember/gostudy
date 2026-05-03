// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title MevArbProfitWalletMasterV2
 * @dev 用户套利净利润（A 单位）累计；key 前缀 `mevarb.joyue.profit.balance:`。
 *      不模拟中间 A/B 持仓，仅在意图 Delta 中增加 `outHigh - borrowA`。
 *      `setBalance` / `setBalances` 与 erc20 bootstrap 同 ABI（任意非零用户）。
 */
contract MevArbProfitWalletMasterV2 is JoyueCoordinatorV2 {
    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return JoyueLib.keyOfAddr("mevarb.joyue.profit.balance:", user);
    }

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "MevArb: zero user");
        bytes32 key = JoyueLib.keyOfAddr("mevarb.joyue.profit.balance:", user);
        _setUint(key, v);
        _broadcastState(key);
    }

    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "MevArb: zero user");
            bytes32 key = JoyueLib.keyOfAddr("mevarb.joyue.profit.balance:", u);
            _setUint(key, v);
            _broadcastState(key);
        }
    }
}
