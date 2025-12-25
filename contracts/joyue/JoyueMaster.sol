// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22 <0.9.0;

/**
 * @title JoyueMaster（第一阶段：仅用于触发 relayer）
 * @notice 最小 Master 合约：部署时 emit 事件，让链下 relayer 监听后去其它分片部署 Agent。
 *
 * 说明：
 * - 这个合约本身不做任何 JOYUE 的 Guard/Delta 逻辑（先把“自动铺 agent”跑通）。
 * - 跨分片部署由 relayer 通过 RPC 发送交易实现，不改共识/协议。
 */
contract JoyueMaster {
    /**
     * @notice 部署事件（在 constructor emit）
     * @param master      Master 合约地址（address(this)）
     * @param salt       预留字段：未来如果做 CREATE2 / factory 可用
     * @param agentCreationCode 完整的 Agent Creation Code（包含 constructor args 的那段 tx.data）
     * @param masterShardId  master 分片 id（仅做事件携带，不由 EVM 强制）
     */
    event MasterDeployed(
        address indexed master,
        bytes32 indexed salt,
        bytes agentCreationCode,
        uint32 masterShardId
    );

    bytes32 public immutable salt;
    /// @notice 仅存一个 hash，避免把大字节码存到 state（字节码本体放在事件里）
    bytes32 public immutable agentCreationCodeHash;
    uint32 public immutable masterShardId;

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
     * @param agentCreationCode_ 完整的 agent creation code（注意：这个参数会写进日志，太大可能导致部署交易非常昂贵）
     * @param masterShardId_ master shard id（仅做标识）
     */
    constructor(bytes32 salt_, bytes memory agentCreationCode_, uint32 masterShardId_) {
        salt = salt_;
        agentCreationCodeHash = keccak256(agentCreationCode_);
        masterShardId = masterShardId_;

        emit MasterDeployed(address(this), salt_, agentCreationCode_, masterShardId_);
    }

    /// @notice relayer 把 agent 产出的结果提交到 master shard（方案A 最小实现）
    /// @dev demo 阶段不做权限控制；生产应限制只允许特定 relayer/验证 proof。
    function submitAgentResult(bytes32 requestId, address sender, uint32 fromShardId, bytes calldata payload) external {
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
}


