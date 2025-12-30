# JOYUE 分组执行与结果汇总方案

## 概述

在 provider（proposer/leader）打包交易时，按照 `to` 地址的方法分组。当每组都执行完成后，汇总返回值，由节点向主合约所在地发送一个汇总交易。

## 实现方案

### 1. 交易分组逻辑

在 `consensus/consensus_block_proposing.go` 的 `ProposeNewBlock` 中，修改交易打包逻辑：

```go
// 在获取 pending transactions 后，按 to 地址和方法分组
func groupTransactionsByTarget(pendingPlainTxs map[common.Address]types.Transactions) map[string][]*types.Transaction {
    groups := make(map[string][]*types.Transaction)
    
    for _, txs := range pendingPlainTxs {
        for _, tx := range txs {
            if tx.To() == nil {
                continue // 跳过合约创建交易
            }
            
            // 生成分组 key：to地址-方法选择器（前4字节）
            to := tx.To().Hex()
            methodSelector := ""
            if len(tx.Data()) >= 4 {
                methodSelector = hex.EncodeToString(tx.Data()[:4])
            }
            groupKey := fmt.Sprintf("%s-%s", to, methodSelector)
            
            groups[groupKey] = append(groups[groupKey], tx)
        }
    }
    
    return groups
}
```

### 2. 执行分组交易并收集结果

在 `node/harmony/worker/worker.go` 中修改 `CommitSortedTransactions`：

```go
// 新增：分组执行交易
func (w *Worker) CommitGroupedTransactions(
    groups map[string][]*types.Transaction,
    coinbase common.Address,
) map[string]*GroupExecutionResult {
    results := make(map[string]*GroupExecutionResult)
    
    for groupKey, txs := range groups {
        result := &GroupExecutionResult{
            GroupKey: groupKey,
            TxHashes: make([]common.Hash, 0),
            ReturnValues: make([][]byte, 0),
            SuccessCount: 0,
            FailCount: 0,
        }
        
        // 执行组内所有交易
        for _, tx := range txs {
            if w.current.gasPool.Gas() < params.TxGas {
                break
            }
            
            // 执行交易
            w.current.state.SetTxContext(tx.Hash(), common.Hash{}, len(w.current.txs))
            receipt, returnValue, err := w.commitTransactionWithReturn(tx, coinbase)
            
            result.TxHashes = append(result.TxHashes, tx.Hash())
            
            if err == nil && receipt.Status == 1 {
                result.SuccessCount++
                result.ReturnValues = append(result.ReturnValues, returnValue)
            } else {
                result.FailCount++
                result.ReturnValues = append(result.ReturnValues, nil)
            }
        }
        
        results[groupKey] = result
    }
    
    return results
}

// 修改 commitTransaction 以返回执行结果
func (w *Worker) commitTransactionWithReturn(
    tx *types.Transaction, coinbase common.Address,
) (*types.Receipt, []byte, error) {
    snap := w.current.state.Snapshot()
    gasUsed := w.current.header.GasUsed()
    
    // 使用 Call 方式执行，获取返回值
    var returnValue []byte
    if tx.To() != nil {
        // 构造 CallMsg
        from, _ := types.Sender(w.current.signer, tx)
        msg := ethereum.CallMsg{
            From:     from,
            To:       tx.To(),
            Gas:      tx.Gas(),
            GasPrice: tx.GasPrice(),
            Value:    tx.Value(),
            Data:     tx.Data(),
        }
        
        // 在临时 state 上执行 call
        vmenv := vm.NewEVM(
            core.NewEVMContext(msg, w.current.header, w.chain, &coinbase),
            w.current.state,
            w.chain.Config(),
            vm.Config{},
        )
        
        ret, _, err := vmenv.Call(
            vm.AccountRef(from),
            *tx.To(),
            tx.Data(),
            tx.Gas(),
            tx.Value(),
        )
        if err == nil {
            returnValue = ret
        }
    }
    
    // 正常执行交易
    receipt, cx, stakeMsgs, _, err := core.ApplyTransaction(
        w.chain,
        &coinbase,
        w.current.gasPool,
        w.current.state,
        w.current.header,
        tx,
        &gasUsed,
        vm.Config{},
    )
    w.current.header.SetGasUsed(gasUsed)
    
    if err != nil {
        w.current.state.RevertToSnapshot(snap)
        return nil, returnValue, err
    }
    
    if receipt == nil {
        return nil, returnValue, errNilReceipt
    }
    
    w.current.txs = append(w.current.txs, tx)
    w.current.receipts = append(w.current.receipts, receipt)
    w.current.logs = append(w.current.logs, receipt.Logs...)
    w.current.stakeMsgs = append(w.current.stakeMsgs, stakeMsgs...)
    
    if cx != nil {
        w.current.outcxs = append(w.current.outcxs, cx)
    }
    
    return receipt, returnValue, nil
}
```

