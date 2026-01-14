// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinator.sol";

/**
 * @title WalletMaster
 * @dev Master-side authoritative state (demo) + batch intent processing via JoyueMaster.
 */
contract WalletMaster is JoyueCoordinator {
    function balanceKey(address user) external pure returns (bytes32) {
        return JoyueLib.keyOfAddr("wallet.balance:", user);
    }

    // convenience admin setter for testing
    function setBalance(address user, uint256 v) external {
        _setUint(JoyueLib.keyOfAddr("wallet.balance:", user), v);
    }
}