// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title FruitShopMasterV2
 * @dev Master-side authoritative state (demo) + batch intent processing via JoyueCoordinatorV2.
 * 
 * 使用新的 CoordinatorV2，所有核心逻辑都通过预编译合约执行。
 */
contract FruitShopMasterV2 is JoyueCoordinatorV2 {
    bytes32 public constant APPLE = keccak256("apple");
    bytes32 public constant BANANA = keccak256("banana");

    constructor() {
        // demo initial stocks
        bytes32 appleStockKey = JoyueLib.keyOf2("fruit.stock:", APPLE);
        bytes32 bananaStockKey = JoyueLib.keyOf2("fruit.stock:", BANANA);
        
        _setUint(appleStockKey, 20);
        _setUint(bananaStockKey, 8);
        
        // 发出 StateBroadcast 事件，让 Agent 可以读取缓存
        _broadcastState(appleStockKey);
        _broadcastState(bananaStockKey);

        // 设置价格和积分（主合约的权威状态）
        bytes32 applePriceKey = priceKey(APPLE);
        bytes32 bananaPriceKey = priceKey(BANANA);
        bytes32 applePointsKey = pointsKey(APPLE);
        bytes32 bananaPointsKey = pointsKey(BANANA);
        
        _setUint(applePriceKey, 20);   // APPLE_PRICE = 20
        _setUint(bananaPriceKey, 10);  // BANANA_PRICE = 10
        _setUint(applePointsKey, 5);   // APPLE_POINTS = 5
        _setUint(bananaPointsKey, 1);  // BANANA_POINTS = 1
        
        // 发出 StateBroadcast 事件，让 Agent 可以读取缓存
        _broadcastState(applePriceKey);
        _broadcastState(bananaPriceKey);
        _broadcastState(applePointsKey);
        _broadcastState(bananaPointsKey);
    }

    function stockKey(bytes32 fruitType) external pure returns (bytes32) {
        return JoyueLib.keyOf2("fruit.stock:", fruitType);
    }

    function priceKey(bytes32 fruitType) public pure returns (bytes32) {
        return JoyueLib.keyOf2("fruit.price:", fruitType);
    }

    function pointsKey(bytes32 fruitType) public pure returns (bytes32) {
        return JoyueLib.keyOf2("fruit.points:", fruitType);
    }

    // Example business entry signature (Master will later implement processIntent).
    function buyFruit(bytes32 fruitType, uint256 quantity) external pure returns (bytes32, uint256) {
        // placeholder (master does not execute business here in this example)
        return (fruitType, quantity);
    }

    // convenience admin setters for testing
    function setStock(bytes32 fruitType, uint256 v) external {
        bytes32 key = JoyueLib.keyOf2("fruit.stock:", fruitType);
        _setUint(key, v);
        _broadcastState(key);
    }

    function setPrice(bytes32 fruitType, uint256 v) external {
        bytes32 key = priceKey(fruitType);
        _setUint(key, v);
        _broadcastState(key);
    }

    function setPoints(bytes32 fruitType, uint256 v) external {
        bytes32 key = pointsKey(fruitType);
        _setUint(key, v);
        _broadcastState(key);
    }
}