### 3. 结果汇总结构

```go
type GroupExecutionResult struct {
    GroupKey     string           // 分组 key：to地址-方法选择器
    TxHashes     []common.Hash    // 组内所有交易 hash
    ReturnValues [][]byte         // 每个交易的返回值
    SuccessCount int              // 成功数量
    FailCount    int              // 失败数量
}

type AggregatedResult struct {
    GroupKey      string           // 分组 key
    FromShardID   uint32          // 来源分片
    TotalCount    int             // 总交易数
    SuccessCount  int             // 成功数
    ReturnValues  [][]byte        // 所有返回值
    TxHashes      []common.Hash   // 所有交易 hash
    AggregatedData []byte         // 汇总后的数据（可自定义格式）
}
```

### 4. 汇总结果并发送到主合约

在 `consensus/post_processing.go` 中，区块处理完成后汇总并发送：

```go
func (consensus *Consensus) postConsensusProcessing(newBlock *types.Block) error {
    // ... 现有逻辑
    
    // 汇总分组执行结果并发送到主合约
    if consensus.NodeConfig.Joyue.GroupedExecutionEnabled {
        go aggregateAndSendResults(consensus, newBlock)
    }
    
    return nil
}

func aggregateAndSendResults(consensus *Consensus, block *types.Block) {
    // 1. 从 worker 获取分组执行结果
    worker := consensus.registry.GetWorker()
    groupResults := worker.GetGroupExecutionResults() // 需要在 worker 中存储
    
    if len(groupResults) == 0 {
        return
    }
    
    // 2. 按分组汇总结果
    aggregated := make(map[string]*AggregatedResult)
    for groupKey, result := range groupResults {
        // 检查是否是 JoyueAgent 相关的调用
        if !isJoyueAgentGroup(groupKey) {
            continue
        }
        
        // 解析 groupKey 获取 to 地址和方法
        to, method := parseGroupKey(groupKey)
        
        // 汇总返回值
        aggregatedData := aggregateReturnValues(result.ReturnValues)
        
        aggregated[groupKey] = &AggregatedResult{
            GroupKey:      groupKey,
            FromShardID:   consensus.ShardID,
            TotalCount:    len(result.TxHashes),
            SuccessCount:  result.SuccessCount,
            ReturnValues:  result.ReturnValues,
            TxHashes:      result.TxHashes,
            AggregatedData: aggregatedData,
        }
    }
    
    // 3. 向主合约发送汇总交易
    for groupKey, agg := range aggregated {
        // 获取主合约地址（从配置或合约状态读取）
        masterAddr, masterShardID := getMasterContractInfo(agg)
        
        // 构造汇总交易 calldata
        // 假设主合约有方法：submitGroupedResults(bytes32 groupKey, uint32 fromShardId, bytes[] returnValues, bytes aggregatedData)
        calldata, err := encodeSubmitGroupedResults(
            groupKey,
            agg.FromShardID,
            agg.ReturnValues,
            agg.AggregatedData,
        )
        if err != nil {
            utils.Logger().Error().Err(err).Msg("failed to encode aggregated results")
            continue
        }
        
        // 发送到主合约所在分片
        sendAggregatedResultToMaster(
            masterAddr,
            masterShardID,
            calldata,
            consensus,
        )
    }
}

func sendAggregatedResultToMaster(
    masterAddr common.Address,
    masterShardID uint32,
    calldata []byte,
    consensus *Consensus,
) {
    // 1. 获取主分片 RPC
    masterRPC := getShardRPC(masterShardID)
    if masterRPC == "" {
        utils.Logger().Error().Uint32("shard", masterShardID).Msg("master shard RPC not configured")
        return
    }
    
    // 2. 连接 RPC
    client, err := ethclient.Dial(masterRPC)
    if err != nil {
        utils.Logger().Error().Err(err).Msg("failed to dial master RPC")
        return
    }
    
    // 3. 获取部署私钥
    privKey := getDeployPrivateKey()
    from := crypto.PubkeyToAddress(privKey.PublicKey)
    
    // 4. 构造并发送交易
    chainID, _ := client.ChainID(context.Background())
    nonce, _ := client.PendingNonceAt(context.Background(), from)
    
    tx := types.NewTransaction(
        nonce,
        &masterAddr,
        big.NewInt(0),
        500000, // gas limit
        big.NewInt(1000000000), // gas price
        calldata,
    )
    
    signedTx, _ := types.SignTx(tx, types.NewEIP155Signer(chainID), privKey)
    err = client.SendTransaction(context.Background(), signedTx)
    if err != nil {
        utils.Logger().Error().Err(err).Msg("failed to send aggregated result tx")
        return
    }
    
    utils.Logger().Info().
        Str("tx", signedTx.Hash().Hex()).
        Str("master", masterAddr.Hex()).
        Uint32("masterShard", masterShardID).
        Msg("aggregated result tx sent")
}
```

