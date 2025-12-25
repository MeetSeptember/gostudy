## joyue-result-relayer（方案A：Agent -> Relayer -> Master）

这个 relayer 用于把各分片 JoyueAgent 的 `AgentResult` 事件转发到 shard0（或你指定的 master shard）的 JoyueMaster。

### 合约接口要求

Agent（`contracts/joyue/JoyueAgent.sol`）事件：

- `event AgentResult(address indexed master, bytes32 indexed requestId, address indexed user, bytes payload);`

Master（`contracts/joyue/JoyueMaster.sol`）方法：

- `submitAgentResult(bytes32 requestId, address sender, uint32 fromShardId, bytes payload)`

### 运行示例

假设 master 在 shard0：

```bash
go run ./cmd/joyue-result-relayer \
  --master-rpc http://127.0.0.1:9500 \
  --master 0xMASTER_ADDR \
  --agent-shards 1=http://127.0.0.1:9501,2=http://127.0.0.1:9502 \
  --private-key <hex> \
  --from-block 0 \
  --wait-receipt=true
```

### 输出与状态

会输出每条转发的 master txHash，并在本地生成：

- `joyue-result-relayer-state.json`

用来记录每个 agent shard 的扫描高度与已处理日志，避免重复转发。


