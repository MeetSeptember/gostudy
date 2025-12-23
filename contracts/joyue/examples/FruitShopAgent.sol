// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../JoyueLib.sol";
import "../JoyueMockCache.sol";
import "./FruitShopMaster.sol";
import "./WalletMaster.sol";
import "./PointsMaster.sol";

/**
 * @title FruitShopAgent
 * @dev Agent-side business logic (runs on cache). Generates Guards + Deltas + RawRequest (Intent).
 *
 * Cross-contract example:
 * - Read FruitShopMaster.stock from cache
 * - Read WalletMaster.balance from cache
 * - Read PointsMaster.points from cache
 * - Generate deltas across 3 master contracts
 *
 * NOTE: In JOYUE, this Agent would be deployed on non-master shards.
 */
contract FruitShopAgent {
    using JoyueLib for JoyueLib.JVar;
    using JoyueLib for JoyueLib.Context;
    using JoyueLib for JoyueLib.ExprBuilder;

    // P2P cache contract address (mock for now)
    address public cache;

    // Master contract addresses (authoritative targets)
    address public fruitShopMaster;
    address public walletMaster;
    address public pointsMaster;

    // Pricing constants (example)
    uint256 public constant APPLE_PRICE = 20;
    uint256 public constant BANANA_PRICE = 10;
    uint256 public constant APPLE_POINTS = 5;
    uint256 public constant BANANA_POINTS = 1;

    constructor(address cacheAddr, address fruitShopMasterAddr, address walletMasterAddr, address pointsMasterAddr) {
        cache = cacheAddr;
        fruitShopMaster = fruitShopMasterAddr;
        walletMaster = walletMasterAddr;
        pointsMaster = pointsMasterAddr;
    }

    function buyFruit(bytes32 fruitType, uint256 quantity) external returns (bool ok, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas) {
        // 构建意图上下文，目标为 Master 入口（用于逻辑树重试）
        JoyueLib.Context memory ctx = JoyueLib.newContext(
            cache,
            fruitShopMaster,
            FruitShopMaster.buyFruit.selector,
            abi.encode(fruitType, quantity)
        );

        uint256 price = _getPrice(fruitType) * quantity;
        uint256 reward = _getPoints(fruitType) * quantity;

        // 从 P2P 缓存读取主合约状态
        JoyueLib.JVar memory stock = JoyueLib.loadFromCacheByKey(
            ctx,
            fruitShopMaster,
            FruitShopMaster(fruitShopMaster).stockKey(fruitType)
        );
        JoyueLib.JVar memory balance = JoyueLib.loadFromCacheByKey(
            ctx,
            walletMaster,
            WalletMaster(walletMaster).balanceKey(msg.sender)
        );
        JoyueLib.JVar memory points = JoyueLib.loadFromCacheByKey(
            ctx,
            pointsMaster,
            PointsMaster(pointsMaster).pointsKey(msg.sender)
        );

        // Guard: 库存充足
        if (!stock.checkGte(ctx, quantity)) {
            return (false, ctx.guards, ctx.deltas);
        }

        // Guard: 余额充足
        if (!balance.checkGte(ctx, price)) {
            return (false, ctx.guards, ctx.deltas);
        }

        // 生成增量：扣库存、扣余额、加积分
        stock.sub(ctx, quantity);
        balance.sub(ctx, price);
        points.add(ctx, reward);

        JoyueLib.emitIntent(ctx);
        return (true, ctx.guards, ctx.deltas);
    }

    // Optional: complex expr example (points + balance > 1000) to show RPN builder
    function isVipPreview(address user, uint256 assumedBalance, uint256 assumedPoints) external returns (bool isVip, JoyueLib.Guard[] memory guards) {
        JoyueLib.Context memory ctx = JoyueLib.newContext(
            cache,
            fruitShopMaster,
            bytes4(0), // no master method for this preview
            abi.encode(user)
        );

        JoyueLib.JVar memory balance = JoyueLib.loadFromCacheByKey(ctx, walletMaster, WalletMaster(walletMaster).balanceKey(user));
        JoyueLib.JVar memory points = JoyueLib.loadFromCacheByKey(ctx, pointsMaster, PointsMaster(pointsMaster).pointsKey(user));

        // For demo: override eval by supplying assumed values as ARG tokens (real system can read args)
        bool vip = ctx.expr()
            .pushVar(points)
            .pushVar(balance)
            .opAdd()
            .checkExprGt(ctx, 1000);

        // NOTE: this emits OP_EXPR guard inside ctx.guards
        JoyueLib.emitIntent(ctx);
        return (vip, ctx.guards);
    }

    function _getPrice(bytes32 fruitType) internal view returns (uint256) {
        bytes32 apple = FruitShopMaster(fruitShopMaster).APPLE();
        bytes32 banana = FruitShopMaster(fruitShopMaster).BANANA();
        if (fruitType == apple) return APPLE_PRICE;
        if (fruitType == banana) return BANANA_PRICE;
        return 0;
    }

    function _getPoints(bytes32 fruitType) internal view returns (uint256) {
        bytes32 apple = FruitShopMaster(fruitShopMaster).APPLE();
        bytes32 banana = FruitShopMaster(fruitShopMaster).BANANA();
        if (fruitType == apple) return APPLE_POINTS;
        if (fruitType == banana) return BANANA_POINTS;
        return 0;
    }
}