### 5. 主合约接口

需要在 `JoyueMaster.sol` 中添加汇总方法：

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
        successCount: countSuccess(returnValues), // 需要实现
        returnValues: returnValues,
        aggregatedData: aggregatedData,
        exists: true
    });
    
    emit GroupedResultSubmitted(groupKey, fromShardId, returnValues.length);
}

function getGroupedResult(bytes32 groupKey) external view returns (
    bool exists,
    uint32 fromShardId,
    uint256 totalCount,
    uint256 successCount,
    bytes[] memory returnValues,
    bytes memory aggregatedData
) {
    GroupedResult storage r = _groupedResults[groupKey];
    return (
        r.exists,
        r.fromShardId,
        r.totalCount,
        r.successCount,
        r.returnValues,
        r.aggregatedData
    );
}
```

### 6. 汇总策略

返回值汇总可以有多种策略：

```go
// 策略1：简单拼接
func aggregateReturnValuesSimple(values [][]byte) []byte {
    var result []byte
    for _, v := range values {
        if v != nil {
            result = append(result, v...)
        }
    }
    return result
}

// 策略2：JSON 格式
func aggregateReturnValuesJSON(values [][]byte) []byte {
    type Result struct {
        Index int    `json:"index"`
        Value string `json:"value"`
    }
    results := make([]Result, 0)
    for i, v := range values {
        if v != nil {
            results = append(results, Result{
                Index: i,
                Value: hex.EncodeToString(v),
            })
        }
    }
    jsonData, _ := json.Marshal(results)
    return jsonData
}

// 策略3：自定义格式（例如：长度前缀 + 数据）
func aggregateReturnValuesCustom(values [][]byte) []byte {
    var result []byte
    for _, v := range values {
        if v != nil {
            // 长度前缀（4字节）
            lenBytes := make([]byte, 4)
            binary.BigEndian.PutUint32(lenBytes, uint32(len(v)))
            result = append(result, lenBytes...)
            result = append(result, v...)
        }
    }
    return result
}
```

## 实现位置

### 修改点1：交易分组（`consensus/consensus_block_proposing.go`）

在 `ProposeNewBlock` 中，获取 pending transactions 后：

```go
// 按 to 地址和方法分组
groupedTxs := groupTransactionsByTarget(pendingPlainTxs)

// 传递给 worker 执行
if err := worker.CommitGroupedTransactions(groupedTxs, beneficiary); err != nil {
    return nil, err
}
```

### 修改点2：分组执行（`node/harmony/worker/worker.go`）

- 添加 `CommitGroupedTransactions` 方法
- 修改 `commitTransaction` 为 `commitTransactionWithReturn`
- 在 `environment` 中添加 `groupResults` 字段存储结果

### 修改点3：结果汇总（`consensus/post_processing.go`）

在 `postConsensusProcessing` 中添加汇总逻辑

## 配置项

```go
Joyue struct {
    // 是否启用分组执行
    GroupedExecutionEnabled bool
    
    // 分组策略
    GroupingStrategy string // "by-to-method" | "by-to" | "custom"
    
    // 汇总策略
    AggregationStrategy string // "simple" | "json" | "custom"
    
    // 主合约地址和分片
    MasterContractAddress string
    MasterShardID        uint32
    
    // 其他分片 RPC（用于发送汇总交易）
    OtherShardRPCs string
}
```

## 优缺点分析

### 优点

1. **批量处理**：一次汇总多个执行结果，减少跨分片交易数量
2. **原子性**：同一组的交易在同一区块执行，结果一致
3. **效率高**：减少网络开销和 gas 消耗

### 缺点

1. **实现复杂**：需要修改多个核心模块
2. **延迟**：需要等待组内所有交易执行完成
3. **错误处理**：如果组内部分交易失败，需要决定如何处理

## 与现有方案的对比

| 特性 | 分组执行汇总 | 单个事件转发 |
|------|------------|------------|
| 交易数量 | 少（批量汇总） | 多（每个结果一个交易） |
| 实现复杂度 | 高 | 中 |
| 延迟 | 高（需等待组完成） | 低（立即转发） |
| Gas 消耗 | 低（批量） | 高（多次） |

## 建议

1. **短期**：继续使用现有的事件转发方案（简单可靠）
2. **中期**：如果批量处理需求明确，可以考虑实现分组执行
3. **混合**：可以同时支持两种方案，根据场景选择

