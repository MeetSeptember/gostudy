// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "./FruitShopMasterV2.sol";
import "./WalletMasterV2.sol";
import "./PointsMasterV2.sol";

/**
 * @title FruitShopAgentV2
 * @dev Agent-side business logic (runs on cache). Generates Guards + Deltas + RawRequest (Intent).
 *
 * Cross-contract example:
 * - Read FruitShopMasterV2.stock from cache (via precompile 0x64)
 * - Read WalletMasterV2.balance from cache (via precompile 0x64)
 * - Read PointsMasterV2.points from cache (via precompile 0x64)
 * - Generate deltas across 3 master contracts
 *
 * NOTE: In JOYUE, this Agent would be deployed on non-master shards.
 * NOTE: Cache access is now done directly via precompile contracts, no need for cacheAddr.
 */
contract FruitShopAgentV2 {
    using JoyueLib for JoyueLib.JVar;
    using JoyueLib for JoyueLib.Context;
    using JoyueLib for JoyueLib.ExprBuilder;

    // Master contract addresses (authoritative targets)
    address public fruitShopMaster;
    address public walletMaster;
    address public pointsMaster;

    // 各 Master 所在的分片
    uint32 public fruitShopMasterShardID;
    uint32 public walletMasterShardID;
    uint32 public pointsMasterShardID;

    // Intent 结果存储：requestId (txId) => (success, data)
    struct IntentResult {
        bool success;
        bytes data;
        bool exists;
    }
    mapping(bytes32 => IntentResult) public intentResults;

    /// 已收到结果的所有 requestId 列表（用于遍历查询）
    bytes32[] private _requestIds;

    event IntentSent(bytes32 indexed txId, address indexed user, bytes32 fruitType, uint256 quantity);
    event IntentResultReceived(bytes32 indexed requestId, bool success);

    constructor(
        address fruitShopMasterAddr,
        address walletMasterAddr,
        address pointsMasterAddr,
        uint32 fruitShopMasterShardID_,
        uint32 walletMasterShardID_,
        uint32 pointsMasterShardID_
    ) {
        fruitShopMaster = fruitShopMasterAddr;
        walletMaster = walletMasterAddr;
        pointsMaster = pointsMasterAddr;
        fruitShopMasterShardID = fruitShopMasterShardID_;
        walletMasterShardID = walletMasterShardID_;
        pointsMasterShardID = pointsMasterShardID_;
    }

    /**
     * @dev 购买水果（支持字符串参数，用户友好）
     * @param fruitName 水果名称（如 "apple" 或 "banana"）
     * @param quantity 购买数量
     * @return ok 是否通过验证并发出 Intent
     * @return txId 交易标识符，用于后续 getIntentResult 查询结果
     */
    function buyFruit(string memory fruitName, uint256 quantity) external returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas) {
        bytes32 fruitType = keccak256(bytes(fruitName));
        return buyFruit(fruitType, quantity);
    }

    /**
     * @dev 购买水果（内部实现，使用 bytes32）
     * @param fruitType 水果类型（bytes32，通过 keccak256 计算）
     * @param quantity 购买数量
     */
    function buyFruit(bytes32 fruitType, uint256 quantity) public returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas) {
        // 构建交易上下文（自动生成唯一 txId，用于 D_SUB 冻结）
        JoyueLib.Context memory ctx = JoyueLib.newTransactionContext(
            address(0),
            fruitShopMaster,
            FruitShopMasterV2.buyFruit.selector,
            abi.encode(fruitType, quantity)
        );

        // 从缓存读取状态并验证
        if (!_loadAndValidateState(ctx, fruitType, quantity)) {
            return (false, bytes32(0), ctx.guards, ctx.deltas);
        }

        // 通过预编译合约发出跨分片 Intent 请求
        bool success = ctx.emitIntentViaPrecompile(fruitShopMasterShardID, fruitShopMaster);
        JoyueLib.emitIntent(ctx);

        // 发出事件，用户可监听 txId 用于后续查询结果
        emit IntentSent(ctx.txId, msg.sender, fruitType, quantity);

        return (true, ctx.txId, ctx.guards, ctx.deltas);
    }

    /**
     * @dev 从缓存加载状态并验证，生成 Deltas
     * @param ctx 上下文
     * @param fruitType 水果类型
     * @param quantity 购买数量
     * @return 是否验证通过
     */
    function _loadAndValidateState(JoyueLib.Context memory ctx, bytes32 fruitType, uint256 quantity) internal returns (bool) {
        return _loadAndValidateStateForUser(ctx, fruitType, quantity, msg.sender);
    }

    /// @dev 指定 user 的验证逻辑（供 recomputeIntent 使用）
    function _loadAndValidateStateForUser(JoyueLib.Context memory ctx, bytes32 fruitType, uint256 quantity, address user) internal returns (bool) {
        return _loadAndValidateStateForUserWithOverrides(ctx, fruitType, quantity, user, new JoyueLib.StateOverride[](0));
    }

    /// @dev 带 StateOverride 的验证逻辑：优先使用 callback 返回的权威最新状态
    function _loadAndValidateStateForUserWithOverrides(
        JoyueLib.Context memory ctx,
        bytes32 fruitType,
        uint256 quantity,
        address user,
        JoyueLib.StateOverride[] memory overrides
    ) internal returns (bool) {
        JoyueLib.JVar memory stock = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, fruitShopMaster, fruitShopMasterShardID, JoyueLib.keyOf2("fruit.stock:", fruitType), overrides);
        JoyueLib.JVar memory priceVar = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, fruitShopMaster, fruitShopMasterShardID, JoyueLib.keyOf2("fruit.price:", fruitType), overrides);
        JoyueLib.JVar memory pointsVar = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, fruitShopMaster, fruitShopMasterShardID, JoyueLib.keyOf2("fruit.points:", fruitType), overrides);
        uint256 price = JoyueLib.toUint(priceVar.value) * quantity;
        uint256 reward = JoyueLib.toUint(pointsVar.value) * quantity;
        JoyueLib.JVar memory balance = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, walletMaster, walletMasterShardID, JoyueLib.keyOfAddr("wallet.balance:", user), overrides);
        JoyueLib.JVar memory userPoints = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, pointsMaster, pointsMasterShardID, JoyueLib.keyOfAddr("points.user:", user), overrides);

        // Guard: 库存充足
        if (!stock.checkGte(ctx, quantity)) {
            return false;
        }

        // Guard: 余额充足
        if (!balance.checkGte(ctx, price)) {
            return false;
        }

        // 生成增量：扣库存、扣余额、加积分
        stock.sub(ctx, quantity);
        balance.sub(ctx, price);
        userPoints.add(ctx, reward);

        return true;
    }

    /**
     * @dev 根据 RawRequest 和 user 重新计算 guards 和 deltas（用于 retry）。
     *      Coordinator 在 retry 时调用。stateOverrides 为 callback 返回的权威最新状态，优先于缓存。
     * @param req 原始请求（targetAddr, selector, args）
     * @param user 用户地址（用于读取余额/积分）
     * @param stateOverrides callback 返回的 (contractAddr, shardId, key, value, version)，优先于缓存
     * @return guards 新的 guards
     * @return deltas 新的 deltas
     * @return ok 是否验证通过
     */
    function recomputeIntent(
        JoyueLib.RawRequest calldata req,
        address user,
        JoyueLib.StateOverride[] calldata stateOverrides
    ) external returns (JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas, bool ok) {
        if (req.targetAddr != fruitShopMaster || req.selector != FruitShopMasterV2.buyFruit.selector) {
            return (guards, deltas, false);
        }
        (bytes32 fruitType, uint256 quantity) = abi.decode(req.args, (bytes32, uint256));
        JoyueLib.Context memory ctx = JoyueLib.newContext(
            address(0),
            fruitShopMaster,
            req.selector,
            req.args
        );
        ok = _loadAndValidateStateForUserWithOverrides(ctx, fruitType, quantity, user, stateOverrides);
        return (ctx.guards, ctx.deltas, ok);
    }

    // ============================================================
    //                      Debug helpers
    // ============================================================

    /// @dev 调试：返回当前节点 ShardID（调用预编译 0x6C）。用于验证 Agent 所在分片。
    /// 若返回 0，说明节点 evm.Context.ShardID=0（可能是单节点或配置为 shard 0）
    function debugGetCurrentShardID() external view returns (uint32) {
        (bool success, bytes memory result) = JoyueLib.PRECOMPILE_CURRENT_SHARD.staticcall(new bytes(0));
        if (success && result.length >= 32) {
            uint32 shardID;
            assembly {
                shardID := and(mload(add(result, 0x20)), 0xffffffff)
            }
            return shardID;
        }
        return 0;
    }

    /// @dev 计算 fruitType（与 Master 的 APPLE/BANANA 常量一致：keccak256("apple")）
    function fruitTypeOf(string memory fruitName) external pure returns (bytes32) {
        return keccak256(bytes(fruitName));
    }

    /// @dev 仅用于调试：返回一条缓存记录（key/value/version/ok）
    function _debugLoad(address contractAddr, uint32 shardId, bytes32 key)
        internal
        view
        returns (bytes32 outKey, uint256 value, uint64 version, bool ok)
    {
        JoyueLib.Context memory ctx = JoyueLib.newContext(address(0), address(0), bytes4(0), new bytes(0));
        JoyueLib.JVar memory v = JoyueLib.loadFromCacheByKey(ctx, contractAddr, shardId, key);
        return (key, JoyueLib.toUint(v.value), v.version, v.loaded);
    }

    // ---------- FruitShopMasterV2 cache ----------

    function debugMasterStock(string memory fruitName)
        external
        view
        returns (bytes32 key, uint256 value, uint64 version, bool ok)
    {
        bytes32 fruitType = keccak256(bytes(fruitName));
        return _debugLoad(fruitShopMaster, fruitShopMasterShardID, JoyueLib.keyOf2("fruit.stock:", fruitType));
    }

    function debugMasterPrice(string memory fruitName)
        external
        view
        returns (bytes32 key, uint256 value, uint64 version, bool ok)
    {
        bytes32 fruitType = keccak256(bytes(fruitName));
        return _debugLoad(fruitShopMaster, fruitShopMasterShardID, JoyueLib.keyOf2("fruit.price:", fruitType));
    }

    function debugMasterPoints(string memory fruitName)
        external
        view
        returns (bytes32 key, uint256 value, uint64 version, bool ok)
    {
        bytes32 fruitType = keccak256(bytes(fruitName));
        return _debugLoad(fruitShopMaster, fruitShopMasterShardID, JoyueLib.keyOf2("fruit.points:", fruitType));
    }

    // ---------- WalletMasterV2 / PointsMasterV2 cache ----------

    function debugWalletBalance(address user)
        external
        view
        returns (bytes32 key, uint256 value, uint64 version, bool ok)
    {
        return _debugLoad(walletMaster, walletMasterShardID, JoyueLib.keyOfAddr("wallet.balance:", user));
    }

    function debugUserPoints(address user)
        external
        view
        returns (bytes32 key, uint256 value, uint64 version, bool ok)
    {
        return _debugLoad(pointsMaster, pointsMasterShardID, JoyueLib.keyOfAddr("points.user:", user));
    }

    // ============================================================
    //              Intent 结果回调与查询
    // ============================================================

    /**
     * @dev 由 Coordinator 通过跨分片调用，通知 Intent 执行结果
     * @param requestId 即 buyFruit 返回的 txId
     * @param success 是否成功提交
     * @param data 附加数据（可为空）
     */
    function onIntentResult(bytes32 requestId, bool success, bytes memory data) external {
        if (intentResults[requestId].exists) return; // 避免重复回调导致 _requestIds 重复
        intentResults[requestId] = IntentResult({
            success: success,
            data: data,
            exists: true
        });
        _requestIds.push(requestId);
        emit IntentResultReceived(requestId, success);
    }

    /**
     * @dev 用户查询 Intent 执行结果
     * @param requestId 即 buyFruit 返回的 txId
     * @return success 是否成功
     * @return data 附加数据
     * @return exists 是否已收到结果
     */
    function getIntentResult(bytes32 requestId) external view returns (bool success, bytes memory data, bool exists) {
        IntentResult memory r = intentResults[requestId];
        return (r.success, r.data, r.exists);
    }

    /**
     * @dev 返回已收到结果的数量
     */
    function getIntentResultCount() external view returns (uint256) {
        return _requestIds.length;
    }

    /**
     * @dev 查询所有 Intent 结果（分页，避免 gas 超限）
     * @param offset 起始索引
     * @param limit 最多返回条数
     * @return requestIds 对应的 txId 列表
     * @return successes 是否成功
     * @return datas 附加数据
     * @return exists 是否已存在（均为 true）
     */
    function getIntentResults(uint256 offset, uint256 limit)
        external
        view
        returns (
            bytes32[] memory requestIds,
            bool[] memory successes,
            bytes[] memory datas,
            bool[] memory exists
        )
    {
        uint256 total = _requestIds.length;
        if (offset >= total) {
            return (new bytes32[](0), new bool[](0), new bytes[](0), new bool[](0));
        }
        uint256 end = offset + limit;
        if (end > total) end = total;
        uint256 n = end - offset;

        requestIds = new bytes32[](n);
        successes = new bool[](n);
        datas = new bytes[](n);
        exists = new bool[](n);

        for (uint256 i = 0; i < n; i++) {
            bytes32 id = _requestIds[offset + i];
            IntentResult memory r = intentResults[id];
            requestIds[i] = id;
            successes[i] = r.success;
            datas[i] = r.data;
            exists[i] = r.exists;
        }
    }

    /**
     * @dev 查询所有 Intent 结果（便捷方法，等价于 getIntentResults(0, count)）
     * 注意：结果较多时可能 gas 超限，建议用 getIntentResults(offset, limit) 分页
     */
    function getAllIntentResults()
        external
        view
        returns (
            bytes32[] memory requestIds,
            bool[] memory successes,
            bytes[] memory datas,
            bool[] memory exists
        )
    {
        (requestIds, successes, datas, exists) = this.getIntentResults(0, _requestIds.length);
    }

