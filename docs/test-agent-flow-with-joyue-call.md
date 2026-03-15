# Agent 流程测试文档（使用 joyue-call 工具）

## 概述

本测试文档使用 `joyue-call` 工具来测试 `FruitShopAgentV2` 的完整流程，包括：
1. 部署合约
2. 初始化状态
3. Agent 从缓存读取状态
4. Agent 生成 Intent 并发送
5. Master 处理 Intent
6. 验证状态更新

## 前置条件

### 1. 启动本地链

```bash
# 启动本地链（假设使用 3 个分片：shard0, shard1, shard2）
./test/debug.sh test/configs/localnet-3-shards.txt

# 等待链启动完成，确认所有分片都在出块
# 检查日志：tmp_log/9000.log, tmp_log/9002.log, tmp_log/9004.log
```

### 2. 准备私钥

```bash
# 使用一个测试私钥（确保账户有足够的余额）
# 例如：0x1f84c95ac16e6a50f08d44c7bde7aff8742212fda6e4321fde48bf83bef266dc
# 对应地址：0xA5241513DA9F4463F1d4874b548dFBAC29D91f34
export PRIVATE_KEY=1f84c95ac16e6a50f08d44c7bde7aff8742212fda6e4321fde48bf83bef266dc
export USER_ADDRESS=0xA5241513DA9F4463F1d4874b548dFBAC29D91f34
```

### 3. 启动 Relayer（如果需要跨分片）

```bash
# 启动 shard0 的 Relayer（监听 shard0，转发到其他分片）
go run cmd/joyue-relayer/main.go \
  --rpc http://127.0.0.1:9500 \
  --shard-id 0 \
  --private-key $PRIVATE_KEY \
  --target-shard-rpcs 1=http://127.0.0.1:9502,2=http://127.0.0.1:9504 &

# 启动 shard1 的 Relayer（监听 shard1，转发到其他分片）
go run cmd/joyue-relayer/main.go \
  --rpc http://127.0.0.1:9502 \
  --shard-id 1 \
  --private-key $PRIVATE_KEY \
  --target-shard-rpcs 0=http://127.0.0.1:9500,2=http://127.0.0.1:9504 &
```

## 部署步骤

### 步骤 1: 部署缓存合约（可选）

如果使用预编译合约（0x64），则不需要部署 `JoyueCache` 合约。`JoyueLib` 会直接调用预编译合约。

### 步骤 2: 部署 Master 合约（shard1）

假设 Master 合约部署在 **shard1**：

```bash
# 部署 FruitShopMasterV2
# 注意：需要先编译合约，然后使用部署工具
# 假设部署后的地址为：0xFruitShopMasterV2

# 部署 WalletMasterV2
# 假设部署后的地址为：0xWalletMasterV2

# 部署 PointsMasterV2
# 假设部署后的地址为：0xPointsMasterV2
```

**验证部署**：

```bash
# 检查 FruitShopMasterV2 是否已部署
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xFruitShopMasterV2 \
  --sig "APPLE()(bytes32)"

# 检查库存（应该返回初始值）
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xFruitShopMasterV2 \
  --sig "getUint(bytes32)(uint256,uint64)" \
  --arg 0x<APPLE_STOCK_KEY>
```

### 步骤 3: 初始化用户余额和积分（shard1）

```bash
# 设置用户余额（会发出 StateBroadcast 事件）
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xWalletMasterV2 \
  --sig "setBalance(address,uint256)()" \
  --arg $USER_ADDRESS \
  --arg 1000 \
  --send \
  --private-key $PRIVATE_KEY \
  --wait

# 设置用户积分（会发出 StateBroadcast 事件）
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xPointsMasterV2 \
  --sig "setPoints(address,uint256)()" \
  --arg $USER_ADDRESS \
  --arg 0 \
  --send \
  --private-key $PRIVATE_KEY \
  --wait
```

**验证初始化**：

```bash
# 检查余额
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xWalletMasterV2 \
  --sig "getUint(bytes32)(uint256,uint64)" \
  --arg 0x<BALANCE_KEY>

# 检查积分
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xPointsMasterV2 \
  --sig "getUint(bytes32)(uint256,uint64)" \
  --arg 0x<POINTS_KEY>
```

