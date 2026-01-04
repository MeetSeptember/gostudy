# JOYUE P2P 缓存集成指南

## 实现概述

实现了完整的 P2P 缓存系统，包括：
1. **节点层面**：监听 `StateBroadcast` 事件，更新本地缓存，P2P 广播
2. **合约层面**：通过 precompile 访问节点层面的缓存

## 架构

```
Master 合约状态修改
    ↓
StateBroadcast 事件
    ↓
节点监听 (post_processing.go)
    ↓
CacheBroadcaster.ProcessBlockLogs()
    ↓
更新本地缓存 + P2P 广播
    ↓
其他节点接收并更新缓存
    ↓
合约调用 JoyueMockCache.get()
    ↓
调用 Precompile (0x64)
    ↓
从节点缓存获取数据
```

## 已实现文件

### 1. 节点层面

- **`internal/joyue/cache_broadcast.go`**
  - `CacheBroadcaster`：缓存广播器
  - `ProcessBlockLogs()`：处理区块日志
  - `GetCache()`：获取缓存值
  - 全局访问器：`SetGlobalCacheBroadcaster()`, `GetGlobalCacheBroadcaster()`

### 2. Precompile

- **`core/vm/contracts_joyue.go`**
  - `joyueCachePrecompile`：P2P 缓存访问 precompile
  - 地址：`0x0000000000000000000000000000000000000100` (100, 0x64)

### 3. 合约

- **`contracts/baselib/JoyueMockCache.sol`**
  - 修改为通过 precompile 访问 P2P 缓存
  - 保留本地 mock 缓存作为 fallback

## 集成步骤

### 1. 在节点启动时初始化缓存广播器

在 `node/harmony/node.go` 或类似位置：

```go
import "github.com/harmony-one/harmony/internal/joyue"

// 在节点初始化时
if nodeConfig.Joyue.CacheEnabled { // 需要添加配置项
    cacheBroadcaster, err := joyue.NewCacheBroadcaster(host, nodeConfig)
    if err != nil {
        log.Error("failed to create cache broadcaster", "error", err)
    } else {
        // 设置为全局访问器（供 precompile 使用）
        joyue.SetGlobalCacheBroadcaster(cacheBroadcaster)
        
        // 可选：设置到 consensus（如果需要）
        // consensus.SetCacheBroadcaster(cacheBroadcaster)
    }
}
```

### 2. 在共识后处理中调用

在 `consensus/post_processing.go` 的 `postConsensusProcessing` 函数中：

```go
import "github.com/harmony-one/harmony/internal/joyue"

func (consensus *Consensus) postConsensusProcessing(newBlock *types.Block) error {
    // ... 现有代码 ...
    
    // 处理 JOYUE 缓存广播
    cacheBroadcaster := joyue.GetGlobalCacheBroadcaster()
    if cacheBroadcaster != nil {
        receipts := consensus.Blockchain().GetReceiptsByHash(newBlock.Hash())
        if receipts != nil {
            cacheBroadcaster.ProcessBlockLogs(newBlock, receipts)
        }
    }
    
    // ... 其他代码 ...
    return nil
}
```

### 3. 在 EVM 中注册 precompile

已在 `core/vm/evm.go` 的 `run` 函数中添加：

```go
// 添加 JOYUE 缓存 precompile 支持
if p, ok := PrecompiledContractsJoyue[*contract.CodeAddr]; ok {
    return RunPrecompiledContract(p, input, contract)
}
```

### 4. 添加配置项（可选）

在 `internal/configs/node/config.go` 中：

```go
type ConfigType struct {
    // ... 现有字段 ...
    Joyue struct {
        AutoDeployEnabled bool
        DeployPrivateKey  string
        OtherShardRPCs    string
        CacheEnabled      bool  // 新增：是否启用 P2P 缓存
    }
}
```

## 使用方式

### 合约中使用

```solidity
// Agent 合约中
address public cache; // JoyueMockCache 合约地址

function myBusinessLogic() external {
    // 从 P2P 缓存读取状态
    (bytes memory value, uint64 version, bool ok) = 
        IJoyueCache(cache).get(masterContract, key);
    
    if (ok) {
        uint256 state = abi.decode(value, (uint256));
        // 使用缓存的状态值
    }
}
```

### Precompile 调用格式

```solidity
// 直接调用 precompile
address constant JOYUE_CACHE = address(0x64);

function getFromCache(address contractAddr, bytes32 key) external view 
    returns (bytes memory value, uint64 version, bool ok) 
{
    (bool success, bytes memory result) = JOYUE_CACHE.staticcall(
        abi.encodePacked(contractAddr, key)
    );
    // 解析 result...
}
```

## 工作流程示例

### 1. Master 合约状态更新

```solidity
contract MyMaster is JoyueVerifier {
    function updateState(bytes32 key, uint256 value) external {
        _setUint(key, value);  // 自动 emit StateBroadcast 事件
    }
}
```

### 2. 节点处理事件

- 节点在 `postConsensusProcessing` 中检测到 `StateBroadcast` 事件
- 更新本地缓存
- 通过 P2P 广播到其他节点

### 3. Agent 合约读取缓存

```solidity
contract MyAgent {
    function execute() external {
        // 从 P2P 缓存读取 Master 状态
        JoyueLib.JVar memory state = JoyueLib.loadFromCacheByKey(
            ctx,
            master,
            key
        );
        // 使用缓存值...
    }
}
```

## 注意事项

1. **Precompile 地址**：`0x64` (100) 是固定的，不要与其他 precompile 冲突
2. **全局访问器**：使用 `sync.RWMutex` 保护，线程安全
3. **Fallback 机制**：如果 precompile 失败，`JoyueMockCache` 会回退到本地 mock 缓存
4. **性能**：本地缓存使用简单 map，生产环境建议使用 LRU 缓存

## 测试建议

1. **单元测试**：
   - 测试 precompile 的编码/解码
   - 测试缓存广播器的序列化/反序列化

2. **集成测试**：
   - 部署 Master 合约，更新状态
   - 验证节点是否监听到事件并更新缓存
   - 验证 Agent 合约能否从缓存读取数据

3. **P2P 测试**：
   - 多节点环境测试缓存同步
   - 验证跨节点缓存一致性