//    // Optional: complex expr example (points + balance > 1000) to show RPN builder
//    function isVipPreview(address user, uint256 assumedBalance, uint256 assumedPoints) external returns (bool isVip, JoyueLib.Guard[] memory guards) {
//        // 注意：cacheAddr 传入 address(0)，因为现在直接使用预编译合约读取缓存
//        JoyueLib.Context memory ctx = JoyueLib.newContext(
//            address(0),  // cacheAddr 不再需要，loadFromCacheByKey 直接使用预编译合约
//            fruitShopMaster,
//            bytes4(0), // no master method for this preview
//            abi.encode(user)
//        );
//
//        // 注意：key 的计算在 Agent 中直接完成，不需要跨分片调用 pure 函数
//        bytes32 balanceKey = JoyueLib.keyOfAddr("wallet.balance:", user);
//        bytes32 userPointsKey = JoyueLib.keyOfAddr("points.user:", user);
//        JoyueLib.JVar memory balance = JoyueLib.loadFromCacheByKey(ctx, walletMaster, balanceKey);
//        JoyueLib.JVar memory points = JoyueLib.loadFromCacheByKey(ctx, pointsMaster, userPointsKey);
//
//        // For demo: override eval by supplying assumed values as ARG tokens (real system can read args)
//        bool vip = ctx.expr()
//            .pushVar(points)
//            .pushVar(balance)
//            .opAdd()
//            .checkExprGt(ctx, 1000);
//
//        // NOTE: this emits OP_EXPR guard inside ctx.guards
//        JoyueLib.emitIntent(ctx);
//        return (vip, ctx.guards);
//    }

}

