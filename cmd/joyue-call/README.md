## joyue-call：合约调用小工具（读/写）

这个工具用于快速调用合约：

- 默认：`eth_call`（读，不改链上状态）
- `--send`：发送交易（写，改链上状态）

### 参数说明

- `--rpc`：RPC URL（例如 `http://127.0.0.1:9500`）
- `--to`：合约地址（0x...）
- `--sig`：函数签名，格式：`函数名(参数类型1,参数类型2)(返回类型1,返回类型2)`
- `--arg`：参数值（可多次指定，按顺序对应 sig 中的参数）
- `--send`：发送交易（写操作），默认 false 表示只读
- `--private-key`：私钥 hex（不带 0x，仅在 `--send` 时需要）
- `--wait`：发送交易后等待 receipt 并打印状态
- `--gas`：gas limit（默认 300000）
- `--data`：直接传完整 calldata（0x...），传了会忽略 `--sig/--arg`

---

## JOYUE 系统使用示例

### 1) 读取合约状态（读操作）

#### 读取 JoyueMaster 的 `masterShardId()`：

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to 0xMASTER_ADDR \
  --sig "masterShardId()(uint32)"
```

#### 读取 JoyueAgent 的 `initialized` 状态：

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9501 \
  --to 0xAGENT_ADDR \
  --sig "initialized()(bool)"
```

#### 读取 JoyueAgent 的 `master` 地址：

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9501 \
  --to 0xAGENT_ADDR \
  --sig "master()(address)"
```

#### 查询 JoyueMaster 的结果（方案A）：

```bash
# 假设 requestId 是 0x1234...
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to 0xMASTER_ADDR \
  --sig "getResult(bytes32)(bool,address,uint32,bytes)" \
  --arg 0x1234...
```

输出示例：
```
decoded=bool=true, address=0x..., uint32=1, bytes=0x...
```

---

### 2) 调用合约方法（写操作）

#### 初始化 JoyueAgent：

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9501 \
  --to 0xAGENT_ADDR \
  --sig "initialize(address,uint32,uint32)()" \
  --arg 0xMASTER_ADDR \
  --arg 0 \
  --arg 1 \
  --send \
  --private-key <hex> \
  --wait
```

#### 调用 JoyueAgent.execute()（在 agent shard 上执行）：

```bash
# 在 shard1 上调用 agent.execute()
# master_ 参数：master 合约地址
# payload 参数：业务数据（hex 格式，例如 "0x1234"）
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9501 \
  --to 0xAGENT_ADDR \
  --sig "execute(address,bytes)()" \
  --arg 0xMASTER_ADDR \
  --arg 0x1234 \
  --send \
  --private-key <hex> \
  --wait
```

**注意**：
- 这个调用会 emit `AgentResult` 事件
- `joyue-result-relayer` 会监听到这个事件并转发到 master shard
- 你可以在 master shard 上调用 `getResult(requestId)` 查询结果

#### 在 master shard 上调用 agent.execute()：

```bash
# 如果 master shard（shard0）也部署了 agent，也可以在这里调用
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to 0xAGENT_ADDR \
  --sig "execute(address,bytes)()" \
  --arg 0xMASTER_ADDR \
  --arg 0x5678 \
  --send \
  --private-key <hex> \
  --wait
```

---

### 3) 完整流程示例

假设你已经：
1. 部署了 master 合约（在 shard0）
2. 部署了 agent 合约（在所有分片，包括 shard0）
3. 启动了 `joyue-result-relayer`

#### 步骤 1：在 shard1 上调用 agent.execute()

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9501 \
  --to 0xAGENT_ADDR \
  --sig "execute(address,bytes)()" \
  --arg 0xMASTER_ADDR \
  --arg 0x48656c6c6f576f726c64 \
  --send \
  --private-key <hex> \
  --wait
```

输出会包含 `tx=0x...`，这是交易 hash。

#### 步骤 2：等待 relayer 转发（几秒钟）

`joyue-result-relayer` 会自动监听到 `AgentResult` 事件，并调用 master 的 `submitAgentResult()`。

#### 步骤 3：在 master shard 上查询结果

你需要从步骤 1 的交易 receipt 中获取 `requestId`（从 `AgentResult` 事件的 logs 中解析），或者：

```bash
# 假设 requestId 是 0xabcd...
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to 0xMASTER_ADDR \
  --sig "getResult(bytes32)(bool,address,uint32,bytes)" \
  --arg 0xabcd...
```

---

### 4) 高级：直接传 calldata

如果某些类型解析不支持，你可以自己准备 calldata：

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to 0xCONTRACT \
  --data 0x1234abcd...
```

---

## 参数类型支持

工具支持以下 Solidity 类型：

- `address`：地址（0x...）
- `uint8/uint16/uint32/uint64/uint256`：无符号整数（十进制或 0x 十六进制）
- `int8/int16/int32/int64/int256`：有符号整数
- `bool`：布尔值（true/false/1/0）
- `bytes`：动态字节数组（0x...）
- `bytes32`：固定长度字节数组（0x...）
- `string`：字符串

---

## 常见问题

### Q: 如何获取 requestId？

A: `agent.execute()` 的返回值就是 `requestId`，但工具目前不解析返回值。你可以：
1. 查看交易 receipt 的 logs，找到 `AgentResult` 事件
2. 从事件的 `topics[2]` 中提取 `requestId`（bytes32）

### Q: 如何查看交易 receipt？

A: 使用 `--wait` 参数，工具会等待 receipt 并打印 `status` 和 `block`。

### Q: bytes 参数怎么传？

A: 使用 hex 格式，例如：`--arg 0x1234` 或 `--arg 0x48656c6c6f576f726c64`（"HelloWorld" 的 hex）。


