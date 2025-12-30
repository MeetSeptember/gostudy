# joyue-query-events：通用事件查询工具

用于查询任意合约发出的任意事件。

## 功能特性

- ✅ 支持通过事件签名查询
- ✅ 支持通过 ABI 文件查询（更详细的解析）
- ✅ 支持查询合约的所有事件
- ✅ 支持按区块范围过滤
- ✅ 支持文本和 JSON 格式输出
- ✅ 自动解析 indexed 和 non-indexed 参数

## 使用方法

### 1. 通过事件签名查询

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --event "MasterDeployed(address,bytes32,bytes,uint32)" \
  --from-block 0
```

### 2. 通过 ABI 文件查询（推荐）

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --abi-file contracts/joyue/JoyueMaster.abi \
  --event-name "MasterDeployed" \
  --from-block 0
```

### 3. 查询合约的所有事件

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --from-block 0
```

### 4. 指定区块范围

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --event "MasterDeployed(address,bytes32,bytes,uint32)" \
  --from-block 100 \
  --to-block 200
```

### 5. JSON 格式输出

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --event "MasterDeployed(address,bytes32,bytes,uint32)" \
  --from-block 0 \
  --json
```

### 6. 显示详细信息

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --event "MasterDeployed(address,bytes32,bytes,uint32)" \
  --from-block 0 \
  --verbose
```

## 参数说明

| 参数 | 说明 | 必需 |
|------|------|------|
| `--rpc` | RPC URL | 否（默认：http://127.0.0.1:9500） |
| `--contract` | 合约地址（0x...） | **是** |
| `--event` | 事件签名（例如：`MasterDeployed(address,bytes32,bytes,uint32)`） | 否 |
| `--abi-file` | ABI 文件路径（JSON 格式） | 否 |
| `--event-name` | 事件名称（需要配合 `--abi-file` 使用） | 否 |
| `--from-block` | 起始区块号（包含） | 否（默认：0） |
| `--to-block` | 结束区块号（包含，0 表示最新） | 否（默认：0，表示最新） |
| `--json` | 以 JSON 格式输出 | 否 |
| `--verbose` | 显示详细信息 | 否 |

## 查询方式说明

### 方式 1：事件签名（简单快速）

使用 `--event` 参数指定事件签名：
```bash
--event "MasterDeployed(address,bytes32,bytes,uint32)"
```

**优点**：
- 不需要 ABI 文件
- 快速查询

**缺点**：
- 只能显示原始数据（topics 和 data）
- 无法自动解析参数名称

### 方式 2：ABI 文件（推荐）

使用 `--abi-file` 和 `--event-name` 参数：
```bash
--abi-file contracts/joyue/JoyueMaster.abi \
--event-name "MasterDeployed"
```

**优点**：
- 自动解析参数名称和类型
- 区分 indexed 和 non-indexed 参数
- 格式化输出更易读

**缺点**：
- 需要提供 ABI 文件

### 方式 3：查询所有事件

不指定 `--event` 或 `--abi-file`，查询合约的所有事件。

## 输出示例

### 文本格式（使用 ABI 文件）

```
找到 1 个事件
合约地址: 0x12a4113F44E5689C93df233008FD805BDc882ABb
事件签名: MasterDeployed(address,bytes32,bytes,uint32)
区块范围: 0 - 100

=== 事件 #1 ===
区块号: 24
交易哈希: 0xb64776144469470e44d3296963b76e0486432df6373130c2bc9754f2a9a177fa
日志索引: 0
合约地址: 0x12a4113F44E5689C93df233008FD805BDc882ABb
事件名称: MasterDeployed

--- 事件参数 ---
Indexed 参数:
  master (address): 0x12a4113F44E5689C93df233008FD805BDc882ABb
  salt (bytes32): 0x0000000000000000000000000000000000000000000000012223691111122211
Non-indexed 参数:
  agentCreationCode (bytes): 0x6080604052348015600f57600080fd5b... (2334 bytes)
  masterShardId (uint32): 0
```

### JSON 格式

```json
{
  "index": 1,
  "blockNumber": 24,
  "txHash": "0xb64776144469470e44d3296963b76e0486432df6373130c2bc9754f2a9a177fa",
  "logIndex": 0,
  "address": "0x12a4113F44E5689C93df233008FD805BDc882ABb",
  "topics": [
    "0xefdeb09d7e10be4d0f0a1008af53d985776824c3026f0505ac302986d23765e6",
    "0x00000000000000000000000012a4113f44e5689c93df233008fd805bdc882abb",
    "0x0000000000000000000000000000000000000000000000012223691111122211"
  ],
  "data": "0x...",
  "eventName": "MasterDeployed",
  "parameters": {
    "indexed": {
      "master": "0x12a4113F44E5689C93df233008FD805BDc882ABb",
      "salt": "0x0000000000000000000000000000000000000000000000012223691111122211"
    },
    "nonIndexed": {
      "agentCreationCode": "0x6080604052348015600f57600080fd5b... (2334 bytes)",
      "masterShardId": "0"
    }
  }
}
```

## 常见事件查询示例

### 查询 JoyueMaster 的 MasterDeployed 事件

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --event "MasterDeployed(address,bytes32,bytes,uint32)" \
  --from-block 0
```

### 查询 JoyueAgent 的 AgentResult 事件

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9502 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --event "AgentResult(address,bytes32,address,bytes)" \
  --from-block 0
```

### 查询 JoyueMaster 的 ResultAccepted 事件

```bash
go run cmd/joyue-query-events/main.go \
  --rpc http://127.0.0.1:9500 \
  --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
  --event "ResultAccepted(bytes32,address,uint32)" \
  --from-block 0
```

## 注意事项

1. **事件签名格式**：必须与合约中定义的事件完全一致，包括参数类型
2. **区块范围**：`--to-block 0` 表示查询到最新区块
3. **ABI 文件格式**：必须是标准的 Solidity ABI JSON 格式
4. **性能**：查询大量区块可能较慢，建议指定合理的区块范围

## 与现有工具的区别

- `joyue-query-master-event`：专门查询 `MasterDeployed` 事件
- `joyue-query-events`：通用工具，可以查询任意合约的任意事件

