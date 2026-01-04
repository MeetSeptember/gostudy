# JOYUE RPC Oracle 模块

## 功能概述

RPC Oracle 模块用于通过 RPC 查询其他分片的主合约权威状态（非缓存）。与 P2P 缓存不同，RPC Oracle 直接从其他分片的节点查询最新状态，确保获取的是权威数据。

## 架构

### 1. RPC Oracle 模块 (`internal/joyue/rpc_oracle.go`)

- **`RpcOracle`**: 管理跨分片 RPC 连接和查询
- **`QueryContractState`**: 通过 `eth_getStorageAt` 查询合约 storage
- **`QueryContractGetter`**: 通过 `eth_call` 调用合约 getter 函数

### 2. Precompile (`core/vm/contracts_joyue.go`)

- **地址**: `0x0000000000000000000000000000000000000103` (0x67 = 103)
- **功能**: 在 EVM 执行过程中调用 RPC Oracle
- **输入格式**:
  - 查询 storage: `4 bytes (shardID) + 20 bytes (contractAddr) + 32 bytes (key)`
  - 调用 getter: `4 bytes (shardID) + 20 bytes (contractAddr) + 4 bytes (calldataLen) + calldata`
- **输出格式**:
  - 查询 storage: `abi.encode(bytes value, uint64 version, bool ok)`
  - 调用 getter: 直接返回函数返回值

### 3. Solidity 合约 (`contracts/baselib/JoyueRpcOracleMock.sol`)

- **`getUint`**: 查询其他分片的主合约状态（通过 storage slot）
- **`callGetter`**: 调用其他分片的主合约 getter 函数

## 配置

在节点配置中设置其他分片的 RPC 地址：

```toml
[Joyue]
other_shard_rpcs = "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502"
```

或通过命令行参数：

```bash
--joyue.other-shard-rpcs="0=http://127.0.0.1:9500,1=http://127.0.0.1:9502"
```

格式：`shardID=rpcURL,shardID=rpcURL`

## 使用方法

### 在 Solidity 合约中使用

```solidity
import "./baselib/JoyueRpcOracleMock.sol";

contract MyContract {
    JoyueRpcOracleMock oracle = new JoyueRpcOracleMock();

    function queryOtherShard(uint32 shardID, address contractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok) {
        return oracle.getUint(shardID, contractAddr, key);
    }

    function callOtherShardGetter(uint32 shardID, address contractAddr, bytes calldata calldata_) external view returns (bytes memory) {
        return oracle.callGetter(shardID, contractAddr, calldata_);
    }
}
```

### 查询 Storage Slot

```solidity
// 查询 shard 0 的合约 0x1234... 的 storage slot 0x0000...01
(uint256 value, uint64 version, bool ok) = oracle.getUint(0, 0x1234..., 0x0000...01);
```

### 调用 Getter 函数

```solidity
// 调用 shard 0 的合约 0x1234... 的 getResult(bytes32) 函数
bytes memory calldata_ = abi.encodeWithSignature("getResult(bytes32)", requestId);
bytes memory result = oracle.callGetter(0, 0x1234..., calldata_);
(bool exists, address sender, uint32 fromShardId, bytes memory payload) = abi.decode(result, (bool, address, uint32, bytes));
```

## 与 P2P 缓存的区别

| 特性 | P2P 缓存 | RPC Oracle |
|------|----------|------------|
| 数据来源 | 本地缓存（通过 P2P 同步） | 直接 RPC 查询其他分片节点 |
| 数据新鲜度 | 可能有延迟（P2P 同步） | 最新状态（直接查询） |
| 性能 | 快（本地读取） | 较慢（网络 I/O） |
| 可靠性 | 依赖 P2P 同步 | 依赖 RPC 连接 |
| 使用场景 | 快速读取（可容忍延迟） | 需要最新状态 |

## 注意事项

1. **性能**: RPC 调用涉及网络 I/O，可能较慢，建议在必要时使用
2. **超时**: 默认超时时间为 5 秒，可通过修改 `RpcOracle.clientTimeout` 调整
3. **连接管理**: RPC 客户端会被缓存，避免重复创建连接
4. **安全性**: 当前实现不考虑安全性，仅实现基本功能
5. **版本号**: 通过 `eth_getStorageAt` 查询无法获取版本号，返回 0。如需版本号，请使用 `callGetter` 调用合约的 getter 函数

## 初始化

RPC Oracle 在节点启动时自动初始化（`node/harmony/node.go`）：

```go
rpcOracle, err := joyue.NewRpcOracle(node.NodeConfig)
if err != nil {
    utils.Logger().Error().Err(err).Msg("[JOYUE] failed to create RPC oracle")
} else {
    joyue.SetGlobalRpcOracle(rpcOracle)
    utils.Logger().Info().Msg("[JOYUE] RPC oracle initialized")
}
```

## 示例场景

### 场景 1: 查询主合约的最新状态

假设主合约在 shard 0，代理合约在 shard 1：

```solidity
// 在 shard 1 的代理合约中
contract MyAgent {
    JoyueRpcOracleMock oracle = new JoyueRpcOracleMock();

    function checkMasterState(uint32 masterShardID, address masterAddr, bytes32 key) external view returns (uint256 value) {
        (uint256 val, , bool ok) = oracle.getUint(masterShardID, masterAddr, key);
        require(ok, "state not found");
        return val;
    }
}
```

### 场景 2: 调用主合约的 getter 函数

```solidity
// 查询主合约的 getResult 函数
bytes memory calldata_ = abi.encodeWithSignature("getResult(bytes32)", requestId);
bytes memory result = oracle.callGetter(masterShardID, masterAddr, calldata_);
(bool exists, address sender, uint32 fromShardId, bytes memory payload) = abi.decode(result, (bool, address, uint32, bytes));
```

## 故障排除

1. **RPC 连接失败**: 检查配置的 RPC URL 是否正确，目标分片的节点是否运行
2. **查询超时**: 增加 `clientTimeout` 或检查网络连接
3. **返回空值**: 检查合约地址和 key 是否正确，目标分片的合约是否已部署

