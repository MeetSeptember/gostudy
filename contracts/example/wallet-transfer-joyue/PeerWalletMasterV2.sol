// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title PeerWalletMasterV2
 * @dev 转账示例专用 Master；初始无余额。部署后由 `cmd/bootstrap-wallet-balances-from-csv -kind joyue` 调 `setBalances` / `setBalance` 写入。
 *      单分片场景：每条链部署一份，由 PeerTransferAgentV2(walletMaster, shardId) 引用。
 */
contract PeerWalletMasterV2 is JoyueCoordinatorV2 {
    constructor() {}

    function balanceKey(address user) external pure returns (bytes32) {
        return JoyueLib.keyOfAddr("wallet.balance:", user);
    }

    /**
     * @dev RawRequest 占位：Guard/Delta 由同分片 PeerTransferAgentV2 生成。
     */
    function transferIntent(address from, address to, uint256 amount) external pure returns (address, address, uint256) {
        return (from, to, amount);
    }

    function setBalance(address user, uint256 v) external {
        require(user != address(0), "PWM: zero");
        bytes32 key = JoyueLib.keyOfAddr("wallet.balance:", user);
        _setUint(key, v);
        _broadcastState(key);
    }

    /// @dev 与 PeerTransferWallet2PC.setBalances 对称，供 bootstrap 分批灌入。
    function setBalances(address[] calldata users, uint256 v) external {
        for (uint256 i = 0; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "PWM: zero");
            bytes32 key = JoyueLib.keyOfAddr("wallet.balance:", u);
            _setUint(key, v);
            _broadcastState(key);
        }
    }
}
