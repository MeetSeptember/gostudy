# FruitStore 跨分片调用测试指南

## 前置条件

1. 已部署以下合约：
   - `FruitStore` (分片 0)
   - `CurrencyContract` (分片 1)
   - `PointsContract` (分片 1)
   - `JoyueCrossShardExecutor` (分片 1)

2. 记录合约地址：
   - FruitStore 地址
   - CurrencyContract 地址
   - PointsContract 地址
   - Executor 地址

## 快速测试（使用脚本）

### 1. 设置环境变量

```bash
export FRUIT_STORE_ADDR="0x你的FruitStore地址"
export CURRENCY_CONTRACT_ADDR="0x你的CurrencyContract地址"
export POINTS_CONTRACT_ADDR="0x你的PointsContract地址"
export EXECUTOR_ADDR="0x你的Executor地址"
export USER_ADDR="0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6"  # 部署者地址
export PRIVATE_KEY="3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6"
```

### 2. 运行测试脚本

```bash
./test-fruit-store.sh
```

脚本会自动执行以下步骤：
1. 给用户充值货币（分片 1）
2. 查询用户货币余额
3. 查询用户积分余额
4. 添加水果到商店（分片 0）
5. 查询水果信息
6. 购买水果（触发跨分片调用）
7. 等待跨分片回调完成
8. 查询订单状态
9. 验证货币余额变化
10. 验证积分余额变化

## 手动测试（单步执行）

### 步骤 1: 给用户充值货币（分片 1）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9502" \
  --to "0x你的CurrencyContract地址" \
  --sig "deposit(address,uint256)()" \
  --arg "0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6" \
  --arg "1000000" \
  --send \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --wait
```

### 步骤 2: 查询用户货币余额（分片 1，读操作）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9502" \
  --to "0x你的CurrencyContract地址" \
  --sig "getBalance(address)(uint256)" \
  --arg "0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6"
```

### 步骤 3: 查询用户积分余额（分片 1，读操作）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9502" \
  --to "0x你的PointsContract地址" \
  --sig "getBalance(address)(uint256)" \
  --arg "0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6"
```

### 步骤 4: 添加水果到商店（分片 0，写操作）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9500" \
  --to "0x你的FruitStore地址" \
  --sig "addFruit(string,uint256,uint256,uint256)()" \
  --arg "apple" \
  --arg "100" \
  --arg "10" \
  --arg "100" \
  --send \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --wait
```

参数说明：
- `"apple"`: 水果名称
- `100`: 价格（货币单位）
- `10`: 购买后获得的积分
- `100`: 库存数量

### 步骤 5: 查询水果信息（分片 0，读操作）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9500" \
  --to "0x你的FruitStore地址" \
  --sig "getFruit(string)(string,uint256,uint256,uint256,bool)" \
  --arg "apple"
```

### 步骤 6: 购买水果（分片 0，写操作，触发跨分片调用）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9500" \
  --to "0x你的FruitStore地址" \
  --sig "buyFruit(string,uint256)(uint256)" \
  --arg "apple" \
  --arg "2" \
  --send \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --wait \
  --gas 500000
```

参数说明：
- `"apple"`: 要购买的水果名称
- `2`: 购买数量

**注意**：这个操作会触发跨分片调用：
1. 向分片 1 的 CurrencyContract 发送 `deduct` 请求（扣除货币）
2. 向分片 1 的 PointsContract 发送 `addPoints` 请求（增加积分）
3. 等待两个回调完成后，订单状态才会更新为完成

### 步骤 7: 等待跨分片回调完成

跨分片调用是异步的，需要等待几秒钟让回调完成。可以等待 5-10 秒后再查询。

### 步骤 8: 查询订单状态（分片 0，读操作）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9500" \
  --to "0x你的FruitStore地址" \
  --sig "getOrder(uint256)(address,string,uint256,uint256,uint256,uint8,uint64)" \
  --arg "1"
```

返回结果说明：
- `address`: 购买者地址
- `string`: 水果名称
- `uint256`: 数量
- `uint256`: 总价格
- `uint256`: 总积分
- `uint8`: 订单状态（0=pending, 1=completed, 2=failed）
- `uint64`: 创建时间戳

### 步骤 9: 验证货币余额变化（分片 1，读操作）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9502" \
  --to "0x你的CurrencyContract地址" \
  --sig "getBalance(address)(uint256)" \
  --arg "0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6"
```

应该看到余额减少了 200（100 * 2）。

### 步骤 10: 验证积分余额变化（分片 1，读操作）

```bash
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9502" \
  --to "0x你的PointsContract地址" \
  --sig "getBalance(address)(uint256)" \
  --arg "0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6"
```

应该看到积分增加了 20（10 * 2）。

## 查询聚合请求状态（高级）

如果需要查看跨分片请求的详细状态：

```bash
# 查询聚合请求状态（需要知道 aggregatedRequestId）
go run cmd/joyue-call/main.go \
  --rpc "http://127.0.0.1:9500" \
  --to "0x你的FruitStore地址" \
  --sig "getAggregatedRequestStatus(uint256)(address,uint256,uint256,uint256)" \
  --arg "1"
```

## 常见问题

### 1. 交易失败：Gas 不足

增加 gas limit：
```bash
--gas 1000000
```

### 2. 跨分片回调未完成

- 检查 Executor 是否已部署在目标分片
- 检查 RPC 连接是否正常
- 等待更长时间（10-20 秒）

### 3. 余额未变化

- 确认跨分片回调已完成（查询订单状态）
- 检查 Executor 地址是否正确
- 检查目标分片的合约地址是否正确

## 测试流程总结

```
分片 0 (FruitStore)                   分片 1 (Currency/Points)
     |                                       |
     | 1. 添加水果                            |
     | 2. 购买水果 (buyFruit)                 |
     |    |                                   |
     |    |--- 跨分片请求 1: deduct() ------->|
     |    |--- 跨分片请求 2: addPoints() ---->|
     |    |                                   |
     |    |<-- 回调 1: onCrossShardCallback --|
     |    |<-- 回调 2: onCrossShardCallback --|
     |    |                                   |
     | 3. 订单状态更新为 completed            |
     |                                       |
     | 4. 查询订单状态                        |
```

## 注意事项

1. **异步回调**：跨分片调用是异步的，需要等待回调完成
2. **Gas 限制**：购买水果操作需要足够的 gas（建议 500,000+）
3. **地址格式**：所有地址必须包含 `0x` 前缀
4. **分片 ID**：确保 RPC URL 指向正确的分片

