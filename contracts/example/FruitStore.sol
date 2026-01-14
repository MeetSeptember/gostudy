// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "../baselib/JoyueCrossShardCall.sol";
import "./JoyueCrossShardAggregator.sol";

/**
 * @title FruitStore
 * @dev 水果商店合约示例：跨分片调用积分合约和货币合约
 * 
 * 业务场景：
 * - 用户购买水果时，需要扣除货币（跨分片调用 CurrencyContract）
 * - 同时需要增加积分（跨分片调用 PointsContract）
 * - 需要等待两个操作都完成后，才确认订单完成
 * 
 * 使用聚合请求管理器来等待多个跨分片回调完成
 */
contract FruitStore is JoyueCrossShardAggregator {
    using JoyueCrossShardCall for address;

    // 水果信息
    struct Fruit {
        string name;
        uint256 price;      // 价格（货币单位）
        uint256 points;     // 购买后获得的积分
        uint256 stock;      // 库存
        bool exists;
    }

    // 订单信息
    struct Order {
        address buyer;
        string fruitName;
        uint256 quantity;
        uint256 totalPrice;
        uint256 totalPoints;
        uint8 status;      // 0=pending, 1=completed, 2=failed
        uint64 createdAt;
    }

    mapping(string => Fruit) public fruits;
    mapping(uint256 => Order) public orders; // orderId -> Order
    mapping(uint256 => uint256) public aggregatedRequestIdToOrderId; // aggregatedRequestId -> orderId
    uint256 public nextOrderId;

    // 跨分片合约地址配置
    address public currencyContract;      // 货币合约地址
    uint32 public currencyShardID;         // 货币合约所在分片
    address public currencyExecutor;       // 货币合约所在分片的 executor

    address public pointsContract;         // 积分合约地址
    uint32 public pointsShardID;           // 积分合约所在分片
    address public pointsExecutor;         // 积分合约所在分片的 executor

    event FruitAdded(string name, uint256 price, uint256 points, uint256 stock);
    event OrderCreated(uint256 indexed orderId, address indexed buyer, string fruitName, uint256 quantity);
    event OrderCompleted(uint256 indexed orderId, bool success);
    event OrderFailed(uint256 indexed orderId, string reason);

    /**
     * @dev 构造函数：设置跨分片合约地址
     */
    constructor(
        address _currencyContract,
        uint32 _currencyShardID,
        address _currencyExecutor,
        address _pointsContract,
        uint32 _pointsShardID,
        address _pointsExecutor
    ) {
        currencyContract = _currencyContract;
        currencyShardID = _currencyShardID;
        currencyExecutor = _currencyExecutor;
        pointsContract = _pointsContract;
        pointsShardID = _pointsShardID;
        pointsExecutor = _pointsExecutor;
    }

    /**
     * @dev 添加水果
     */
    function addFruit(
        string memory name,
        uint256 price,
        uint256 points,
        uint256 stock
    ) external {
        require(!fruits[name].exists, "fruit already exists");
        fruits[name] = Fruit({
            name: name,
            price: price,
            points: points,
            stock: stock,
            exists: true
        });
        emit FruitAdded(name, price, points, stock);
    }

    /**
     * @dev 购买水果（跨分片调用货币和积分合约）
     * @param fruitName 水果名称
     * @param quantity 购买数量
     */
    function buyFruit(string memory fruitName, uint256 quantity) external returns (uint256 orderId) {
        Fruit storage fruit = fruits[fruitName];
        require(fruit.exists, "fruit not found");
        require(fruit.stock >= quantity, "insufficient stock");

        uint256 totalPrice = fruit.price * quantity;
        uint256 totalPoints = fruit.points * quantity;

        // 生成订单 ID（使用聚合请求 ID）
        orderId = ++nextOrderId;

        // 保存订单信息
        Order storage order = orders[orderId];
        order.buyer = msg.sender;
        order.fruitName = fruitName;
        order.quantity = quantity;
        order.totalPrice = totalPrice;
        order.totalPoints = totalPoints;
        order.status = 0; // pending
        order.createdAt = uint64(block.timestamp);

        // 减少库存（先减少，如果后续失败再恢复）
        fruit.stock -= quantity;

        emit OrderCreated(orderId, msg.sender, fruitName, quantity);

        // 构造两个跨分片请求
        address[] memory executors = new address[](2);
        uint32[] memory targetShardIDs = new uint32[](2);
        address[] memory targets = new address[](2);
        bytes[] memory targetCalldatas = new bytes[](2);
        uint256[] memory targetValues = new uint256[](2);

        // 请求 0：扣除货币
        executors[0] = currencyExecutor;
        targetShardIDs[0] = currencyShardID;
        targets[0] = currencyContract;
        targetCalldatas[0] = abi.encodeWithSignature(
            "deduct(address,uint256,string)",
            msg.sender,
            totalPrice,
            string(abi.encodePacked("buy", fruitName))
        );
        targetValues[0] = 0;

        // 请求 1：增加积分
        executors[1] = pointsExecutor;
        targetShardIDs[1] = pointsShardID;
        targets[1] = pointsContract;
        targetCalldatas[1] = abi.encodeWithSignature(
            "addPoints(address,uint256,string)",
            msg.sender,
            totalPoints,
            string(abi.encodePacked("buy", fruitName))
        );
        targetValues[1] = 0;

        // 发起聚合请求（串行发送，避免并发数据库访问问题）
        uint256 aggregatedRequestId;
        (aggregatedRequestId, ) = sendAggregatedRequest(
            executors,
            targetShardIDs,
            targets,
            targetCalldatas,
            targetValues
        );

        // 将聚合请求 ID 映射到订单 ID
        aggregatedRequestIdToOrderId[aggregatedRequestId] = orderId;

        return orderId;
    }

    /**
     * @dev 重写聚合回调完成处理函数
     */
    function onAllCallbacksCompleted(uint256 aggregatedRequestId, AggregatedRequest storage aggReq) internal override {
        // 通过映射查找对应的订单 ID
        uint256 orderId = aggregatedRequestIdToOrderId[aggregatedRequestId];
        require(orderId != 0, "order not found for aggregated request");

        Order storage order = orders[orderId];

        require(order.buyer != address(0), "order not found");
        require(order.status == 0, "order already processed");

        // 检查所有请求是否都成功
        if (aggReq.successCount == aggReq.totalRequests && aggReq.failedCount == 0) {
            // 所有操作都成功，订单完成
            order.status = 1; // completed
            emit OrderCompleted(orderId, true);
        } else {
            // 有操作失败，订单失败，恢复库存
            order.status = 2; // failed
            fruits[order.fruitName].stock += order.quantity; // 恢复库存
            emit OrderFailed(orderId, "cross-shard operations failed");
        }
    }

    /**
     * @dev 查询订单状态
     */
    function getOrder(uint256 orderId) external view returns (
        address buyer,
        string memory fruitName,
        uint256 quantity,
        uint256 totalPrice,
        uint256 totalPoints,
        uint8 status,
        uint64 createdAt
    ) {
        Order storage order = orders[orderId];
        require(order.buyer != address(0), "order not found");
        return (
            order.buyer,
            order.fruitName,
            order.quantity,
            order.totalPrice,
            order.totalPoints,
            order.status,
            order.createdAt
        );
    }

    /**
     * @dev 查询水果信息
     */
    function getFruit(string memory name) external view returns (
        string memory fruitName,
        uint256 price,
        uint256 points,
        uint256 stock,
        bool exists
    ) {
        Fruit storage fruit = fruits[name];
        return (
            fruit.name,
            fruit.price,
            fruit.points,
            fruit.stock,
            fruit.exists
        );
    }
}

