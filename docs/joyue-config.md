# JOYUE 节点自动部署配置说明

## 配置方式

有三种方式可以设置 JOYUE 自动部署的配置：

### 方式1：使用 deploy.sh 脚本启动 localnet（推荐）

如果你使用 `test/deploy.sh` 脚本启动 localnet，可以在配置文件名称后面添加额外的参数：

```bash
./test/deploy.sh local_config.txt \
  --joyue.auto-deploy=true \
  --joyue.deploy-private-key=你的私钥hex字符串 \
  --joyue.other-shard-rpcs=1=http://127.0.0.1:9501,2=http://127.0.0.1:9502
```

**说明**：
- `deploy.sh` 脚本会将配置文件名称后面的所有参数作为 `extra_args` 传递给每个节点
- 这些参数会被应用到所有启动的节点
- 如果你只想对特定节点应用，可以使用配置文件方式（方式3）

**完整示例**：
```bash
# 启动 localnet，并启用 JOYUE 自动部署
./test/deploy.sh local_config.txt \
  --joyue.auto-deploy=true \
  --joyue.deploy-private-key=ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
  --joyue.other-shard-rpcs=1=http://127.0.0.1:9501,2=http://127.0.0.1:9502
```

### 方式2：直接启动节点（命令行参数）

直接启动单个节点时，通过命令行参数设置：

```bash
./harmony \
  --joyue.auto-deploy=true \
  --joyue.deploy-private-key=你的私钥hex字符串 \
  --joyue.other-shard-rpcs=1=http://127.0.0.1:9501,2=http://127.0.0.1:9502
```

**参数说明**：
- `--joyue.auto-deploy`: 布尔值，是否启用自动部署（`true` 或 `false`）
- `--joyue.deploy-private-key`: 字符串，部署私钥的 hex 格式（**不带 0x 前缀**）
- `--joyue.other-shard-rpcs`: 字符串，其他分片的 RPC 地址，格式为 `shardID=rpcURL,shardID=rpcURL`

**示例**：
```bash
# 启动节点并启用 JOYUE 自动部署
./harmony \
  --run=validator \
  --network=testnet \
  --joyue.auto-deploy=true \
  --joyue.deploy-private-key=ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
  --joyue.other-shard-rpcs=1=http://127.0.0.1:9501,2=http://127.0.0.1:9502,3=http://127.0.0.1:9503
```

### 方式3：配置文件（推荐用于生产环境或特定节点）

如果你使用 `deploy.sh` 脚本，并且想为特定节点设置不同的配置，可以在配置文件中指定节点配置文件：

**步骤1**：创建节点配置文件（例如 `node0.conf`）：
```toml
version = "1.0.0"

[General]
node_type = "validator"
shard_id = 0

[Network]
network_type = "localnet"

[P2P]
ip = "127.0.0.1"
port = 9000

[HTTP]
enabled = true
ip = "0.0.0.0"
port = 9500

# ... 其他配置 ...

[Joyue]
auto_deploy_enabled = true
deploy_private_key = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
other_shard_rpcs = "1=http://127.0.0.1:9501,2=http://127.0.0.1:9502"
```

**步骤2**：在 `local_config.txt` 中指定配置文件路径：
```
# local_config.txt 格式：ip port mode bls_key shard node_config
127.0.0.1 9000 validator .hmy/blskeys/key1 0 node0.conf
127.0.0.1 9001 validator .hmy/blskeys/key2 1 node1.conf
127.0.0.1 9002 validator .hmy/blskeys/key3 2 node2.conf
```

**步骤3**：启动 localnet：
```bash
./test/deploy.sh local_config.txt
```

**注意**：`deploy.sh` 脚本会检查配置文件是否存在（第 115-118 行），如果存在，会使用 `--config` 参数加载该配置文件。

### 方式4：独立节点配置文件（生产环境）

在 Harmony 配置文件中添加 `[Joyue]` 部分：

**配置文件路径**：通过 `--config` 参数指定，例如 `--config=./harmony.conf`

**配置文件格式**（TOML）：

```toml
# ... 其他配置 ...

[Joyue]
# 是否启用自动部署
auto_deploy_enabled = true

# 部署私钥（hex，不带 0x）
deploy_private_key = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

# 其他分片的 RPC 地址
# 格式：shardID=rpcURL,shardID=rpcURL
other_shard_rpcs = "1=http://127.0.0.1:9501,2=http://127.0.0.1:9502,3=http://127.0.0.1:9503"
```

**使用配置文件启动**：
```bash
./harmony --config=./harmony.conf
```

## 配置项详细说明

### 1. `auto_deploy_enabled` / `--joyue.auto-deploy`

