# JoyueRpcOracleMock 调用流程详解

## 概述

`JoyueRpcOracleMock` 是一个 Solidity 合约，用于通过 RPC 查询其他分片的主合约权威状态。它通过调用 precompile（预编译合约）来实现跨分片状态查询。

## 整体架构

```
┌─────────────────────────────────────────────────────────────┐
│  Solidity 合约层                                             │
│  JoyueRpcOracleMock.sol                                     │
│  - getUint()                                                │
│  - callGetter()                                             │
└───────────────────┬─────────────────────────────────────────┘
                    │ staticcall(0x67, input)
                    ▼
┌─────────────────────────────────────────────────────────────┐
│  Precompile 层                                               │
│  core/vm/contracts_joyue.go                                 │
│  joyueRpcOraclePrecompile (地址: 0x67)                       │
│  - RequiredGas()                                            │
│  - Run()                                                    │
└───────────────────┬─────────────────────────────────────────┘
                    │ GetGlobalRpcOracle()
                    ▼
┌─────────────────────────────────────────────────────────────┐
│  RPC Oracle 层                                              │
│  internal/joyue/rpc_oracle.go                               │
│  - QueryContractState()                                     │
│  - QueryContractGetter()                                    │
└───────────────────┬─────────────────────────────────────────┘
                    │ ethclient.DialContext()
                    ▼
┌─────────────────────────────────────────────────────────────┐
│  RPC 调用层                                                  │
│  - eth_getStorageAt (查询 storage)                          │
│  - eth_call (调用 getter 函数)                              │
└───────────────────┬─────────────────────────────────────────┘
                    │ HTTP/WebSocket RPC
                    ▼
┌─────────────────────────────────────────────────────────────┐
│  其他分片的节点                                              │
│  - 查询主合约状态                                            │
│  - 返回权威状态值                                            │
└─────────────────────────────────────────────────────────────┘
```

## 详细调用流程

### 场景 1: 查询 Storage Slot (`getUint`)

#### 步骤 1: Solidity 合约调用

```solidity
// 用户调用
(uint256 value, uint64 version, bool ok) = oracle.getUint(
    0,                              // shardID: 目标分片 ID
    0x12a4113F44E5689C93df233008FD805BDc882ABb,  // contractAddr: 主合约地址
    0x0000000000000000000000000000000000000000000000000000000000000001  // key: storage slot
);
```

#### 步骤 2: 构造输入数据

```solidity
// 在 getUint() 函数内部
bytes memory input = abi.encodePacked(
    uint32(shardID),      // 4 bytes: 0x00000000
    contractAddr,         // 20 bytes: 0x12a4113F44E5689C93df233008FD805BDc882ABb
    key                    // 32 bytes: 0x0000000000000000000000000000000000000000000000000000000000000001
);
// 总长度: 4 + 20 + 32 = 56 bytes
```

#### 步骤 3: 调用 Precompile

```solidity
(bool success, bytes memory result) = JOYUE_RPC_ORACLE_PRECOMPILE.staticcall(input);
// 地址: 0x0000000000000000000000000000000000000103 (0x67 = 103)
```

**EVM 执行流程**：
1. EVM 检测到 `staticcall` 到地址 `0x67`
2. 检查 `PrecompiledContractsJoyue` 映射
3. 找到 `joyueRpcOraclePrecompile`
4. 调用 `RequiredGas(input)` 计算 gas
5. 调用 `Run(input)` 执行 precompile

#### 步骤 4: Precompile 处理

```go
// core/vm/contracts_joyue.go
func (c *joyueRpcOraclePrecompile) Run(input []byte) ([]byte, error) {
    // 1. 解析输入
    shardID := binary.BigEndian.Uint32(input[0:4])        // 提取 shardID
    contractAddr := common.BytesToAddress(input[4:24])    // 提取合约地址
    key := common.BytesToHash(input[24:56])               // 提取 key
    
    // 2. 获取全局 RPC Oracle
    rpcOracle := joyue.GetGlobalRpcOracle()
    
    // 3. 查询状态
    value, version, ok := rpcOracle.QueryContractState(shardID, contractAddr, key)
    
    // 4. 编码返回结果
    return encodeRpcOracleResult(value, version, ok), nil
}
```

#### 步骤 5: RPC Oracle 查询

```go
// internal/joyue/rpc_oracle.go
func (ro *RpcOracle) QueryContractState(shardID uint32, contractAddr common.Address, key common.Hash) ([]byte, uint64, bool) {
    // 1. 获取或创建 RPC 客户端（连接池管理）
    client, err := ro.getClient(shardID)
    // 例如: shardID=0 -> http://127.0.0.1:9500
    
    // 2. 通过 eth_getStorageAt 查询 storage
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    
    state, err := client.StorageAt(ctx, contractAddr, key, nil)
    // RPC 调用: eth_getStorageAt(contractAddr, key, "latest")
    
    // 3. 返回结果（version 设为 0，因为 eth_getStorageAt 不返回版本号）
    return state, 0, true
}
```

