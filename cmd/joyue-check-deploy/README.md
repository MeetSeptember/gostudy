## joyue-check-deploy：检查合约部署是否成功

这个小工具用来快速判断一笔“合约创建交易”是否部署成功。

### 用法

最常用（不等待）：

```bash
go run ./cmd/joyue-check-deploy \
  --rpc http://127.0.0.1:9501 \
  --tx 0xYOUR_TX_HASH
```

如果交易刚发出，receipt 可能暂时查不到，可以等待：

```bash
go run ./cmd/joyue-check-deploy \
  --rpc http://127.0.0.1:9501 \
  --tx 0xYOUR_TX_HASH \
  --wait \
  --timeout 90s
```

### 判定规则

- **成功**：`status=1` 且 `eth_getCode(contractAddress)` 返回非空
- **失败**：`status=0`
- **未确认**：查不到 receipt（可能还没上链/节点同步中）


