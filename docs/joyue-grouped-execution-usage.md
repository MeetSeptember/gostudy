# JOYUE 分组执行与结果汇总使用指南

## 概述

JOYUE 分组执行功能允许在 proposer 打包区块时，按照 `to` 地址和方法对交易进行分组，执行后汇总返回值，并向主合约发送汇总交易。

## 配置项

### 命令行参数

```bash
--joyue.grouped-execution              # 是否启用分组执行（默认：false）
--joyue.grouping-strategy              # 分组策略：by-to-method | by-to（默认：by-to-method）
--joyue.aggregation-strategy           # 汇总策略：simple | json | custom（默认：simple）
--joyue.master-contract-address        # 主合约地址（hex，带 0x）
--joyue.master-shard-id                # 主合约所在分片 ID（默认：0）
```

### 配置示例

```bash
./harmony \
  --joyue.grouped-execution \
  --joyue.grouping-strategy=by-to-method \
  --joyue.aggregation-strategy=simple \
  --joyue.master-contract-address=0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --joyue.master-shard-id=0 \
  --joyue.other-shard-rpcs="0=http://127.0.0.1:9500,1=http://127.0.0.1:9502"
```

## 工作流程

### 1. 交易分组

在 `consensus/consensus_block_proposing.go` 的 `ProposeNewBlock` 中：
- 从交易池获取待处理交易
- 根据 `grouping-strategy` 对交易进行分组：
  - `by-to-method`：按 `to` 地址和方法选择器（前 4 字节）分组
  - `by-to`：仅按 `to` 地址分组

### 2. 分组执行

在 `node/harmony/worker/worker.go` 的 `CommitGroupedTransactions` 中：
- 对每个分组内的交易按 nonce 排序
- 依次执行交易，收集返回值
- 记录成功/失败数量

### 3. 结果汇总

在 `consensus/post_processing.go` 的 `aggregateAndSendResults` 中：
- 从 worker 获取分组执行结果
- 根据 `aggregation-strategy` 汇总返回值：
  - `simple`：简单拼接所有返回值
  - `json`：JSON 格式汇总
  - `custom`：长度前缀 + 数据格式
- 构造汇总交易并发送到主合约

## 汇总策略详解

### Simple（简单拼接）

```go
// 将所有返回值直接拼接
result = append(result, value1...)
result = append(result, value2...)
```

### JSON（JSON 格式）

```json
[
  {"index": 0, "value": "0x..."},
  {"index": 1, "value": "0x..."}
]
```

### Custom（自定义格式）

```
[4字节长度][数据][4字节长度][数据]...
```

## 主合约接口

需要在 `JoyueMaster.sol` 中添加以下方法：

```solidity
struct GroupedResult {
    bytes32 groupKey;
    uint32 fromShardId;
    uint256 totalCount;
    uint256 successCount;
    bytes[] returnValues;
    bytes aggregatedData;
}

mapping(bytes32 => GroupedResult) private _groupedResults;

event GroupedResultSubmitted(bytes32 indexed groupKey, uint32 indexed fromShardId, uint256 totalCount);

function submitGroupedResults(
    bytes32 groupKey,
    uint32 fromShardId,
    bytes[] calldata returnValues,
    bytes calldata aggregatedData
) external {
    require(!_groupedResults[groupKey].exists, "duplicate groupKey");
    
    _groupedResults[groupKey] = GroupedResult({
        groupKey: groupKey,
        fromShardId: fromShardId,
        totalCount: returnValues.length,
        successCount: countSuccess(returnValues),
        returnValues: returnValues,
        aggregatedData: aggregatedData,
        exists: true
    });
    
    emit GroupedResultSubmitted(groupKey, fromShardId, returnValues.length);
}
```

## 注意事项

1. **返回值获取**：当前实现通过 EVM Call 获取返回值，需要在临时快照上执行，可能影响性能
2. **ABI 编码**：`encodeSubmitGroupedResults` 使用了简化版本，实际应该使用完整的 ABI 编码
3. **错误处理**：如果组内部分交易失败，汇总结果仍会发送，但会记录失败数量
4. **Gas 限制**：汇总交易的 gas limit 固定为 500000，可能需要根据实际情况调整

## 日志

分组执行相关的日志：

- `[JOYUE] aggregated result tx sent successfully` - 汇总交易发送成功
- `[JOYUE] failed to send aggregated result tx` - 汇总交易发送失败
- `[JOYUE] master contract address not configured` - 主合约地址未配置

## 测试建议

1. 部署主合约并配置地址
2. 启用分组执行功能
3. 发送多个相同 `to` 地址和方法的交易
4. 检查主合约是否收到汇总结果
5. 验证汇总数据的正确性

