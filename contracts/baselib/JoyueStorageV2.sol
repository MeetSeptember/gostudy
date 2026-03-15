// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22;

/**
 * @title JoyueStorageV2
 * @dev 数据层：权威状态存储，适配预编译合约的存储布局。
 * 
 * 存储布局（与预编译合约一致）：
 * - slot 0: mapping(bytes32 => uint256) _u
 * - slot 1: mapping(bytes32 => uint64) _ver
 * - slot 2: address public rpcOracle
 * 
 * 注意：
 * - 瞬时状态（_tU, _tSet, _tKeys, _tKeySeen）现在由预编译合约在内存中管理，不再使用 Storage
 * - _frozenDeltas 已改为 Blob 模式存储，由预编译合约直接管理
 * - _frozen 映射已移除，通过 _frozenDeltas 的存在来判断冻结状态
 */
contract JoyueStorageV2 {
    // 权威状态
    mapping(bytes32 => uint256) internal _u;
    mapping(bytes32 => uint64) internal _ver;

    // 协调者侧 RPC Oracle
    address public rpcOracle;

    /// @notice 与 Coordinator 同分片的 Agent 地址（用于重试等）。部署同分片 Agent 后手动调用 setAgentOnSameShard 注册
    address public agentOnSameShard;

    function setRpcOracle(address oracle) external {
        rpcOracle = oracle;
    }

    /// @notice 注册与 Coordinator 同分片的 Agent 地址。仅在同分片部署 Agent 后手动调用一次
    /// @return 返回设置的 agentAddr，便于调用方验证成功
    function setAgentOnSameShard(address agentAddr) external returns (address) {
        agentOnSameShard = agentAddr;
        return agentAddr;
    }

    // ---------- 权威读写（供外部查询使用） ----------
    function getUint(bytes32 key) public view returns (uint256 value, uint64 version) {
        return (_u[key], _ver[key]);
    }

    function _setUint(bytes32 key, uint256 value) internal {
        _u[key] = value;
        _ver[key] = _ver[key] + 1;
    }

    // ---------- State Broadcast 事件（用于缓存同步） ----------
    event StateBroadcast(address indexed contractAddr, bytes32 indexed key, uint256 value, uint64 version);

    function _broadcastState(bytes32 key) internal {
        emit StateBroadcast(address(this), key, _u[key], _ver[key]);
    }
}

