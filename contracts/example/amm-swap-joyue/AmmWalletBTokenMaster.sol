// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title AmmWalletBTokenMaster
 * @dev 用户接收侧代币 B 余额；可选 `bootstrap-wallet-balances-from-csv -kind amm-joyue-wallet-b`；swap 成功后会加款。
 */
contract AmmWalletBTokenMaster is JoyueCoordinatorV2 {
    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return JoyueLib.keyOfAddr("amm.joyue.walletB.balance:", user);
    }

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "AMM: zero");
        bytes32 key = JoyueLib.keyOfAddr("amm.joyue.walletB.balance:", user);
        _setUint(key, v);
        _broadcastState(key);
    }

    /// @dev 与 WalletA / erc20 bootstrap 对称，供链下分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "AMM: zero");
            bytes32 key = JoyueLib.keyOfAddr("amm.joyue.walletB.balance:", u);
            _setUint(key, v);
            _broadcastState(key);
        }
    }
}
