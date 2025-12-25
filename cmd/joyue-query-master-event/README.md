## joyue-query-master-event：查询 Master 部署时 emit 的事件

这个工具用于扫描 `JoyueMaster` 在 constructor 里 emit 的 `MasterDeployed` 事件，并解析输出：

- master 地址
- salt
- masterShardId
- agentCreationCode 长度
- agentCreationCodeHash（keccak256）

可选：把 `agentCreationCode` 导出到文件（hex，不带 0x），方便后续比对或直接复用。

### 运行示例（扫 shard0）

```bash
go run ./cmd/joyue-query-master-event \
  --rpc http://127.0.0.1:9500 \
  --from-block 0
```

### 只过滤某个 master 地址

```bash
go run ./cmd/joyue-query-master-event \
  --rpc http://127.0.0.1:9500 \
  --from-block 0 \
  --master 0xYourMasterAddress
```

### 导出 agentCreationCode 到文件

```bash
go run ./cmd/joyue-query-master-event \
  --rpc http://127.0.0.1:9500 \
  --from-block 0 \
  --out-agent-code /tmp/agentCreationCode.hex
```