### 步骤 4: 部署 Agent 合约（shard0）

假设 Agent 合约部署在 **shard0**：

```bash
# 部署 FruitShopAgentV2
# 构造函数参数：
# - cacheAddr: 0x0000000000000000000000000000000000000064 (预编译合约地址，可选)
# - fruitShopMasterAddr: 0xFruitShopMasterV2
# - walletMasterAddr: 0xWalletMasterV2
# - pointsMasterAddr: 0xPointsMasterV2
# - masterShardID: 1 (Master 所在的分片)
# 假设部署后的地址为：0xFruitShopAgentV2
```

**验证部署**：

```bash
# 检查 Agent 合约是否已部署
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0xFruitShopAgentV2 \
  --sig "fruitShopMaster()(address)"

# 检查 masterShardID
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0xFruitShopAgentV2 \
  --sig "masterShardID()(uint32)"
```

## 测试流程

### 测试 1: Agent 生成 Intent（单分片测试）

**目的**：验证 Agent 能够正确生成 Guards 和 Deltas，并发出 Intent。

#### 步骤 1.1: 计算 APPLE 的哈希值

```bash
# 计算 "apple" 的 keccak256 哈希值
echo -n "apple" | sha256sum
# 或者使用 Solidity 的方式：keccak256("apple")
# 假设结果为：0x3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835120b5b8b1c5b5b5b
export APPLE_HASH=0x3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835120b5b8b1c5b5b5b
```

#### 步骤 1.2: 调用 Agent.buyFruit

```bash
# 调用 FruitShopAgentV2.buyFruit(fruitType, quantity)
# 注意：buyFruit 返回 (bool, Guard[], Delta[])，但工具可能无法完全解析复杂类型
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0xFruitShopAgentV2 \
  --sig "buyFruit(bytes32,uint256)(bool,JoyueLib.Guard[],JoyueLib.Delta[])" \
  --arg $APPLE_HASH \
  --arg 2 \
  --send \
  --private-key $PRIVATE_KEY \
  --wait
```

**预期输出**：
```
from=0xA5241513DA9F4463F1d4874b548dFBAC29D91f34
to=0xFruitShopAgentV2
tx=0x...
calldata=0x...
status=1
block=...
```

#### 步骤 1.3: 检查日志

查看 shard0 的日志（`tmp_log/9000.log`），应该看到：

```
[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Parameters
  bundleId=0x...
  agentShardId=0
  masterShardId=1
  agentContract=0xFruitShopAgentV2
  masterContract=0xFruitShopMasterV2
  epoch=0
  agentBlockNumber=...
  timestampMs=...
  detailCount=1
  roundIdBase=...
  participantsCount=3

[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: IntentDetail
  detailIndex=0
  txHash=0x...
  sender=0xFruitShopAgentV2
  nonce=...
  req.targetAddr=0xFruitShopMasterV2
  req.selector=0x...
  req.argsLen=...
  guardsCount=2  # 库存 Guard + 余额 Guard
  deltasCount=3  # 扣库存 Delta + 扣余额 Delta + 加积分 Delta

[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Participant
  participantIndex=0
  participant=0xFruitShopMasterV2
  participantIndex=1
  participant=0xWalletMasterV2
  participantIndex=2
  participant=0xPointsMasterV2
```

### 测试 2: 验证缓存读取

**目的**：验证 Agent 能够从缓存读取状态。

#### 步骤 2.1: 检查缓存是否已更新

```bash
# 查询缓存中的库存（通过预编译合约 0x64）
# 注意：需要先确认 StateBroadcast 事件已发出并更新缓存
go run cmd/joyue-query-cache/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0xFruitShopMasterV2 \
  --key 0x<STOCK_KEY>

# 查询缓存中的余额
go run cmd/joyue-query-cache/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0xWalletMasterV2 \
  --key 0x<BALANCE_KEY>

# 查询缓存中的积分
go run cmd/joyue-query-cache/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0xPointsMasterV2 \
  --key 0x<POINTS_KEY>
```

