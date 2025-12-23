// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../JoyueLib.sol";
import "../JoyueCoordinator.sol";

/**
 * @title PointsMaster
 * @dev Master-side authoritative state (demo) + batch intent processing via JoyueMaster.
 */
contract PointsMaster is JoyueCoordinator {
    function pointsKey(address user) external pure returns (bytes32) {
        return JoyueLib.keyOfAddr("points.user:", user);
    }

    // convenience admin setter for testing
    function setPoints(address user, uint256 v) external {
        _setUint(JoyueLib.keyOfAddr("points.user:", user), v);
    }
}


