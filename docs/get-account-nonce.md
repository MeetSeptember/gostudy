# 获取账户 Nonce 的方法

## 方法 1：使用 hmy CLI（推荐）

### 基本语法

```bash
hmy --node="<RPC地址>" blockchain transaction-count <账户地址> --shard <分片编号>
```

### 参数说明

- `--node`: Harmony 节点的 RPC 地址
- `<账户地址>`: 要查询的账户地址（支持 `0x...` 或 `one1...` 格式）
- `--shard`: 目标分片编号（0, 1, 2, ...）

### 示例

#### 查询 shard 0 上的 nonce

```bash
# 使用 one1 格式地址
hmy --node="http://localhost:9500" blockchain transaction-count one1d2rngmem4x2c6zxsjjz29dlah0jzkr0k2n88wc --shard 0

# 使用 0x 格式地址
hmy --node="http://localhost:9500" blockchain transaction-count 0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6 --shard 0
```

#### 查询 shard 1 上的 nonce

```bash
# 注意：需要连接到 shard 1 的 RPC（端口 9502）
hmy --node="http://localhost:9502" blockchain transaction-count one1d2rngmem4x2c6zxsjjz29dlah0jzkr0k2n88wc --shard 1

# 或者使用 shard 0 的 RPC，但指定 --shard 1
hmy --node="http://localhost:9500" blockchain transaction-count one1d2rngmem4x2c6zxsjjz29dlah0jzkr0k2n88wc --shard 1
```

### 输出示例

```json
{
  "nonce": 5
}
```

## 方法 2：使用 RPC 调用

### Harmony RPC API

```bash
curl -X POST "http://localhost:9500" \
  -H "Content-Type: application/json" \
  --data '{
    "jsonrpc": "2.0",
    "method": "hmy_getAccountNonce",
    "params": ["<账户地址>", "latest"],
    "id": 1
  }'
```

**示例**：

```bash
curl -X POST "http://localhost:9500" \
  -H "Content-Type: application/json" \
  --data '{
    "jsonrpc": "2.0",
    "method": "hmy_getAccountNonce",
    "params": ["one1d2rngmem4x2c6zxsjjz29dlah0jzkr0k2n88wc", "latest"],
    "id": 1
  }'
```

**响应**：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": 5
}
```

### 以太坊兼容 API

```bash
curl -X POST "http://localhost:9500" \
  -H "Content-Type: application/json" \
  --data '{
    "jsonrpc": "2.0",
    "method": "eth_getTransactionCount",
    "params": ["<账户地址>", "latest"],
    "id": 1
  }'
```

**示例**：

```bash
curl -X POST "http://localhost:9500" \
  -H "Content-Type: application/json" \
  --data '{
    "jsonrpc": "2.0",
    "method": "eth_getTransactionCount",
    "params": ["0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6", "latest"],
    "id": 1
  }'
```

**响应**：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": "0x5"
}
```

注意：`eth_getTransactionCount` 返回的是十六进制格式的 nonce。

## 方法 3：使用 Go 代码

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/ethclient"
)

func main() {
    // 连接到 Harmony 节点
    client, err := ethclient.DialContext(context.Background(), "http://localhost:9500")
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    // 账户地址
    address := common.HexToAddress("0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6")

    // 获取 nonce（latest）
    nonce, err := client.PendingNonceAt(context.Background(), address)
    if err != nil {
        log.Fatal(err)
    }

    fmt.Printf("Nonce: %d\n", nonce)
}
```

## 注意事项

1. **Nonce 是分片特定的**：每个账户在每个分片上都有独立的 nonce
2. **Pending Nonce vs Confirmed Nonce**：
   - `PendingNonceAt`: 包含 pending 交易的 nonce（用于发送新交易）
   - `NonceAt`: 已确认区块的 nonce
3. **地址格式**：
   - Harmony 支持 `0x...` 和 `one1...` 两种格式
   - 两种格式指向同一个地址
4. **RPC 端口**：
   - Shard 0: `9500`
   - Shard 1: `9502` (P2P 端口 9002 + 500)
   - Shard N: `9000 + N*2 + 500`

## 常见问题

### Q: 为什么不同分片的 nonce 不同？

A: 因为每个分片都有独立的状态，账户在每个分片上的 nonce 是独立的。

### Q: 如何获取 pending nonce？

A: 使用 `eth_getTransactionCount` 时，将 `"latest"` 改为 `"pending"`：

```bash
curl -X POST "http://localhost:9500" \
  -H "Content-Type: application/json" \
  --data '{
    "jsonrpc": "2.0",
    "method": "eth_getTransactionCount",
    "params": ["0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6", "pending"],
    "id": 1
  }'
```

### Q: 如何查询特定区块的 nonce？

A: 使用区块号代替 `"latest"`：

```bash
curl -X POST "http://localhost:9500" \
  -H "Content-Type: application/json" \
  --data '{
    "jsonrpc": "2.0",
    "method": "eth_getTransactionCount",
    "params": ["0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6", "0x100"],
    "id": 1
  }'
```

## 相关命令

- `hmy balances <address>` - 查询账户余额（所有分片）
- `hmy blockchain latest-headers` - 查询最新区块头
- `hmy blockchain transaction-by-hash <txHash>` - 查询交易详情

