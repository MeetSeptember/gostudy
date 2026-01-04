# JOYUE 通用合约部署工具

将合约部署到所有指定的分片。

## 功能

- 支持嵌入的合约 bytecode（在代码中修改）
- 支持从文件或参数读取合约 bytecode（可选）
- 支持构造函数参数（简单 hex 格式）
- 并发部署到多个分片
- 等待 receipt 并记录合约地址
- **自动保存工具合约地址到配置文件**（用于 Agent 部署）

## 使用方法

### 基本用法（使用嵌入的 bytecode）

```bash
# 1. 修改代码中的 embeddedContractBinHex 常量
# 2. 直接运行部署
go run cmd/joyue-deploy-all-shards/main.go \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --shards "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502"
```

### 部署工具合约并保存地址

```bash
# 部署 JoyueMockCache（会自动保存到配置文件）
go run cmd/joyue-deploy-all-shards/main.go \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --shards "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502" \
  --contract-type "cache"

# 部署 JoyueRpcOracleMock（会自动保存到配置文件）
go run cmd/joyue-deploy-all-shards/main.go \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --shards "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502" \
  --contract-type "rpc-oracle"
```

### 部署带构造函数参数的合约

```bash
# 使用简单 hex 参数（每个参数 32 字节）
go run cmd/joyue-deploy-all-shards/main.go \
  --private-key "..." \
  --shards "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502" \
  --bytecode-file "contracts/joyue/JoyueMaster.bin" \
  --constructor-args "0x0000000000000000000000000000000000000000000000000000000000000001,0x6080604052...,0"
```

### 使用已编译的 bytecode

```bash
# 直接提供 hex bytecode（不包含构造函数参数）
go run cmd/joyue-deploy-all-shards/main.go \
  --private-key "..." \
  --shards "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502" \
  --bytecode "608060405234801561001057600080fd5b50..."
```

## 参数说明

### 必需参数

- `--private-key`: 部署账户私钥（hex，不带 0x）
- `--shards`: 分片列表，格式：`id=url,id=url`（例如：`0=http://127.0.0.1:9500,1=http://127.0.0.1:9502`）

### 合约 bytecode（二选一）

- `--bytecode-file`: 合约 bytecode 文件路径（.bin 文件）
- `--bytecode`: 合约 bytecode hex（直接提供）

### 构造函数参数（可选）

- `--constructor-args`: 构造函数参数（逗号分隔的 hex 字符串，每个参数会被补齐到 32 字节）
- `--constructor-args-file`: 构造函数参数文件路径（JSON 格式，暂未完全实现）
- `--abi-file`: 合约 ABI 文件路径（用于编码构造函数参数，暂未完全实现）

### Gas 配置

- `--gas`: gasLimit（默认：3,500,000）
- `--gas-tip-gwei`: EIP-1559 priority fee（gwei，默认：1）

### 其他选项

- `--wait`: 是否等待 receipt 并打印合约地址（默认：true）
- `--timeout`: 等待 receipt 超时（默认：90s）
- `--concurrency`: 并发部署的分片数量（默认：3）
- `--config`: 工具合约地址配置文件路径（默认：`joyue-tool-contracts.json`）
- `--save-config`: 是否保存部署地址到配置文件（默认：true）
- `--contract-type`: 合约类型标识（`cache` 或 `rpc-oracle`，用于配置文件分类）

## 输出示例

```
部署者地址: 0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6
分片数量: 2

合约 bytecode 长度: 2334 字节
构造函数参数长度: 0 字节
完整 creation code 长度: 2334 字节

[Shard 0] 交易已发送: 0x1234...
[Shard 1] 交易已发送: 0x5678...
[Shard 0] 合约已部署: 0xabcd... (区块: 12345)
[Shard 1] 合约已部署: 0xef01... (区块: 67890)

=== 部署结果 ===
✅ Shard 0 (http://127.0.0.1:9500): 成功
   合约地址: 0xabcd...
   TxHash: 0x1234...
   区块号: 12345
✅ Shard 1 (http://127.0.0.1:9502): 成功
   合约地址: 0xef01...
   TxHash: 0x5678...
   区块号: 67890

=== 总结 ===
成功: 2/2
```

## 注意事项

1. **构造函数参数格式**：当前实现使用简单的 hex 格式，每个参数会被补齐到 32 字节。对于复杂类型（如动态数组、结构体），建议先手动编码后再传入。

2. **ABI 编码**：`--abi-file` 和 `--constructor-args-file` 功能尚未完全实现。如需复杂的构造函数参数，建议：
   - 使用 `solc` 编译时直接包含构造函数参数
   - 或使用其他工具（如 `abigen`）生成完整的 creation code

3. **并发控制**：默认并发数为 3，可根据网络情况调整 `--concurrency` 参数。

4. **账户余额**：确保部署账户在所有目标分片上都有足够的余额支付 gas 费用。

## 配置文件格式

工具合约地址会保存到 `joyue-tool-contracts.json` 文件，格式如下：

```json
{
  "shards": {
    "0": {
      "cacheAddr": "0x1234...",
      "rpcOracleAddr": "0x5678..."
    },
    "1": {
      "cacheAddr": "0xabcd...",
      "rpcOracleAddr": "0xef01..."
    }
  }
}
```

节点在部署 `JoyueAgent` 时会自动从这个文件读取工具合约地址。

## 完整部署流程示例

### 1. 部署 JoyueMockCache

```bash
# 1. 修改代码中的 embeddedContractBinHex 为 JoyueMockCache 的 bytecode
# 2. 部署并保存地址
go run cmd/joyue-deploy-all-shards/main.go \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --shards "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502" \
  --contract-type "cache" \
  --wait
```

### 2. 部署 JoyueRpcOracleMock

```bash
# 1. 修改代码中的 embeddedContractBinHex 为 JoyueRpcOracleMock 的 bytecode
# 2. 部署并保存地址
go run cmd/joyue-deploy-all-shards/main.go \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --shards "0=http://127.0.0.1:9500,1=http://127.0.0.1:9502" \
  --contract-type "rpc-oracle" \
  --wait
```

### 3. 部署 JoyueMaster（会自动触发 Agent 部署）

```bash
# 部署 Master 后，节点会自动从配置文件读取工具合约地址并部署 Agent
go run cmd/joyue-deploy-master/main.go \
  --private-key "3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
  --rpc "http://127.0.0.1:9500" \
  --wait
```