#### 步骤 6: RPC 调用

```json
// HTTP POST 请求
POST http://127.0.0.1:9500
Content-Type: application/json

{
  "jsonrpc": "2.0",
  "method": "eth_getStorageAt",
  "params": [
    "0x12a4113F44E5689C93df233008FD805BDc882ABb",
    "0x0000000000000000000000000000000000000000000000000000000000000001",
    "latest"
  ],
  "id": 1
}

// 响应
{
  "jsonrpc": "2.0",
  "result": "0x0000000000000000000000000000000000000000000000000000000000000064",
  "id": 1
}
```

#### 步骤 7: 编码返回结果

```go
// encodeRpcOracleResult 编码返回结果
// 格式: abi.encode(bytes value, uint64 version, bool ok)
func encodeRpcOracleResult(value []byte, version uint64, ok bool) []byte {
    // result[0:32] = offset for bytes (96)
    // result[32:64] = length of bytes (32)
    // result[64:96] = version (uint64, padded to 32 bytes)
    // result[96:128] = ok (bool, padded to 32 bytes)
    // result[128:] = bytes data (value)
    return encodeCacheResult(value, version, ok)
}
```

#### 步骤 8: Solidity 解析返回结果

```solidity
// 在 getUint() 函数内部
// 解析返回结果
uint256 offset = abi.decode(result[0:32], (uint256));      // 96
uint256 valueLen = abi.decode(result[32:64], (uint256));    // 32
version = uint64(uint256(abi.decode(result[64:96], (uint256))));  // 0
ok = abi.decode(result[96:128], (bool)) != false;          // true

// 提取 value
if (valueLen == 32) {
    value = abi.decode(result[128:160], (uint256));  // 0x64 = 100
}

return (value, version, ok);  // (100, 0, true)
```

---

### 场景 2: 调用 Getter 函数 (`callGetter`)

#### 步骤 1: Solidity 合约调用

```solidity
// 用户调用
bytes memory calldata_ = abi.encodeWithSignature(
    "getResult(bytes32)",
    requestId
);
bytes memory result = oracle.callGetter(
    0,                              // shardID
    0x12a4113F44E5689C93df233008FD805BDc882ABb,  // contractAddr
    calldata_                       // calldata: 函数选择器 + 参数
);
```

#### 步骤 2: 构造输入数据

```solidity
// 在 callGetter() 函数内部
bytes memory input = abi.encodePacked(
    uint32(shardID),        // 4 bytes
    contractAddr,           // 20 bytes
    uint32(calldata_.length), // 4 bytes: calldata 长度
    calldata_               // 可变长度: 函数选择器 + 参数
);
// 例如: 4 + 20 + 4 + 36 = 64 bytes
```

#### 步骤 3-4: 调用 Precompile（同场景 1）

#### 步骤 5: RPC Oracle 调用 Getter

```go
// internal/joyue/rpc_oracle.go
func (ro *RpcOracle) QueryContractGetter(shardID uint32, contractAddr common.Address, calldata []byte) ([]byte, error) {
    // 1. 获取 RPC 客户端
    client, err := ro.getClient(shardID)
    
    // 2. 通过 eth_call 调用合约函数
    msg := ethereum.CallMsg{
        To:   &contractAddr,
        Data: calldata,  // 函数选择器 + 参数
    }
    
    result, err := client.CallContract(ctx, msg, nil)
    // RPC 调用: eth_call({to: contractAddr, data: calldata}, "latest")
    
    return result, nil
}
```

#### 步骤 6: RPC 调用

```json
// HTTP POST 请求
POST http://127.0.0.1:9500
Content-Type: application/json

{
  "jsonrpc": "2.0",
  "method": "eth_call",
  "params": [
    {
      "to": "0x12a4113F44E5689C93df233008FD805BDc882ABb",
      "data": "0x12345678..."  // 函数选择器 + 参数
    },
    "latest"
  ],
  "id": 1
}

// 响应
{
  "jsonrpc": "2.0",
  "result": "0x0000000000000000000000000000000000000000000000000000000000000001...",
  "id": 1
}
```

#### 步骤 7: 直接返回结果

```go
// Precompile 直接返回 getter 的结果（不包装）
return result, nil
```

#### 步骤 8: Solidity 接收结果

```solidity
// 在 callGetter() 函数内部
return ret;  // 直接返回，由调用者解析
```

---

## 数据格式详解

### 输入格式（Storage 查询）

```
┌──────────┬──────────────┬──────────────────────────────┐
│ shardID  │ contractAddr │            key               │
│ (4 bytes)│  (20 bytes)  │        (32 bytes)            │
└──────────┴──────────────┴──────────────────────────────┘
总长度: 56 bytes
```

### 输入格式（Getter 调用）

