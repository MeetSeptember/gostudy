# JOYUE 节点自动部署代理合约方案

## 概述

当主合约（JoyueMaster）部署完成后，由节点自动向其他分片发起代理合约（JoyueAgent）部署交易，替代链下 relayer 方案。

## 实现方案

### 1. 检测主合约部署（必须在共识完成后）

**重要**：必须在区块达成共识并写入链后，才能触发代理合约部署。如果在区块处理过程中触发，可能会因为区块最终未被共识接受而导致误触发。

在 `consensus/post_processing.go` 的 `postConsensusProcessing` 函数中，检查已确认的区块中是否有主合约部署：

```go
// postConsensusProcessing 在共识完成后、区块已写入链后调用
func (consensus *Consensus) postConsensusProcessing(newBlock *types.Block) error {
    // ... 现有逻辑
    
    // 检查是否有新的主合约部署（仅在共识完成后）
    if consensus.NodeConfig.Joyue.AutoDeployEnabled {
        go checkAndDeployAgents(consensus, newBlock)
    }
    
    return nil
}

func checkAndDeployAgents(consensus *Consensus, block *types.Block) {
    // 1. 遍历区块中的所有交易
    for i, tx := range block.Transactions() {
        // 2. 检查是否是合约创建交易
        if tx.To() != nil {
            continue // 不是合约创建
        }
        
        // 3. 获取该交易的 receipt（区块已确认，receipt 已存在）
        receipts := consensus.Blockchain().GetReceiptsByHash(block.Hash())
        if i >= len(receipts) {
            continue
        }
        receipt := receipts[i]
        
        // 4. 检查 receipt 是否成功且包含合约地址
        if receipt.Status != 1 || receipt.ContractAddress == (common.Address{}) {
            continue
        }
        
        // 5. 检查是否是 JoyueMaster 合约（通过事件）
        if isJoyueMaster(receipt.Logs) {
            // 触发自动部署
            triggerAgentDeployment(receipt.ContractAddress, tx, receipt, consensus)
        }
    }
}
```

### 2. 识别 JoyueMaster 合约

#### 方法1：通过事件识别（推荐）

检查 receipt 中是否有 `MasterDeployed` 事件：

```go
func isJoyueMaster(logs []*types.Log) bool {
    // MasterDeployed 事件的 topic0
    masterDeployedSig := crypto.Keccak256([]byte("MasterDeployed(address,bytes32,bytes,uint32)"))[0]
    for _, log := range logs {
        if len(log.Topics) > 0 && log.Topics[0] == masterDeployedSig {
            return true
        }
    }
    return false
}
```

#### 方法2：通过字节码识别（备选）

检查合约字节码是否包含特定标识（例如函数选择器）：

```go
func isJoyueMasterCode(code []byte) bool {
    // 检查是否包含 submitAgentResult 函数选择器
    submitSig := crypto.Keccak256([]byte("submitAgentResult(bytes32,address,uint32,bytes)"))[:4]
    return bytes.Contains(code, submitSig)
}
```

### 3. 从事件中提取 agentCreationCode

如果使用事件识别，需要从 `MasterDeployed` 事件中解析 `agentCreationCode`：

```go
func extractAgentCreationCode(logs []*types.Log) ([]byte, error) {
    // 解析 MasterDeployed 事件
    // topic0: MasterDeployed sig
    // topic1: master (indexed)
    // topic2: salt (indexed)
    // data: agentCreationCode (bytes) + masterShardId (uint32)
    // ...
}
```

### 4. 向其他分片发送部署交易

在 `consensus/post_processing.go` 中实现：

```go
func triggerAgentDeployment(
    masterAddr common.Address, 
    tx *types.Transaction, 
    receipt *types.Receipt,
    consensus *Consensus,
) {
    // 1. 从 receipt.Logs 中解析 agentCreationCode
    agentCode, masterShardID, err := parseMasterDeployedEvent(receipt.Logs)
    if err != nil {
        utils.Logger().Error().Err(err).Msg("failed to parse MasterDeployed event")
        return
    }
    
    // 2. 获取其他分片的 RPC 地址（从配置读取）
    otherShards := getOtherShardRPCs(masterShardID)
    
    // 3. 向每个分片发送部署交易
    for shardID, rpcURL := range otherShards {
        go deployAgentToShard(shardID, rpcURL, agentCode, masterAddr, masterShardID)
    }
}

func deployAgentToShard(shardID uint32, rpcURL string, agentCode []byte, masterAddr common.Address, masterShardID uint32) {
    // 1. 连接目标分片 RPC
    client, err := ethclient.Dial(rpcURL)
    if err != nil {
        utils.Logger().Error().Err(err).Uint32("shard", shardID).Msg("failed to dial shard RPC")
        return
    }
    
    // 2. 获取部署私钥（从配置读取）
    privKey := getDeployPrivateKey()
    from := crypto.PubkeyToAddress(privKey.PublicKey)
    
    // 3. 构造交易
    chainID, _ := client.ChainID(context.Background())
    nonce, _ := client.PendingNonceAt(context.Background(), from)
    
    tx := types.NewTransaction(
        nonce,
        nil, // to = nil 表示合约创建
        big.NewInt(0), // value
        2500000, // gas limit
        big.NewInt(1000000000), // gas price（或使用 EIP-1559）
        agentCode, // data = creation code
    )
    
    // 4. 签名并发送
    signedTx, _ := types.SignTx(tx, types.NewEIP155Signer(chainID), privKey)
    err = client.SendTransaction(context.Background(), signedTx)
    if err != nil {
        utils.Logger().Error().Err(err).Uint32("shard", shardID).Msg("failed to send agent deployment tx")
        return
    }
    
    utils.Logger().Info().
        Uint32("shard", shardID).
        Str("tx", signedTx.Hash().Hex()).
        Msg("agent deployment tx sent")
}
```

