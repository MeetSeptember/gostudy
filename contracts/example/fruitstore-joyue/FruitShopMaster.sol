// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinator.sol";

/**
 * @title FruitShopMaster
 * @dev Master-side authoritative state (demo) + batch intent processing via JoyueMaster.
 */
contract FruitShopMaster is JoyueCoordinator {
    bytes32 public constant APPLE = keccak256("apple");
    bytes32 public constant BANANA = keccak256("banana");

    constructor() {
        // demo initial stocks
        _setUint(JoyueLib.keyOf2("fruit.stock:", APPLE), 10);
        _setUint(JoyueLib.keyOf2("fruit.stock:", BANANA), 8);
    }

    function stockKey(bytes32 fruitType) external pure returns (bytes32) {
        return JoyueLib.keyOf2("fruit.stock:", fruitType);
    }

    // Example business entry signature (Master will later implement processIntent).
    function buyFruit(bytes32 fruitType, uint256 quantity) external pure returns (bytes32, uint256) {
        // placeholder (master does not execute business here in this example)
        return (fruitType, quantity);
    }

    // convenience admin setter for testing
    function setStock(bytes32 fruitType, uint256 v) external {
        _setUint(JoyueLib.keyOf2("fruit.stock:", fruitType), v);
    }
}