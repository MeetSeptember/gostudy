// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22;

/**
 * @title JoyueMaster（第一阶段：仅用于触发 relayer）
 * @notice 最小 Master 合约：部署时 emit 事件，让链下 relayer 监听后去其它分片部署 Agent。
 *
 * 说明：
 * - 这个合约本身不做任何 JOYUE 的 Guard/Delta 逻辑（先把"自动铺 agent"跑通）。
 * - 跨分片部署由 relayer 通过 RPC 发送交易实现，不改共识/协议。
 * - 添加了缓存和 RPC 测试功能
 */
// 缓存接口
interface IJoyueCache {
    function get(address contractAddr, bytes32 key) external view returns (bytes memory value, uint64 version, bool ok);
}

// RPC Oracle 接口
interface IJoyueRpcOracle {
    function getUint(uint32 shardID, address contractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok);
}

contract JoyueMaster {
    // 引入缓存和 RPC Oracle
    address public cacheAddr;
    address public rpcOracleAddr;
    /**
     * @notice 部署事件（在 constructor emit）
     * @param master      Master 合约地址（address(this)）
     * @param salt       预留字段：未来如果做 CREATE2 / factory 可用
     * @param agentCreationCode Agent 合约的纯 bytecode（不含构造函数参数）
     *                          节点部署时会动态添加构造函数参数：constructor(address master_, uint32 masterShardId_, uint32 agentShardId_)
     * @param masterShardId  master 分片 id（仅做事件携带，不由 EVM 强制）
     */
    event MasterDeployed(
        address indexed master,
        bytes32 indexed salt,
        bytes agentCreationCode,
        uint32 masterShardId,
        address cacheAddr,
        address rpcOracleAddr
    );

    bytes32 public immutable salt;
    /// @notice 仅存一个 hash，避免把大字节码存到 state（字节码本体放在事件里）
    bytes32 public immutable agentCreationCodeHash;
    uint32 public immutable masterShardId;

    // =========================
    // 缓存和状态管理（用于测试）
    // =========================

    // 状态存储（用于测试 StateBroadcast 事件）
    mapping(bytes32 => uint256) private _state;
    mapping(bytes32 => uint64) private _version;

    // StateBroadcast 事件（用于 P2P 缓存同步）
    event StateBroadcast(address indexed contractAddr, bytes32 indexed key, uint256 value, uint64 version);

    // =========================
    // 方案A：Agent ->(event)-> Relayer -> Master
    // =========================

    struct ForwardedResult {
        bool exists;
        address sender;      // 用户地址（在 agent shard 上发起 execute 的用户）
        uint32 fromShardId;  // 来源分片 id（由 relayer 填）
        bytes payload;       // agent 产出结果/意图
    }

    mapping(bytes32 => ForwardedResult) private _results;

    event ResultAccepted(bytes32 indexed requestId, address indexed sender, uint32 fromShardId);

    /**
     * @param salt_ 预留字段
     * @param agentCreationCode_ Agent 合约的纯 bytecode（不含构造函数参数）
     *                           节点部署时会动态添加构造函数参数（注意：这个参数会写进日志，太大可能导致部署交易非常昂贵）
     * @param masterShardId_ master shard id（仅做标识）
     * @param cacheAddr_ 缓存合约地址（用于测试，可为 address(0)）
     * @param rpcOracleAddr_ RPC Oracle 合约地址（用于测试，可为 address(0)）
     */
    constructor(bytes32 salt_, bytes memory agentCreationCode_, uint32 masterShardId_, address cacheAddr_, address rpcOracleAddr_) {
        salt = salt_;
        agentCreationCodeHash = keccak256(agentCreationCode_);
        masterShardId = masterShardId_;
        cacheAddr = cacheAddr_;
        rpcOracleAddr = rpcOracleAddr_;

        emit MasterDeployed(address(this), salt_, agentCreationCode_, masterShardId_,cacheAddr_,rpcOracleAddr_);
    }

    /// @notice relayer 把 agent 产出的结果提交到 master shard（方案A 最小实现）
    /// @dev demo 阶段不做权限控制；生产应限制只允许特定 relayer/验证 proof。
    function submitAgentResult(bytes32 requestId, address sender, uint32 fromShardId, bytes memory payload) external {
        require(!_results[requestId].exists, "duplicate requestId");
        _results[requestId] = ForwardedResult({
            exists: true,
            sender: sender,
            fromShardId: fromShardId,
            payload: payload
        });
        emit ResultAccepted(requestId, sender, fromShardId);
    }

    /// @notice 查询转发结果（用户在 master shard 上查询）
    function getResult(bytes32 requestId) external view returns (bool exists, address sender, uint32 fromShardId, bytes memory payload) {
        ForwardedResult storage r = _results[requestId];
        return (r.exists, r.sender, r.fromShardId, r.payload);
    }

    // =========================
    // 缓存和 RPC 测试功能
    // =========================

    /**
     * @notice 设置状态并广播（用于测试 P2P 缓存）
     * @param key 状态键
     * @param value 状态值
     */
    function setState(bytes32 key, uint256 value) external {
        _version[key] = _version[key] + 1;
        _state[key] = value;
        emit StateBroadcast(address(this), key, value, _version[key]);
    }

    /**
     * @notice 获取状态（用于测试）
     * @param key 状态键
     * @return value 状态值
     * @return version 版本号
     */
    function getState(bytes32 key) external view returns (uint256 value, uint64 version) {
        return (_state[key], _version[key]);
    }

    /**
     * @notice 从缓存读取状态（测试 P2P 缓存）
     * @param key 状态键
     * @return value 缓存值
     * @return version 版本号
     * @return ok 是否存在
     */
    function getStateFromCache(bytes32 key) external view returns (uint256 value, uint64 version, bool ok) {
        require(cacheAddr != address(0), "cache not set");
        // 通过接口调用缓存合约
        IJoyueCache cache = IJoyueCache(cacheAddr);
        bytes memory valueBytes;
        (valueBytes, version, ok) = cache.get(address(this), key);
        if (ok && valueBytes.length >= 32) {
            // 解析 bytes 为 uint256
            assembly {
                value := mload(add(valueBytes, 0x20))
            }
        }
        return (value, version, ok);
    }

    /**
     * @notice 通过 RPC 查询其他分片的状态（测试 RPC Oracle）
     * @param shardID 目标分片 ID
     * @param contractAddr 合约地址
     * @param key 状态键
     * @return value 状态值
     * @return version 版本号
     * @return ok 是否存在
     */
    function getStateFromRPC(uint32 shardID, address contractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok) {
        require(rpcOracleAddr != address(0), "rpc oracle not set");
        // 通过接口调用 RPC Oracle 合约
        IJoyueRpcOracle rpcOracle = IJoyueRpcOracle(rpcOracleAddr);
        (value, version, ok) = rpcOracle.getUint(shardID, contractAddr, key);
        return (value, version, ok);
    }
}