```
┌──────────┬──────────────┬──────────────┬──────────────┐
│ shardID  │ contractAddr │ calldataLen  │   calldata   │
│ (4 bytes)│  (20 bytes)  │  (4 bytes)   │ (可变长度)    │
└──────────┴──────────────┴──────────────┴──────────────┘
总长度: 28 + calldataLen bytes
```

### 输出格式（Storage 查询）

```
┌──────────┬──────────────┬──────────────┬──────────────┬──────────────┐
│  offset  │  valueLen    │   version    │     ok       │    value     │
│ (32 bytes)│  (32 bytes) │  (32 bytes)  │  (32 bytes)  │ (valueLen)   │
└──────────┴──────────────┴──────────────┴──────────────┴──────────────┘
offset = 96 (固定值)
总长度: 128 + valueLen bytes
```

### 输出格式（Getter 调用）

```
直接返回函数返回值（不包装）
```

---

## 关键点说明

### 1. Precompile 地址

```solidity
address private constant JOYUE_RPC_ORACLE_PRECOMPILE = address(0x67);
// 0x0000000000000000000000000000000000000103
```

### 2. staticcall vs call

- **staticcall**: 只读操作，不修改状态
- **call**: 可写操作，可能修改状态

这里使用 `staticcall` 因为只是查询，不修改状态。

### 3. Gas 消耗

```go
baseGas := uint64(1000)  // RPC 调用需要更多 gas（网络 I/O）
dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
return baseGas + dataGas
```

RPC 调用需要更多 gas，因为涉及网络 I/O。

### 4. 超时处理

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
```

RPC 调用有 5 秒超时，避免长时间阻塞。

### 5. 连接池管理

```go
// RPC 客户端会被缓存，避免重复创建连接
func (ro *RpcOracle) getClient(shardID uint32) (*ethclient.Client, error) {
    // 检查缓存
    if client, exists := ro.rpcClients[shardID]; exists {
        return client, nil
    }
    // 创建新客户端并缓存
    client, err := ethclient.DialContext(ctx, rpcURL)
    ro.rpcClients[shardID] = client
    return client, nil
}
```

---

## 使用示例

### 示例 1: 查询 Storage Slot

```solidity
contract MyContract {
    JoyueRpcOracleMock oracle = new JoyueRpcOracleMock();
    
    function checkMasterState(
        uint32 masterShardID,
        address masterAddr,
        bytes32 key
    ) external view returns (uint256 value) {
        (uint256 val, , bool ok) = oracle.getUint(masterShardID, masterAddr, key);
        require(ok, "state not found");
        return val;
    }
}
```

### 示例 2: 调用 Getter 函数

```solidity
function getMasterResult(
    uint32 masterShardID,
    address masterAddr,
    bytes32 requestId
) external view returns (bool exists, address sender, uint32 fromShardId, bytes memory payload) {
    bytes memory calldata_ = abi.encodeWithSignature("getResult(bytes32)", requestId);
    bytes memory result = oracle.callGetter(masterShardID, masterAddr, calldata_);
    return abi.decode(result, (bool, address, uint32, bytes));
}
```

---

## 性能考虑

### 延迟

- **RPC 调用延迟**: 50-200ms（网络 I/O）
- **Precompile 处理**: < 1ms
- **Solidity 解析**: < 1ms
- **总延迟**: 约 50-200ms

### Gas 消耗

- **基础 gas**: 1000
- **数据 gas**: 根据输入长度计算
- **总 gas**: 约 1000-2000（取决于输入大小）

### 优化建议

1. **缓存结果**: 在合约层面缓存查询结果
2. **批量查询**: 一次查询多个状态
3. **异步处理**: 使用事件通知，而不是同步等待

---

## 错误处理

### 常见错误

1. **RPC 连接失败**
   ```go
   return nil, 0, false  // 返回空值
   ```

2. **超时**
   ```go
   context.WithTimeout(context.Background(), 5*time.Second)
   ```

3. **版本号未知**
   ```go
   return state, 0, true  // version 设为 0（eth_getStorageAt 不返回版本号）
   ```

### Solidity 错误处理

```solidity
(bool success, bytes memory result) = JOYUE_RPC_ORACLE_PRECOMPILE.staticcall(input);
if (!success || result.length < 128) {
    return (0, 0, false);  // 返回默认值
}
```

---

## 总结

`JoyueRpcOracleMock` 的调用流程：

1. **Solidity 合约** → 构造输入数据
2. **staticcall** → 调用 precompile (0x67)
3. **Precompile** → 解析输入，调用 RPC Oracle
4. **RPC Oracle** → 通过 RPC 查询其他分片
5. **RPC 调用** → HTTP/WebSocket 请求
6. **返回结果** → 编码并返回给 Solidity
7. **Solidity 解析** → 解析返回结果

整个过程是**同步的**，会阻塞 EVM 执行直到 RPC 调用完成。

