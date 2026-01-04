# joyue-query-agent-event

查询 JoyueAgent 合约部署时发出的 `Initialized` 事件。

## 功能

- 查询所有 Agent 合约的 `Initialized` 事件
- 显示 `master`, `masterShardId`, `agentShardId`, `cacheAddr`, `rpcOracleAddr` 等信息
- 支持按 `master` 地址或 `agent` 地址过滤

## 使用方法

### 基本查询（查询所有 Initialized 事件）

```bash
go run cmd/joyue-query-agent-event/main.go \
  --rpc="http://127.0.0.1:9500" \
  --from-block=0
```

### 查询指定 Master 的 Agent 事件

```bash
go run cmd/joyue-query-agent-event/main.go \
  --rpc="http://127.0.0.1:9500" \
  --from-block=0 \
  --master="0x36d9eaa5eCF358e2653B489046b0d3dF385B13A6"
```

### 查询指定 Agent 合约的事件

```bash
go run cmd/joyue-query-agent-event/main.go \
  --rpc="http://127.0.0.1:9501" \
  --from-block=0 \
  --agent="0x63169D7049Fa7cfA842179AeC59CED7B00B32733"
```

### 查询指定区块范围

```bash
go run cmd/joyue-query-agent-event/main.go \
  --rpc="http://127.0.0.1:9500" \
  --from-block=100 \
  --to-block=200
```

## 输出示例

```
事件签名: Initialized(address,uint32,uint32,address,address)
事件 Topic0: 0x...
查询区块范围: 0 - 150

=== Initialized ===
block=105 tx=0x...
agent=0x63169D7049Fa7cfA842179AeC59CED7B00B32733
master=0x36d9eaa5eCF358e2653B489046b0d3dF385B13A6
masterShardId=0
agentShardId=1
cacheAddr=0x61a049be2326C44637b6d6AfdF92480f67DCf076
rpcOracleAddr=0x63169D7049Fa7cfA842179AeC59CED7B00B32733
```

## 参数说明

- `--rpc`: RPC 端点 URL（默认: `http://127.0.0.1:9500`）
- `--from-block`: 起始区块号（默认: 0）
- `--to-block`: 结束区块号（默认: 最新区块，0 表示最新）
- `--master`: 过滤指定 Master 地址的事件（可选）
- `--agent`: 过滤指定 Agent 地址的事件（可选）