- **类型**：布尔值
- **默认值**：`false`
- **说明**：是否启用 JOYUE 自动部署功能。设置为 `true` 后，节点会在检测到主合约部署时自动向其他分片发送代理合约部署交易。

### 2. `deploy_private_key` / `--joyue.deploy-private-key`

- **类型**：字符串
- **默认值**：空字符串
- **说明**：用于发送部署交易的私钥（hex 格式，**不带 0x 前缀**）
- **示例**：
  - ✅ 正确：`ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80`
  - ❌ 错误：`0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80`（带 0x）

**生成私钥**（如果需要）：
```bash
# 使用 openssl 生成
openssl rand -hex 32

# 或使用 Node.js
node -e "console.log(require('crypto').randomBytes(32).toString('hex'))"
```

**重要**：确保该私钥对应的地址有足够的余额支付 gas 费用。

### 3. `other_shard_rpcs` / `--joyue.other-shard-rpcs`

- **类型**：字符串
- **默认值**：空字符串
- **说明**：其他分片的 RPC 地址列表，格式为 `shardID=rpcURL,shardID=rpcURL`
- **格式要求**：
  - 每个分片配置为 `shardID=rpcURL`
  - 多个分片用逗号 `,` 分隔
  - 不需要包含当前分片（master shard）的 RPC
  - RPC URL 应该是完整的 HTTP 地址

**示例**：
```bash
# 3 个分片的配置
--joyue.other-shard-rpcs=1=http://127.0.0.1:9501,2=http://127.0.0.1:9502,3=http://127.0.0.1:9503

# 如果只有 2 个分片
--joyue.other-shard-rpcs=1=http://127.0.0.1:9501

# 使用不同的 IP 和端口
--joyue.other-shard-rpcs=1=http://192.168.1.100:8545,2=http://192.168.1.101:8545
```

## 完整配置示例

### 示例1：使用 deploy.sh 脚本启动 localnet（推荐）

```bash
# 方式1：通过 extra_args 传递参数（应用到所有节点）
./test/deploy.sh local_config.txt \
  --joyue.auto-deploy=true \
  --joyue.deploy-private-key=ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
  --joyue.other-shard-rpcs=1=http://127.0.0.1:9501,2=http://127.0.0.1:9502
```

```bash
# 方式2：为每个节点创建独立的配置文件
# 1. 创建 node0.conf（shard 0 的节点）
# 2. 在 local_config.txt 中指定：127.0.0.1 9000 validator .hmy/blskeys/key1 0 node0.conf
# 3. 启动
./test/deploy.sh local_config.txt
```

### 示例2：直接启动单个节点（命令行方式）

```bash
./harmony \
  --run=validator \
  --network=localnet \
  --run.shard=0 \
  --http.ip=0.0.0.0 \
  --http.port=9500 \
  --joyue.auto-deploy=true \
  --joyue.deploy-private-key=ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80 \
  --joyue.other-shard-rpcs=1=http://127.0.0.1:9501,2=http://127.0.0.1:9502
```

### 示例3：配置文件方式（生产环境）

**harmony.conf**：
```toml
version = "1.0.0"

[General]
node_type = "validator"
shard_id = 0
data_dir = "./harmony_db_0"

[Network]
network_type = "testnet"

[P2P]
ip = "0.0.0.0"
port = 9000

[HTTP]
enabled = true
ip = "0.0.0.0"
port = 9500

# ... 其他配置 ...

[Joyue]
auto_deploy_enabled = true
deploy_private_key = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
other_shard_rpcs = "1=http://127.0.0.1:9501,2=http://127.0.0.1:9502,3=http://127.0.0.1:9503"
```

启动：
```bash
./harmony --config=./harmony.conf
```

## 验证配置

启动节点后，检查日志中是否有以下信息：

1. **配置加载成功**：
   ```
   [JOYUE] parsed shard RPCs shardCount=2
   [JOYUE] deploy private key parsed deployAddr=0x...
   ```

2. **检测到主合约部署**：
   ```
   [JOYUE] detected JoyueMaster deployment, triggering agent deployment
   ```

3. **代理合约部署成功**：
   ```
   [JOYUE] agent deployment tx sent successfully shard=1 txHash=0x...
   ```

## 检查代理合约部署状态

### 方式1：使用 joyue-check-deploy 工具（推荐）

编译工具：
```bash
go build -o bin/joyue-check-deploy ./cmd/joyue-check-deploy
```

**通过部署私钥和 nonce 查询**（最简单）：
```bash
# 检查 shard 1
./bin/joyue-check-deploy \
  --rpc=http://127.0.0.1:9501 \
  --private-key=你的部署私钥hex字符串 \
  --nonce=0

# 检查 shard 2
./bin/joyue-check-deploy \
  --rpc=http://127.0.0.1:9502 \
  --private-key=你的部署私钥hex字符串 \
  --nonce=0
```

