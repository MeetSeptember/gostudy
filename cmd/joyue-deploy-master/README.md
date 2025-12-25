## joyue-deploy-master：部署 JoyueMaster（支持传入完整 agentCreationCode）

这是一个 Go 小工具，用于**发起合约部署请求**（发送合约创建交易）：

> JoyueMaster(byte32 salt, bytes agentCreationCode, uint32 masterShardId)

其中 `agentCreationCode` 是“完整的 Agent creation code”（也就是未来 relayer 要转发到其它分片的 tx.data）。

### 1) 先用 solc 生成 .bin

```bash
mkdir -p /tmp/joyue-solc-out
solc --bin -o /tmp/joyue-solc-out contracts/joyue/JoyueMaster.sol
solc --bin -o /tmp/joyue-solc-out contracts/joyue/JoyueAgent.sol
```

会生成：

- `/tmp/joyue-solc-out/JoyueMaster.bin`
- `/tmp/joyue-solc-out/JoyueAgent.bin`

### 2) 部署到 shard0（示例）

```bash
go run ./cmd/joyue-deploy-master \
  --rpc http://127.0.0.1:9500 \
  --private-key <hex> \
  --master-bin-file /tmp/joyue-solc-out/JoyueMaster.bin \
  --agent-code-file /tmp/joyue-solc-out/JoyueAgent.bin \
  --master-shard-id 0 \
  --salt 0x01 \
  --wait
```

输出包括：

- `tx=0x...`
- `status=1`
- `contract=0x...`（JoyueMaster 地址）

### 3) 重要提示

把完整 `agentCreationCode` 放进 master constructor 参数并 emit 到日志，会非常耗 gas，
可能触发交易/区块限制导致部署失败。第一阶段为了演示可以这样做，后续建议改成：

- 事件里只放 codeHash + code 的存储位置（例如 IPFS / 链上 blob / 其它存储方案）

### 4) 更省事的用法（直接内置 bin）

如果你嫌每次都要传 `--master-bin-file/--agent-code-file` 麻烦，可以把 `.bin` 的 hex
直接粘到 `cmd/joyue-deploy-master/main.go` 顶部的：

- `embeddedJoyueMasterBinHex`
- `embeddedJoyueAgentCreationCodeHex`

之后你只需要：

```bash
go run ./cmd/joyue-deploy-master --rpc http://127.0.0.1:9500 --private-key <hex>
```


