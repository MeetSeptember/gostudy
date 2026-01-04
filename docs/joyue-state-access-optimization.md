# JOYUE 主合约状态获取优化方案

## 当前实现的问题

### 1. RPC Oracle
- **问题**: 每次查询都需要网络 I/O，延迟高（通常 50-200ms）
- **适用场景**: 需要绝对最新状态，可容忍延迟

### 2. P2P 缓存
- **问题**: 可能有延迟（P2P 广播需要时间）
- **适用场景**: 快速读取，可容忍轻微延迟

## 优化方案

### 方案 1: 增强 P2P 缓存（推荐 ⭐⭐⭐⭐⭐）

**核心思想**: 让 P2P 缓存更实时，减少延迟

#### 实现要点：

1. **优先级广播**
   - 主合约状态更新时，立即通过 P2P 广播（不等待区块确认）
   - 使用高优先级消息通道

2. **多节点订阅**
   - 每个节点订阅多个其他分片的节点
   - 冗余订阅提高可靠性

3. **增量更新**
   - 只广播变化的状态，不广播整个状态
   - 减少网络带宽

4. **版本号验证**
   - 缓存条目包含版本号
   - 只接受更高版本号的更新

#### 优势：
- ✅ 性能好（本地读取）
- ✅ 实时性较好（P2P 广播通常 < 100ms）
- ✅ 无需额外配置
- ✅ 利用现有 P2P 基础设施

#### 劣势：
- ❌ 仍有轻微延迟（P2P 广播时间）
- ❌ 依赖 P2P 网络稳定性

---

### 方案 2: 混合方案（缓存 + RPC 回退）⭐⭐⭐⭐

**核心思想**: 优先使用缓存，缓存未命中或过期时使用 RPC

#### 实现要点：

1. **智能缓存策略**
   ```go
   type CacheEntry struct {
       Value     []byte
       Version   uint64
       Timestamp time.Time  // 添加时间戳
       TTL       time.Duration  // 缓存有效期
   }
   ```

2. **缓存失效策略**
   - 时间过期：超过 TTL 的缓存视为过期
   - 版本过期：如果知道最新版本号，可以判断缓存是否过期

3. **RPC 回退**
   - 缓存未命中 → RPC 查询
   - 缓存过期 → RPC 查询并更新缓存

#### 优势：
- ✅ 性能好（大部分查询命中缓存）
- ✅ 数据新鲜（RPC 回退确保最新）
- ✅ 自动降级（缓存失败时使用 RPC）

#### 劣势：
- ❌ 实现复杂度较高
- ❌ 需要维护缓存失效逻辑

---

### 方案 3: 状态订阅机制 ⭐⭐⭐⭐

**核心思想**: 节点主动订阅其他分片的状态变化

#### 实现要点：

1. **订阅协议**
   - 节点向其他分片的节点发送订阅请求
   - 订阅特定合约的状态变化

2. **推送更新**
   - 主合约状态更新时，立即推送给订阅者
   - 使用 P2P 消息推送

3. **订阅管理**
   - 维护订阅列表
   - 处理订阅者上线/下线

#### 优势：
- ✅ 实时性好（推送机制）
- ✅ 性能好（本地缓存）
- ✅ 可扩展（支持多个订阅者）

#### 劣势：
- ❌ 需要实现新的 P2P 协议
- ❌ 需要处理订阅者管理

---

### 方案 4: 预取机制 ⭐⭐⭐

**核心思想**: 在交易执行前预取需要的跨分片状态

#### 实现要点：

1. **交易分析**
   - 分析交易需要哪些跨分片状态
   - 在执行前预取这些状态

2. **批量预取**
   - 批量预取多个状态
   - 减少网络往返次数

3. **预取缓存**
   - 预取的状态存入缓存
   - 交易执行时从缓存读取

#### 优势：
- ✅ 减少执行时的等待时间
- ✅ 可以批量优化

#### 劣势：
- ❌ 需要分析交易依赖
- ❌ 可能预取不需要的状态（浪费）

---

### 方案 5: 状态证明（Merkle Proof）⭐⭐⭐

**核心思想**: 使用 Merkle 证明验证跨分片状态

#### 实现要点：

1. **状态树**
   - 每个分片维护状态 Merkle 树
   - 定期发布状态根

2. **证明生成**
   - 查询状态时，返回状态值和 Merkle 证明
   - 验证证明的有效性

3. **轻客户端支持**
   - 不需要同步整个状态
   - 只需要状态根