### 测试 3: 验证 Intent 发出

**目的**：验证 Agent 能够通过预编译合约发出 Intent。

#### 步骤 3.1: 检查 CrossShardRequest 事件

查看 shard0 的日志，应该看到：

```
[JOYUE] emitted CrossShardRequest event (Event + Relayer)
  requestId=...
  shardID=1
  to=0x74  # Executor 预编译地址
  callbackAddr=0xFruitShopMasterV2
  caller=0xFruitShopAgentV2
```

#### 步骤 3.2: 检查 Relayer 是否转发

查看 Relayer 日志，应该看到：

```
[JOYUE Relayer] received CrossShardRequest event
  requestId=...
  targetShard=1
  target=0x74
  callbackAddr=0xFruitShopMasterV2

[JOYUE Relayer] sent transaction successfully
  txHash=...
  targetShard=1
  target=0x74
```

### 测试 4: Master 处理 Intent（完整流程测试）

**注意**：此测试需要先恢复 `handleProcessBundleWithOneRetry` 的主流程执行。

#### 步骤 4.1: 恢复主流程执行

在 `core/vm/contracts_joyue_coordinator.go` 中，取消注释 `executeProcessBundleWithOneRetry` 的调用。

#### 步骤 4.2: 调用 Agent.buyFruit

```bash
# 再次调用 buyFruit
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0xFruitShopAgentV2 \
  --sig "buyFruit(bytes32,uint256)(bool,JoyueLib.Guard[],JoyueLib.Delta[])" \
  --arg $APPLE_HASH \
  --arg 2 \
  --send \
  --private-key $PRIVATE_KEY \
  --wait
```

#### 步骤 4.3: 检查 Master 处理日志

查看 shard1 的日志（`tmp_log/9002.log`），应该看到：

```
[JOYUE Executor] RunWriteCapable called
  executorAddr=0x74
  caller=0x...
  selector=0x2b5d76a4

[JOYUE Executor] handleExecuteAndCallback called
  sourceShardID=0
  callbackAddr=0xFruitShopMasterV2
  target=0xFruitShopMasterV2

[JOYUE Coordinator] handleProcessBundleWithOneRetry called
  bundleId=0x...
  detailCount=1
  participantsCount=3
```

#### 步骤 4.4: 验证状态更新

```bash
# 验证库存已减少
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xFruitShopMasterV2 \
  --sig "getUint(bytes32)(uint256,uint64)" \
  --arg 0x<APPLE_STOCK_KEY>
# 预期：从 10 减少到 8（如果买了 2 个）

# 验证余额已减少
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xWalletMasterV2 \
  --sig "getUint(bytes32)(uint256,uint64)" \
  --arg 0x<BALANCE_KEY>
# 预期：从 1000 减少到 960（如果买了 2 个苹果，每个 20）

# 验证积分已增加
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to 0xPointsMasterV2 \
  --sig "getUint(bytes32)(uint256,uint64)" \
  --arg 0x<POINTS_KEY>
# 预期：从 0 增加到 10（如果买了 2 个苹果，每个 5 积分）
```

## 完整测试脚本示例

### 脚本 1: 初始化测试环境

```bash
#!/bin/bash
# init-test-env.sh

# 设置变量
export PRIVATE_KEY=1f84c95ac16e6a50f08d44c7bde7aff8742212fda6e4321fde48bf83bef266dc
export USER_ADDRESS=0xA5241513DA9F4463F1d4874b548dFBAC29D91f34
export FRUIT_SHOP_MASTER=0xFruitShopMasterV2
export WALLET_MASTER=0xWalletMasterV2
export POINTS_MASTER=0xPointsMasterV2
export FRUIT_SHOP_AGENT=0xFruitShopAgentV2
export APPLE_HASH=0x3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835120b5b8b1c5b5b5b

# 设置用户余额
echo "设置用户余额..."
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to $WALLET_MASTER \
  --sig "setBalance(address,uint256)()" \
  --arg $USER_ADDRESS \
  --arg 1000 \
  --send \
  --private-key $PRIVATE_KEY \
  --wait

# 设置用户积分
echo "设置用户积分..."
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9502 \
  --to $POINTS_MASTER \
  --sig "setPoints(address,uint256)()" \
  --arg $USER_ADDRESS \
  --arg 0 \
  --send \
  --private-key $PRIVATE_KEY \
  --wait

echo "初始化完成！"
```

