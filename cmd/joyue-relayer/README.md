## JOYUE Relayer（第一阶段：跨分片合约部署）

这个 relayer 的作用是跑通最小闭环：

> 在 shard0 部署 Master（emit 事件） → relayer 监听到事件 → 在其它 shard 自动部署 Agent

### 1. Master 事件定义

Master 合约在 constructor 中 emit：

- `MasterDeployed(master, salt, agentCodeHash, masterShardId)`

relayer 通过 **topic0（事件签名 hash）** 扫日志，不需要提前知道 master 合约地址。

### 2. relayer 需要的参数

- `--master-rpc`：master shard RPC（HTTP）
- `--shards`：所有 shard RPC 列表：`id=url,id=url,...`
- `--private-key`：relayer EOA 私钥（同一私钥在每个 shard 上都要有余额）
- 不需要 `--agent-bin`：agent 的完整 creation code 由用户在部署 master 时传入，并由 master 事件携带。

### 3. 生成 agent-bin（solc）

你仍然需要用 `solc` 得到 agent 的 `.bin`（creation bytecode），因为用户要把“完整 creation code”
作为参数传给 master 的 constructor。

推荐方式（最稳）：输出到 `.bin` 文件：

```bash
mkdir -p /tmp/joyue-solc-out
solc --bin -o /tmp/joyue-solc-out contracts/joyue/JoyueAgent.sol
cat /tmp/joyue-solc-out/JoyueAgent.bin
```

### 4. 运行示例

```bash
go run ./cmd/joyue-relayer \
  --master-rpc http://127.0.0.1:9500 \
  --shards 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501,2=http://127.0.0.1:9502,3=http://127.0.0.1:9503 \
  --private-key <hex> \
  --confirmations 2 \
  --wait-receipt
```

它会把处理进度写到 `joyue-relayer-state.json`，避免重复处理。

### 5. 注意事项（重要）

- 本实现 **不要求 agent 地址一致**（每个 shard 的 agent 地址可能不同）。
- 如果你多次运行 relayer、且换了 state 文件/清空 state 文件，可能会在同一 shard 部署多个 agent（不同地址）。
  - 想彻底幂等，需要后续引入 CREATE2 或 factory 方案（第二阶段再做）。

### 6. 重要风险提示

- **把“完整 creation code（bytes）”放进事件日志，会非常耗 gas**，并且可能因为交易/区块大小限制导致部署失败。
  这是为了“最小实现演示功能”，后续更推荐：事件里只放 codeHash + code 的存储地址（IPFS/链上 blob/等）。