### 5. 配置项

配置项需要添加到多个位置，以便从命令行/TOML 读取并传递到节点：

#### 5.1 在 `HarmonyConfig` 中添加（用于命令行/TOML 输入）

**文件**：`internal/configs/harmony/harmony.go`

```go
type HarmonyConfig struct {
    Version    string
    General    GeneralConfig
    Network    NetworkConfig
    // ... 其他配置
    
    Joyue      JoyueConfig  // 添加这一行
}

// 添加 JoyueConfig 结构体定义
type JoyueConfig struct {
    // 是否启用自动部署
    AutoDeployEnabled bool `toml:"auto_deploy_enabled"`
    
    // 部署私钥（hex，不带 0x）
    DeployPrivateKey string `toml:"deploy_private_key"`
    
    // 其他分片的 RPC 地址
    // 格式：shardID=rpcURL,shardID=rpcURL
    // 例如：1=http://127.0.0.1:9501,2=http://127.0.0.1:9502
    OtherShardRPCs string `toml:"other_shard_rpcs"`
}
```

**注意**：`nodeconfig.ConfigType` 中已经有 `Joyue` 字段了（在 `internal/configs/node/config.go` 第 138-149 行），所以不需要再添加。

#### 5.2 在 `cmd/config/flags.go` 中添加命令行 flag 定义

**文件**：`cmd/config/flags.go`

在文件末尾添加：

```go
// joyue flags
var (
    joyueAutoDeployEnabledFlag = cli.BoolFlag{
        Name:     "joyue.auto-deploy",
        Usage:    "是否启用 JOYUE 自动部署代理合约",
        DefValue: false,
    }
    joyueDeployPrivateKeyFlag = cli.StringFlag{
        Name:     "joyue.deploy-private-key",
        Usage:    "JOYUE 部署私钥（hex，不带 0x）",
        DefValue: "",
    }
    joyueOtherShardRPCsFlag = cli.StringFlag{
        Name:     "joyue.other-shard-rpcs",
        Usage:    "其他分片的 RPC 地址（格式：shardID=rpcURL,shardID=rpcURL）",
        DefValue: "",
    }
)

func applyJoyueFlags(cmd *cobra.Command, config *harmonyconfig.HarmonyConfig) {
    if cli.IsFlagChanged(cmd, joyueAutoDeployEnabledFlag) {
        config.Joyue.AutoDeployEnabled = cli.GetBoolFlagValue(cmd, joyueAutoDeployEnabledFlag)
    }
    if cli.IsFlagChanged(cmd, joyueDeployPrivateKeyFlag) {
        config.Joyue.DeployPrivateKey = cli.GetStringFlagValue(cmd, joyueDeployPrivateKeyFlag)
    }
    if cli.IsFlagChanged(cmd, joyueOtherShardRPCsFlag) {
        config.Joyue.OtherShardRPCs = cli.GetStringFlagValue(cmd, joyueOtherShardRPCsFlag)
    }
}
```

然后在 `getRootFlags()` 函数中添加这些 flags（找到其他 flags 定义的位置，添加）：

```go
func getRootFlags() []cli.Flag {
    flags := []cli.Flag{}
    // ... 现有 flags
    
    // 添加 joyue flags
    flags = append(flags, joyueAutoDeployEnabledFlag, joyueDeployPrivateKeyFlag, joyueOtherShardRPCsFlag)
    
    return flags
}
```

在 `applyRootFlags()` 函数中调用（`cmd/config/config.go`）：

```go
func applyRootFlags(cmd *cobra.Command, config *harmonyconfig.HarmonyConfig) {
    // ... 现有 apply 调用
    applyJoyueFlags(cmd, config)  // 添加这一行
}
```

#### 5.3 在 `cmd/harmony/main.go` 中从 HarmonyConfig 传递到 NodeConfig