### 脚本 2: 执行购买测试

```bash
#!/bin/bash
# test-buy-fruit.sh

# 设置变量（与 init-test-env.sh 相同）
export PRIVATE_KEY=1f84c95ac16e6a50f08d44c7bde7aff8742212fda6e4321fde48bf83bef266dc
export FRUIT_SHOP_AGENT=0xFruitShopAgentV2
export APPLE_HASH=0x3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835120b5b8b1c5b5b5b

# 调用 buyFruit
echo "调用 buyFruit..."
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to $FRUIT_SHOP_AGENT \
  --sig "buyFruit(bytes32,uint256)(bool,JoyueLib.Guard[],JoyueLib.Delta[])" \
  --arg $APPLE_HASH \
  --arg 2 \
  --send \
  --private-key $PRIVATE_KEY \
  --wait

echo "购买完成！请检查日志和状态更新。"
```

## 常见问题排查

### 问题 1: 交易失败（status=0）

**检查步骤**：

1. **检查合约是否已部署**：
```bash
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0xFruitShopAgentV2 \
  --sig "fruitShopMaster()(address)"
```

2. **检查参数是否正确**：
```bash
# 先使用 eth_call 测试（不加 --send）
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0xFruitShopAgentV2 \
  --sig "buyFruit(bytes32,uint256)(bool,JoyueLib.Guard[],JoyueLib.Delta[])" \
  --arg $APPLE_HASH \
  --arg 2
```

3. **检查 gas 是否足够**：
```bash
# 增加 gas limit
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0xFruitShopAgentV2 \
  --sig "buyFruit(bytes32,uint256)(bool,JoyueLib.Guard[],JoyueLib.Delta[])" \
  --arg $APPLE_HASH \
  --arg 2 \
  --send \
  --private-key $PRIVATE_KEY \
  --gas 500000 \
  --wait
```

### 问题 2: 缓存读取失败

**检查步骤**：

1. **确认 StateBroadcast 事件已发出**：
```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9502 \
  --contract 0xFruitShopMasterV2 \
  --event "StateBroadcast(address,bytes32,uint256,uint64)" \
  --from-block 0 \
  --verbose
```

2. **检查缓存预编译合约是否可用**：
```bash
# 直接调用预编译合约 0x64
go run cmd/joyue-call/main.go \
  --rpc http://127.0.0.1:9500 \
  --to 0x0000000000000000000000000000000000000064 \
  --data 0x<CONTRACT_ADDR><KEY>
```

### 问题 3: Relayer 未转发

**检查步骤**：

1. **确认 Relayer 已启动**：
```bash
ps aux | grep joyue-relayer
```

2. **检查 Relayer 日志**：
查看 Relayer 的输出，确认是否收到事件。

3. **手动检查事件**：
```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x000000000000000000000000000000000000006D \
  --event "CrossShardRequest(uint256,uint32,address,bytes,uint256,address,bytes4)" \
  --from-block 0 \
  --verbose
```

## 测试检查清单

- [ ] 本地链已启动并正常运行
- [ ] Relayer 已启动（如果需要跨分片）
- [ ] Master 合约已部署（shard1）
- [ ] Agent 合约已部署（shard0）
- [ ] 用户余额和积分已初始化
- [ ] 缓存已更新（StateBroadcast 事件已发出）
- [ ] Agent.buyFruit 调用成功
- [ ] 日志中看到参数打印（测试模式）
- [ ] CrossShardRequest 事件已发出
- [ ] Relayer 已转发交易（如果需要）
- [ ] Master 已处理 Intent（完整流程）
- [ ] 状态已更新（库存、余额、积分）

## 下一步

完成测试后，可以：
1. 取消注释主流程执行，进行完整的功能测试
2. 测试错误场景（库存不足、余额不足等）
3. 测试重试逻辑（Guard 验证失败后的重试）
4. 测试多交易批处理
