## joyue-call：合约调用小工具（读/写）

这个工具用于快速调用合约：

- 默认：`eth_call`（读，不改链上状态）
- `--send`：发送交易（写，改链上状态）

### 1) 读（eth_call）

例：读取 JoyueMaster 的 `masterShardId()`：

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to 0xMASTER_ADDR \
  --sig "masterShardId()(uint32)"
```

工具会打印：

- calldata
- raw 返回值（hex）
- decoded（如果 sig 带了返回类型）

### 2) 写（发送交易）

例：调用 JoyueAgent 的 `initialize(address,uint32,uint32)`：

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
  --wait \
  --timeout 90s
```

### 3) 高级：直接传 calldata

如果某些类型解析不支持，你可以自己准备 calldata：

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to 0xCONTRACT \
  --data 0x1234abcd...
```


