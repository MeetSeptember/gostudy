# JOYUE P2P 缓存实现方案

## 方案概述

实现一个基于事件监听和 P2P 广播的合约状态缓存系统。

## 架构设计

```
┌─────────────────┐
│   Master 合约   │
│  (JoyueVerifier)│
└────────┬────────┘
         │
         │ emit StateBroadcast 事件
         ↓
┌─────────────────┐
│   节点监听      │
│ post_processing │
└────────┬────────┘
         │
         │ ProcessBlockLogs()
         ↓
┌─────────────────┐
│ CacheBroadcaster│
│  - 更新本地缓存 │
│  - P2P 广播     │
└────────┬────────┘
         │
         │ PubSub Topic
         ↓
┌─────────────────┐
│   其他节点      │
│  - 接收消息     │
│  - 更新缓存     │
└─────────────────┘
```

## 实现步骤

### 1. 创建缓存广播器模块

文件：`internal/joyue/cache_broadcast.go`

功能：
- 监听 `StateBroadcast` 事件
- 维护本地缓存（map）
- 通过 P2P PubSub 广播缓存更新
- 接收其他节点的缓存更新

### 2. 集成到共识流程

在 `consensus/post_processing.go` 中：

```go
// 在 Consensus 结构体中添加
type Consensus struct {
    // ... 现有字段 ...
    cacheBroadcaster *joyue.CacheBroadcaster
}

// 在 postConsensusProcessing 中调用
func (consensus *Consensus) postConsensusProcessing(newBlock *types.Block) error {
    // ... 现有代码 ...
    
    // 处理 JOYUE 缓存广播
    if consensus.cacheBroadcaster != nil {
        receipts := consensus.Blockchain().GetReceiptsByHash(newBlock.Hash())
        if receipts != nil {
            consensus.cacheBroadcaster.ProcessBlockLogs(newBlock, receipts)
        }
    }
    
    return nil
}
```

### 3. 初始化缓存广播器

在节点启动时（`node/harmony/node.go` 或类似位置）：

```go
// 初始化缓存广播器
if nodeConfig.Joyue.CacheEnabled { // 需要添加配置项
    cacheBroadcaster, err := joyue.NewCacheBroadcaster(host, nodeConfig)
    if err != nil {
        log.Error("failed to create cache broadcaster", "error", err)
    } else {
        consensus.SetCacheBroadcaster(cacheBroadcaster)
    }
}
```

## 消息格式

### P2P 广播消息（二进制）

```
Offset | Size | Description
-------|------|------------
0      | 1    | 版本号 (0x01)
1      | 20   | 合约地址 (contractAddr)
21     | 32   | 状态键 (key)
53     | 8    | 版本号 (version, uint64, big-endian)
61     | 4    | 值长度 (valueLen, uint32, big-endian)
65     | N    | 值内容 (value)
```

### StateBroadcast 事件

```solidity
event StateBroadcast(
    address indexed contractAddr,
    bytes32 indexed key,
    uint256 value,
    uint64 version
);
```

## PubSub Topic

Topic 命名：`joyue-cache-shard-{shardID}`

- Shard 0: `joyue-cache-shard-0`
- Shard 1: `joyue-cache-shard-1`
- ...

## 使用示例

### 查询缓存

```go
// 获取缓存值
value, version, ok := cacheBroadcaster.GetCache(contractAddr, key)
if ok {
    fmt.Printf("Value: %x, Version: %d\n", value, version)
} else {
    fmt.Println("Cache miss")
}
```

### 合约中触发缓存更新

```solidity
contract MyMaster is JoyueVerifier {
    function updateState(bytes32 key, uint256 value) external {
        _setUint(key, value);  // 这会自动 emit StateBroadcast 事件
    }
}
```

## 配置项

需要在 `internal/configs/node/config.go` 中添加：

```go
type ConfigType struct {
    // ... 现有字段 ...
    Joyue struct {
        AutoDeployEnabled bool
        DeployPrivateKey  string
        OtherShardRPCs    string
        CacheEnabled      bool  // 新增：是否启用缓存
    }
}
```

## 当前限制

1. **安全性**：
   - ❌ 没有消息签名验证
   - ❌ 没有防重放攻击
   - ❌ 没有访问控制

2. **性能**：
   - ⚠️ 使用简单 map，生产环境建议 LRU 缓存
   - ⚠️ 没有缓存过期机制
   - ⚠️ 没有批量更新优化

3. **功能**：
   - ⚠️ 只在同一分片内广播
   - ⚠️ 没有跨分片缓存同步
   - ⚠️ 没有缓存持久化

## 未来改进

1. **安全性增强**：
   - 添加消息签名（使用节点私钥）
   - 实现防重放（nonce 或时间戳）
   - 添加访问控制列表

2. **性能优化**：
   - 使用 LRU 缓存替代 map
   - 实现批量广播（合并多个更新）
   - 添加消息压缩

3. **功能扩展**：
   - 跨分片缓存同步
   - 缓存持久化（数据库）
   - 缓存预热机制

4. **监控和调试**：
   - 缓存命中率统计
   - 广播延迟监控
   - 缓存大小监控
   - 调试日志

## 测试建议

1. **单元测试**：
   - 测试事件解析
   - 测试消息序列化/反序列化
   - 测试缓存更新逻辑

2. **集成测试**：
   - 多节点 P2P 广播测试
   - 缓存同步测试
   - 性能压力测试

3. **端到端测试**：
   - 完整流程测试（合约更新 → 事件 → 缓存 → 广播 → 同步）

