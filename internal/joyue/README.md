# JOYUE P2P 缓存广播模块

## 功能概述

实现合约状态的 P2P 缓存系统：
1. **监听事件**：监听合约发出的 `StateBroadcast` 事件
2. **更新本地缓存**：将状态更新保存到本地缓存
3. **P2P 广播**：通过 PubSub 将缓存更新广播到其他节点
4. **接收更新**：接收其他节点的缓存更新，同步本地缓存

## 架构设计

```
合约状态修改
    ↓
StateBroadcast 事件
    ↓
节点监听事件 (post_processing.go)
    ↓
CacheBroadcaster.ProcessBlockLogs()
    ↓
更新本地缓存 + P2P 广播
    ↓
其他节点接收并更新本地缓存
```

## 使用方法

### 1. 初始化缓存广播器

在节点启动时初始化：

```go
import "github.com/harmony-one/harmony/internal/joyue"

// 在节点初始化时
cacheBroadcaster, err := joyue.NewCacheBroadcaster(host, nodeConfig)
if err != nil {
    log.Fatal("failed to create cache broadcaster:", err)
}
defer cacheBroadcaster.Close()
```

### 2. 在区块处理时调用

在 `consensus/post_processing.go` 的 `postConsensusProcessing` 函数中：

```go
func (consensus *Consensus) postConsensusProcessing(newBlock *types.Block) error {
    // ... 现有代码 ...
    
    // 处理 JOYUE 缓存广播
    if consensus.cacheBroadcaster != nil {
        receipts := consensus.Blockchain().GetReceiptsByHash(newBlock.Hash())
        if receipts != nil {
            consensus.cacheBroadcaster.ProcessBlockLogs(newBlock, receipts)
        }
    }
    
    // ... 其他代码 ...
}
```

### 3. 查询缓存

```go
// 获取缓存值
value, version, ok := cacheBroadcaster.GetCache(contractAddr, key)
if ok {
    // 使用缓存值
    fmt.Printf("Value: %x, Version: %d\n", value, version)
}
```

## 消息格式

P2P 广播消息格式（二进制）：

```
[1 byte: 版本号]
[20 bytes: 合约地址]
[32 bytes: 状态键]
[8 bytes: 版本号 (uint64, big-endian)]
[4 bytes: 值长度 (uint32, big-endian)]
[N bytes: 值内容]
```

## PubSub Topic

Topic 格式：`joyue-cache-shard-{shardID}`

例如：
- Shard 0: `joyue-cache-shard-0`
- Shard 1: `joyue-cache-shard-1`

## 注意事项

1. **当前实现不考虑安全性**：
   - 没有消息签名验证
   - 没有防重放攻击
   - 没有访问控制

2. **性能考虑**：
   - 本地缓存使用简单的 map，生产环境建议使用 LRU 缓存
   - P2P 广播是异步的，不阻塞区块处理

3. **跨分片**：
   - 当前实现只在同一分片内广播
   - 跨分片缓存同步需要额外实现

4. **事件过滤**：
   - 只处理 `StateBroadcast` 事件
   - 事件签名：`keccak256("StateBroadcast(address,bytes32,uint256,uint64)")`

## 未来改进

1. **安全性**：
   - 添加消息签名验证
   - 实现防重放机制
   - 添加访问控制

2. **性能优化**：
   - 使用 LRU 缓存替代简单 map
   - 批量广播多个更新
   - 压缩消息内容

3. **跨分片支持**：
   - 实现跨分片缓存同步
   - 支持多分片订阅

4. **监控和指标**：
   - 添加缓存命中率统计
   - 添加广播延迟监控
   - 添加缓存大小监控

