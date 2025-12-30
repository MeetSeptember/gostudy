// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22 <0.9.0;

/**
 * @title JoyueAgent（第一阶段：最小代理合约）
 * @notice 最小 Agent 合约：只存一些路由元信息，证明“该分片的 agent 已部署”。
 *
 * 说明：
 * - 第一阶段不要求 agent 地址一致，也不做跨分片合约调用。
 * - 后续第二阶段才会把“代理合约执行意图 + 主合约权威结算”等逻辑接上来。
 */
contract JoyueAgent {
    bool public initialized;
    address public master;
    uint32 public masterShardId;
    uint32 public agentShardId;

    event Initialized(address master, uint32 masterShardId, uint32 agentShardId);
    /// @notice 代理合约执行后产出的结果事件（方案A：事件 + relayer + master）
    /// @dev requestId 由 agent 内部递增计数生成，保证唯一性；user 为发起用户。
    /// @param master 目标主合约地址（位于 master shard）
    event AgentResult(address indexed master, bytes32 indexed requestId, address indexed user, bytes payload);

    uint256 private _reqCounter;

    /// @notice 构造函数：在部署时初始化主合约地址和分片信息
    /// @param master_ 主合约地址
    /// @param masterShardId_ 主合约所在分片 ID
    /// @param agentShardId_ 代理合约所在分片 ID
    constructor(address master_, uint32 masterShardId_, uint32 agentShardId_) {
        initialized = true;
        master = master_;
        masterShardId = masterShardId_;
        agentShardId = agentShardId_;
        emit Initialized(master_, masterShardId_, agentShardId_);
    }

    /// @notice 兼容旧版本的初始化函数（已废弃，保留用于向后兼容）
    /// @dev 如果合约通过构造函数初始化，此函数将始终 revert
    function initialize(address master_, uint32 masterShardId_, uint32 agentShardId_) external {
        require(!initialized, "already initialized");
        initialized = true;
        master = master_;
        masterShardId = masterShardId_;
        agentShardId = agentShardId_;
        emit Initialized(master_, masterShardId_, agentShardId_);
    }

    /// @notice 用户调用代理合约产生“结果/意图”，通过事件交给 relayer 转发到 master shard
    /// @param master_ 目标主合约地址（demo 阶段由调用方传入；也可先 initialize 存储）
    /// @param payload 任意业务结果/意图 bytes（demo 阶段不做约束）
    /// @return requestId 该次请求的唯一 ID（后续在 master shard 查询用）
    function execute(address master_, bytes calldata payload) external returns (bytes32 requestId) {
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
}