#### 优势：
- ✅ 安全性高（可验证）
- ✅ 轻量级（不需要完整状态）

#### 劣势：
- ❌ 实现复杂度高
- ❌ 需要修改共识协议

---

## 推荐方案

### 短期方案（1-2 周）: 增强 P2P 缓存

1. **优化 P2P 广播**
   - 使用高优先级消息通道
   - 立即广播状态更新（不等待区块确认）

2. **多节点订阅**
   - 每个节点订阅多个其他分片的节点
   - 提高可靠性

3. **版本号验证**
   - 确保只接受更高版本号的更新

### 中期方案（1-2 月）: 混合方案

1. **实现智能缓存**
   - 添加时间戳和 TTL
   - 实现缓存失效策略

2. **RPC 回退**
   - 缓存未命中或过期时使用 RPC
   - 自动更新缓存

### 长期方案（3-6 月）: 状态订阅机制

1. **实现订阅协议**
   - 定义 P2P 订阅消息格式
   - 实现订阅管理

2. **推送更新**
   - 主合约状态更新时立即推送
   - 支持批量推送

---

## 实现建议

### 阶段 1: 优化 P2P 缓存（立即开始）

```go
// 在 ProcessBlockLogs 中，立即广播状态更新
func (cb *CacheBroadcaster) ProcessBlockLogs(block *types.Block, receipts types.Receipts) {
    // ... 现有逻辑 ...
    
    // 立即广播（不等待区块确认）
    if err := cb.broadcastCacheUpdateImmediate(entry); err != nil {
        // 错误处理
    }
}

// 高优先级广播
func (cb *CacheBroadcaster) broadcastCacheUpdateImmediate(entry *CacheEntry) error {
    // 使用高优先级消息通道
    // 立即发送，不等待确认
}
```

### 阶段 2: 实现混合方案（1-2 周后）

```go
type CacheEntry struct {
    Value     []byte
    Version   uint64
    Timestamp time.Time
    TTL       time.Duration
}

func (cb *CacheBroadcaster) GetCacheWithFallback(
    contractAddr common.Address,
    key common.Hash,
    shardID uint32,
) ([]byte, uint64, bool) {
    // 1. 先查缓存
    value, version, ok := cb.GetCache(contractAddr, key)
    if ok {
        // 检查是否过期
        if !cb.isExpired(contractAddr, key) {
            return value, version, true
        }
    }
    
    // 2. 缓存未命中或过期，使用 RPC
    rpcOracle := GetGlobalRpcOracle()
    if rpcOracle != nil {
        value, version, ok := rpcOracle.QueryContractState(shardID, contractAddr, key)
        if ok {
            // 更新缓存
            cb.SetCacheLocal(contractAddr, key, value, version)
            return value, version, true
        }
    }
    
    return nil, 0, false
}
```

### 阶段 3: 实现订阅机制（1-2 月后）

```go
// 订阅消息
type StateSubscribeMessage struct {
    SubscriberShardID uint32
    ContractAddr      common.Address
    Keys              []common.Hash
}

// 推送更新
type StateUpdateMessage struct {
    ContractAddr common.Address
    Key          common.Hash
    Value        []byte
    Version      uint64
}
```

---

## 性能对比

| 方案 | 延迟 | 吞吐量 | 实现复杂度 | 推荐度 |
|------|------|--------|------------|--------|
| 当前 RPC Oracle | 50-200ms | 低 | 低 | ⭐⭐ |
| 当前 P2P 缓存 | 10-100ms | 高 | 中 | ⭐⭐⭐ |
| 增强 P2P 缓存 | 5-50ms | 高 | 中 | ⭐⭐⭐⭐⭐ |
| 混合方案 | 5-200ms | 高 | 高 | ⭐⭐⭐⭐ |
| 订阅机制 | 5-20ms | 高 | 高 | ⭐⭐⭐⭐ |
| 预取机制 | 0-50ms | 中 | 高 | ⭐⭐⭐ |
| 状态证明 | 50-200ms | 中 | 很高 | ⭐⭐⭐ |

---

## 结论

**推荐采用"增强 P2P 缓存"方案**，因为：
1. ✅ 性能好（延迟低，吞吐量高）
2. ✅ 实现相对简单（基于现有 P2P 基础设施）
3. ✅ 无需额外配置
4. ✅ 可逐步优化（后续可升级为混合方案）

如果需要绝对最新状态，可以保留 RPC Oracle 作为回退机制。