**通过交易 hash 查询**（从日志中获取 txHash）：
```bash
./bin/joyue-check-deploy \
  --rpc=http://127.0.0.1:9501 \
  --tx=0x从日志中获取的交易hash
```

工具会显示：
- ✅ 合约已部署：代码长度、初始化状态、Master 地址、Shard ID
- ❌ 合约未部署：说明可能的原因

### 方式2：使用 hmy CLI（如果已安装）

**查询合约代码**：
```bash
# 先计算合约地址（需要知道部署者地址和 nonce）
# 合约地址 = CreateAddress(部署者地址, nonce)

# 查询代码
hmy blockchain get-code \
  --node=http://127.0.0.1:9501 \
  --address=0x计算出的合约地址
```

**查询交易 receipt**：
```bash
# 从日志中获取交易 hash
hmy blockchain get-transaction-receipt \
  --node=http://127.0.0.1:9501 \
  --tx-hash=0x从日志中获取的交易hash
```

### 方式3：使用 RPC 直接查询

**查询合约代码**：
```bash
curl -X POST http://127.0.0.1:9501 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "eth_getCode",
    "params": ["0x合约地址", "latest"],
    "id": 1
  }'
```

如果返回的 `result` 不是 `"0x"`，说明合约已部署。

**查询交易 receipt**：
```bash
curl -X POST http://127.0.0.1:9501 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "eth_getTransactionReceipt",
    "params": ["0x交易hash"],
    "id": 1
  }'
```

从返回的 `result.contractAddress` 字段可以获取合约地址。

### 方式4：使用 joyue-call 工具调用合约函数

如果合约已部署，可以调用 `initialized()` 函数检查：

```bash
./bin/joyue-call \
  --rpc=http://127.0.0.1:9501 \
  --to=0x合约地址 \
  --sig="initialized()(bool)"
```

如果返回 `true`，说明合约已初始化。

### 计算合约地址

合约地址可以通过以下公式计算：
```
合约地址 = keccak256(rlp_encode([部署者地址, nonce]))[12:]
```

**使用 Go 代码**：
```go
import (
    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/crypto"
)

deployerAddr := common.HexToAddress("0x...")
nonce := uint64(0)  // 通常是 0，如果是第一次部署
contractAddr := crypto.CreateAddress(deployerAddr, nonce)
```

**注意**：如果部署者地址发送过其他交易，nonce 可能不是 0。可以通过 RPC 查询当前 nonce：
```bash
curl -X POST http://127.0.0.1:9501 \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "method": "eth_getTransactionCount",
    "params": ["0x部署者地址", "latest"],
    "id": 1
  }'
```

## 注意事项

1. **私钥安全**：
   - ⚠️ **不要**将私钥提交到版本控制系统
   - ⚠️ **不要**在公共场合分享私钥
   - ✅ 使用环境变量或安全的密钥管理工具
   - ✅ 确保配置文件权限设置正确（`chmod 600 harmony.conf`）

2. **网络连接**：
   - 确保节点能够访问配置中指定的其他分片 RPC 地址
   - 检查防火墙设置，确保 RPC 端口可访问

3. **Gas 费用**：
   - 确保部署私钥对应的地址有足够的余额
   - 每个分片的部署交易都需要支付 gas 费用

4. **分片配置**：
   - 不需要在配置中包含 master shard（主合约所在分片）的 RPC
   - 节点会自动跳过 master shard，不会重复部署

5. **错误处理**：
   - 如果某个分片部署失败，会在日志中记录错误信息
   - 不会影响其他分片的部署
   - 可以手动重试失败的部署

## 故障排查

### 问题1：配置未生效

**检查**：
- 确认命令行参数拼写正确（`--joyue.auto-deploy`，注意是 `joyue` 不是 `joyue`）
- 确认配置文件格式正确（TOML 格式）
- 查看启动日志，确认配置是否被加载

### 问题2：无法连接到其他分片 RPC

**检查**：
- 确认 RPC URL 格式正确（包含 `http://` 前缀）
- 确认目标分片的 RPC 服务正在运行
- 测试网络连接：`curl http://127.0.0.1:9501`（应该返回 JSON-RPC 响应）

### 问题3：交易发送失败

**检查**：
- 确认部署私钥对应的地址有足够余额
- 查看日志中的错误信息
- 确认目标分片的 chainID 正确
- 检查 gas price 和 gas limit 设置是否合理

### 问题4：未检测到主合约部署

**检查**：
- 确认主合约确实部署成功
- 确认主合约的 `MasterDeployed` 事件正确触发
- 查看日志中是否有相关错误信息

## 相关文档

- [节点自动部署方案](./joyue-node-auto-deploy.md)
- [JOYUE 系统架构](../contracts/joyue/README.md)

