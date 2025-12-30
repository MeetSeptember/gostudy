# JOYUE 部署检查工具

用于检查代理合约是否在其他分片部署成功。

## 编译

```bash
cd cmd/joyue-check-deploy
go build -o joyue-check-deploy
```

或者从项目根目录：

```bash
go build -o bin/joyue-check-deploy ./cmd/joyue-check-deploy
```

## 使用方法

### 方式1：通过部署私钥和 nonce 计算合约地址（推荐）

如果你知道部署私钥和 nonce，工具会自动计算合约地址：

```bash
./joyue-check-deploy \
  --rpc=http://127.0.0.1:9501 \
  --private-key=你的部署私钥hex字符串 \
  --nonce=0
```

**说明**：
- `--private-key`: 部署私钥（hex，不带 0x）
- `--nonce`: 部署交易的 nonce（通常是 0，如果是第一次部署）

### 方式2：通过部署者地址和 nonce

如果你知道部署者地址和 nonce：

```bash
./joyue-check-deploy \
  --rpc=http://127.0.0.1:9501 \
  --deployer=0x部署者地址 \
  --nonce=0
```

### 方式3：通过交易 hash 查询

如果你知道部署交易的 hash（从日志中获取）：

```bash
./joyue-check-deploy \
  --rpc=http://127.0.0.1:9501 \
  --tx=0x交易hash
```

**如何获取交易 hash**：
- 查看节点日志，搜索 `[JOYUE] agent deployment tx sent successfully`
- 日志中会显示 `txHash` 字段

### 方式4：直接查询指定地址

如果你已经知道合约地址：

```bash
./joyue-check-deploy \
  --rpc=http://127.0.0.1:9501 \
  --contract=0x合约地址
```

## 完整示例

### 示例1：检查 shard 1 的代理合约（使用私钥）

```bash
./joyue-check-deploy \
  --rpc=http://127.0.0.1:9501 \
  --private-key=ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
  --nonce=0
```

### 示例2：检查 shard 2 的代理合约（使用交易 hash）

```bash
# 从日志中获取交易 hash，例如：0x1234...
./joyue-check-deploy \
  --rpc=http://127.0.0.1:9502 \
  --tx=0x1234567890abcdef...
```

### 示例3：批量检查所有分片

```bash
# 检查 shard 1
./joyue-check-deploy --rpc=http://127.0.0.1:9501 --private-key=你的私钥 --nonce=0

# 检查 shard 2
./joyue-check-deploy --rpc=http://127.0.0.1:9502 --private-key=你的私钥 --nonce=0

# 检查 shard 3
./joyue-check-deploy --rpc=http://127.0.0.1:9503 --private-key=你的私钥 --nonce=0
```

## 输出说明

工具会输出以下信息：

- ✅ **合约已部署**：合约地址有代码，部署成功
- ❌ **合约未部署**：合约地址没有代码，可能尚未部署或部署失败
- **代码长度**：合约代码的字节数
- **初始化状态**：合约是否已调用 `initialize()` 函数
- **Master 地址**：代理合约关联的主合约地址
- **Agent Shard ID**：代理合约所在的分片 ID

## 验证所有分片的合约地址是否相同

由于合约地址是通过 `CreateAddress(部署者地址, nonce)` 计算的，如果部署者地址在每个分片上都是第一次使用（nonce=0），那么所有分片上的代理合约地址应该是**相同的**。

**验证方法**：

```bash
# 检查 shard 1
./joyue-check-deploy --rpc=http://127.0.0.1:9501 --private-key=你的私钥 --nonce=0

# 检查 shard 2
./joyue-check-deploy --rpc=http://127.0.0.1:9502 --private-key=你的私钥 --nonce=0

# 检查 shard 3
./joyue-check-deploy --rpc=http://127.0.0.1:9503 --private-key=你的私钥 --nonce=0
```

如果所有分片的合约地址都相同，说明部署成功且地址一致。

**注意**：
- 如果部署者地址在某个分片上已经发送过其他交易，nonce 会不同，合约地址也会不同
- 如果某个分片的 nonce 不是 0，需要相应调整 `--nonce` 参数

## 使用 hmy CLI（如果已安装）

如果你已经安装了 Harmony 官方的 `hmy` CLI 工具，也可以使用以下方式查询：

### 1. 查询合约代码

```bash
# 使用 hmy 查询合约代码
hmy blockchain get-code --node=http://127.0.0.1:9501 --address=0x合约地址
```

### 2. 查询交易 receipt

```bash
# 使用 hmy 查询交易 receipt
hmy blockchain get-transaction-receipt --node=http://127.0.0.1:9501 --tx-hash=0x交易hash
```

### 3. 使用 RPC 调用

```bash
# 直接使用 curl 调用 RPC
curl -X POST http://127.0.0.1:9501 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "eth_getCode",
    "params": ["0x合约地址", "latest"],
    "id": 1
  }'
```

## 计算合约地址

如果你知道部署者地址和 nonce，可以使用以下公式计算合约地址：

**公式**：`keccak256(rlp_encode([deployer_address, nonce]))[12:]`

**使用 Go 代码**：
```go
import (
    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/crypto"
)

deployerAddr := common.HexToAddress("0x...")
nonce := uint64(0)
contractAddr := crypto.CreateAddress(deployerAddr, nonce)
```

## 故障排查

### 问题1：合约地址计算错误

**原因**：nonce 不正确

**解决**：
- 查询部署者地址的当前 nonce：`hmy blockchain get-nonce --node=http://127.0.0.1:9501 --address=0x部署者地址`
- 如果已经发送过其他交易，nonce 可能不是 0
- 查看节点日志中的 `nonce` 字段

### 问题2：查询不到代码

**可能原因**：
- 合约尚未部署（交易还在 pending）
- 部署交易失败
- 使用了错误的分片 RPC

**解决**：
- 检查节点日志中的部署交易状态
- 确认 RPC URL 正确
- 等待几个区块确认

### 问题3：RPC 连接失败

**解决**：
- 确认目标分片的 RPC 服务正在运行
- 检查 RPC URL 格式（包含 `http://` 前缀）
- 确认端口号正确
