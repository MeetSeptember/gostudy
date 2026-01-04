// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22;

// 缓存接口
interface IJoyueCache {
    function get(address contractAddr, bytes32 key) external view returns (bytes memory value, uint64 version, bool ok);
}

interface IJoyueCacheWriter {
    function setUint(address contractAddr, bytes32 key, uint256 value, uint64 version) external;
}

// RPC Oracle 接口
interface IJoyueRpcOracle {
    function getUint(uint32 shardID, address contractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok);
}

/**
 * @title JoyueAgent（第一阶段：最小代理合约）
 * @notice 最小 Agent 合约：只存一些路由元信息，证明"该分片的 agent 已部署"。
 *
 * 说明：
 * - 第一阶段不要求 agent 地址一致，也不做跨分片合约调用。
 * - 后续第二阶段才会把"代理合约执行意图 + 主合约权威结算"等逻辑接上来。
 * - 添加了缓存和 RPC 测试功能
 */
contract JoyueAgent {
    // 引入缓存和 RPC Oracle
    address public cacheAddr;
    address public rpcOracleAddr;
    bool public initialized;
    address public master;
    uint32 public masterShardId;
    uint32 public agentShardId;

    event Initialized(address indexed master, uint32 masterShardId, uint32 agentShardId, address cacheAddr, address rpcOracleAddr);
    /// @notice 代理合约执行后产出的结果事件（方案A：事件 + relayer + master）
    /// @dev requestId 由 agent 内部递增计数生成，保证唯一性；user 为发起用户。
    /// @param master 目标主合约地址（位于 master shard）
    event AgentResult(address indexed master, bytes32 indexed requestId, address indexed user, bytes payload);

    uint256 private _reqCounter;

    /// @notice 构造函数：在部署时初始化主合约地址和分片信息
    /// @param master_ 主合约地址
    /// @param masterShardId_ 主合约所在分片 ID
    /// @param agentShardId_ 代理合约所在分片 ID
    /// @param cacheAddr_ 缓存合约地址（用于测试，可为 address(0)）
    /// @param rpcOracleAddr_ RPC Oracle 合约地址（用于测试，可为 address(0)）
    constructor(address master_, uint32 masterShardId_, uint32 agentShardId_, address cacheAddr_, address rpcOracleAddr_) {
        initialized = true;
        master = master_;
        masterShardId = masterShardId_;
        agentShardId = agentShardId_;
        cacheAddr = cacheAddr_;
        rpcOracleAddr = rpcOracleAddr_;
        emit Initialized(master_, masterShardId_, agentShardId_, cacheAddr_, rpcOracleAddr_);
    }

    /// @notice 兼容旧版本的初始化函数（已废弃，保留用于向后兼容）
    /// @dev 如果合约通过构造函数初始化，此函数将始终 revert
    function initialize(address master_, uint32 masterShardId_, uint32 agentShardId_, address cacheAddr_, address rpcOracleAddr_) external {
        require(!initialized, "already initialized");
        initialized = true;
        master = master_;
        masterShardId = masterShardId_;
        agentShardId = agentShardId_;
        cacheAddr = cacheAddr_;
        rpcOracleAddr = rpcOracleAddr_;
        emit Initialized(master_, masterShardId_, agentShardId_, cacheAddr_, rpcOracleAddr_);
    }

    /// @notice 用户调用代理合约产生"结果/意图"，通过事件交给 relayer 转发到 master shard
    /// @param master_ 目标主合约地址（demo 阶段由调用方传入；也可先 initialize 存储）
    /// @param payload 任意业务结果/意图 bytes（demo 阶段不做约束）
    /// @return requestId 该次请求的唯一 ID（后续在 master shard 查询用）
    function execute(address master_, bytes memory payload) external returns (bytes32 requestId) {
        _reqCounter += 1;
        requestId = keccak256(
            abi.encodePacked(
                address(this),
                msg.sender,
                _reqCounter,
                keccak256(payload)
            )
        );
        emit AgentResult(master_, requestId, msg.sender, payload);
    }

    // =========================
    // 缓存和 RPC 测试功能
    // =========================

    /**
     * @notice 从缓存读取主合约状态（测试 P2P 缓存）
     * @param masterContractAddr 主合约地址
     * @param key 状态键
     * @return value 缓存值
     * @return version 版本号
     * @return ok 是否存在
     */
    function getMasterStateFromCache(address masterContractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok) {
        require(cacheAddr != address(0), "cache not set");
        // 通过接口调用缓存合约
        IJoyueCache cache = IJoyueCache(cacheAddr);
        bytes memory valueBytes;
        (valueBytes, version, ok) = cache.get(masterContractAddr, key);
        if (ok && valueBytes.length >= 32) {
            // 解析 bytes 为 uint256
            assembly {
                value := mload(add(valueBytes, 0x20))
            }
        }
        return (value, version, ok);
    }

    /**
     * @notice 通过 RPC 查询主合约状态（测试 RPC Oracle）
     * @param masterContractAddr 主合约地址
     * @param key 状态键
     * @return value 状态值
     * @return version 版本号
     * @return ok 是否存在
     */
    function getMasterStateFromRPC(address masterContractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok) {
        require(rpcOracleAddr != address(0), "rpc oracle not set");
        // 通过接口调用 RPC Oracle 合约
        IJoyueRpcOracle rpcOracle = IJoyueRpcOracle(rpcOracleAddr);
        (value, version, ok) = rpcOracle.getUint(masterShardId, masterContractAddr, key);
        return (value, version, ok);
    }

    /**
     * @notice 更新本地缓存（测试缓存写入）
     * @param contractAddr 合约地址
     * @param key 状态键
     * @param value 状态值
     * @param version 版本号
     */
    function updateLocalCache(address contractAddr, bytes32 key, uint256 value, uint64 version) external {
        require(cacheAddr != address(0), "cache not set");
        // 通过接口调用缓存合约的 setUint
        IJoyueCacheWriter cache = IJoyueCacheWriter(cacheAddr);
        cache.setUint(contractAddr, key, value, version);
    }
}