**文件**：`cmd/harmony/main.go`，在 `createGlobalConfig()` 函数中：

```go
func createGlobalConfig(hc harmonyconfig.HarmonyConfig) (*nodeconfig.ConfigType, error) {
    // ... 现有代码
    
    // 传递 Joyue 配置
    nodeConfig.Joyue.AutoDeployEnabled = hc.Joyue.AutoDeployEnabled
    nodeConfig.Joyue.DeployPrivateKey = hc.Joyue.DeployPrivateKey
    nodeConfig.Joyue.OtherShardRPCs = hc.Joyue.OtherShardRPCs
    
    // ... 其他代码
    return nodeConfig, nil
}
```

#### 5.4 在代码中访问配置

在 `consensus/post_processing.go` 中，通过 `consensus.registry.GetNodeConfig()` 访问：

```go
nodeConfig := consensus.registry.GetNodeConfig()
if nodeConfig.Joyue.AutoDeployEnabled {
    // ...
}
```

### 6. 实现位置（必须在共识完成后）

**唯一正确的位置**：在 `consensus/post_processing.go` 的 `postConsensusProcessing` 函数中实现。

**为什么必须在这里**：
1. `postConsensusProcessing` 在 `commitBlock` 中调用
2. `commitBlock` 在 `_finalCommit` 中调用，此时区块已经通过 `InsertChain` 写入链
3. 只有区块被共识确认并写入链后，才能保证主合约真的部署成功了
4. 如果在区块处理过程中触发，区块可能最终不被共识接受，导致误触发

**实现方式**：

```go
func (consensus *Consensus) postConsensusProcessing(newBlock *types.Block) error {
    // ... 现有逻辑（广播区块、跨分片 receipts 等）
    
    // 检查是否有新的主合约部署（仅在共识完成后）
    if consensus.NodeConfig.Joyue.AutoDeployEnabled {
        // 异步执行，避免阻塞共识流程
        go checkAndDeployAgents(consensus, newBlock)
    }
    
    return nil
}

func checkAndDeployAgents(consensus *Consensus, block *types.Block) {
    // 从已确认的区块中读取 receipts（使用 GetReceiptsByHash）
    receipts := consensus.Blockchain().GetReceiptsByHash(block.Hash())
    if len(receipts) == 0 {
        return
    }
    
    // 遍历区块中的所有交易
    for i, tx := range block.Transactions() {
        if i >= len(receipts) {
            continue
        }
        
        receipt := receipts[i]
        
        // 检查是否是合约创建交易（to == nil）
        if tx.To() != nil {
            continue
        }
        
        // 检查 receipt 是否成功
        if receipt.Status != 1 || receipt.ContractAddress == (common.Address{}) {
            continue
        }
        
        // 检查是否是 JoyueMaster 合约（通过事件识别）
        if isJoyueMaster(receipt.Logs) {
            triggerAgentDeployment(
                receipt.ContractAddress,
                tx,
                receipt,
                consensus,
            )
        }
    }
}
```

**注意**：不能在 `core/state_processor.go` 的 `ApplyTransaction` 中实现，因为：
- 此时区块还在处理中，尚未达成共识
- 如果区块最终不被共识接受，会导致误触发
- 可能影响区块处理的性能

## 优缺点分析

### 优点

1. **自动化**：无需运行额外的 relayer 进程
2. **可靠性**：节点直接处理，减少中间环节
3. **一致性**：与区块处理逻辑集成，保证时序

### 缺点

1. **需要修改节点代码**：改动较大
2. **需要配置**：需要配置其他分片 RPC 和私钥
3. **网络依赖**：需要节点能访问其他分片的 RPC
4. **错误处理**：如果某个分片部署失败，需要重试机制

## 与现有 relayer 方案的对比

| 特性 | 节点自动部署 | 链下 relayer |
|------|------------|-------------|
| 实现复杂度 | 高（需改节点） | 低（独立工具） |
| 部署速度 | 快（区块处理时） | 稍慢（轮询） |
| 可靠性 | 高（节点保证） | 中（依赖 relayer 运行） |
| 灵活性 | 低（需重新编译） | 高（独立配置） |
| 维护成本 | 低（集成在节点） | 中（需单独维护） |

## 建议

1. **短期**：继续使用链下 relayer 方案（已实现，稳定）
2. **长期**：如果需求明确且稳定，可以考虑实现节点自动部署方案
3. **混合方案**：节点自动部署作为主要方式，relayer 作为备用/监控

## 实现步骤

1. 添加配置项（Joyue.AutoDeployEnabled, DeployPrivateKey, OtherShardRPCs）
2. 实现主合约识别逻辑（事件/字节码）
3. 实现跨分片 RPC 调用逻辑
4. 在 post-processing 或 state_processor 中集成
5. 添加错误处理和重试机制
6. 添加日志和监控

