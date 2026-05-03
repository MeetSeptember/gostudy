// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title NftJoyueWalletMasterV2
 * @dev 购买者支付余额；key 前缀 `nft.joyue.wallet.balance:`。部署后由 `bootstrap-wallet-balances-from-csv -kind nft-joyue-wallet` 灌入。
 */
contract NftJoyueWalletMasterV2 is JoyueCoordinatorV2 {
    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return JoyueLib.keyOfAddr("nft.joyue.wallet.balance:", user);
    }

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "NftJoyue: zero");
        bytes32 key = JoyueLib.keyOfAddr("nft.joyue.wallet.balance:", user);
        _setUint(key, v);
        _broadcastState(key);
    }

    /// @dev 与 PeerWalletMasterV2.setBalances 对称，供 bootstrap 分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "NftJoyue: zero");
            bytes32 key = JoyueLib.keyOfAddr("nft.joyue.wallet.balance:", u);
            _setUint(key, v);
            _broadcastState(key);
        }
    }
}
