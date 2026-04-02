# Sparrow 对照实验（fruitstore + 2PC）

与 `example/twophase` 的原生 2PC、`fruitstore-joyue` 的 JOYUE 并列：同一业务语义（买水果、扣余额、加积分、扣库存），跨分片机制与 2PC 相同（`TwoPhaseLib` 0x6D / 0x74）。

## 推荐流程：用户单笔意图 + Relayer 批处理

更贴近「多人各发一笔、链下聚批」的模型：

1. 用户只调 **`SparrowIntentShop`**（如 `buyFruitIntentByName("apple",1)`），合约 **只发事件** `SparrowBuyIntent`，不触发 2PC。事件里的 **`buyer`** 与 **`msg.sender` 无关**：按合约内 **自增序号对 10 取模**，在 **`SparrowWallet` 相同的 10 个 Hardhat 演示地址** 上轮询，便于单私钥 trigger 稳定轮换多用户（需重新部署 Intent 合约后生效）。
2. **`cmd/sparrow-batcher`**（模拟 Relayer）监听该事件，在内存里攒一批后，代发一笔 **`SparrowCoordinator.buyFruitWave(items[])`**；协调者内部再按买家合并 Wallet/Points、按水果合并 Stock，并并行 prepare / 统一 commit 或 abort。

直连 `buyFruitWave1/2` 仍可用于单测或不走 Relayer 的对照实验。

## 与原生 2PC 的差异

| 项目 | 原生 2PC | Sparrow（本目录） |
|------|-----------|-------------------|
| 调用单位 | 每笔主交易 3 路独立 `prepare(txId,…)` | 一波内按读写集合并：`prepareBatch` 带 **`uint256[]`**（每笔一行），协调者**不**先加成标量；各 participant **先占锁**再逐行判成败 |
| prepare 发出方式 | 三路 prepare 并行 | Wallet / Points / Stock **同时发出**（同笔协调 tx 内全发），收齐回调后解码各批 `lineOk[]` |
| 锁粒度 | 合约级单 `txId` | Wallet/Points **按买家**，Store **按 fruitType**；**commit / abort 均释放锁**（含仅有锁、无 prepare 记账时） |
| 协调者 API | `buyFruit` / `buyBook` | `buyFruitWave`（动态数组）、`buyFruitWave1`、`buyFruitWave2`；生产流由 **Intent + batcher** 调 `buyFruitWave` |

**`BuyItem`** 含 **`txId`**：`SparrowIntentShop` 发出的 **`intentId`** 经 batcher 写入；**`buyFruitWave1/2`** 使用 **`txId = 0`（方案 A，不参与指标）**。

- **`SparrowWaveStarted(waveId, mainTxCount, txIds[])`**：`txIds` 与 `items` 顺序一致。
- **`SparrowWaveFinished(txId, committed)`**：每笔主交易一条，`txId` 同 `BuyItem.txId`。

收齐 prepare 后按 item 判定三端行级 success 并 `emit`；若任一路 Executor 失败则整波 `abort` 且各条 `Finished(..., false)`；**仅当全部 item 的 `committed` 为 true** 时 `commitBatch`。

## 合约

| 合约 | 建议分片 | 说明 |
|------|-----------|------|
| SparrowFruitStore | 0（每库存分片一套） | 库存、单价、积分倍率；**构造参数** `(appleStock, bananaStock)`，单分片常用 `(50,8)`，两分片各一半可用 `(25,4)` |
| SparrowWallet | 1 | 批量扣款；构造函数写入 **Hardhat 默认 account 0–9** 的伪随机初始余额（可与本地默认私钥对照） |
| SparrowPoints | 1 | 同上 10 个地址的伪随机初始积分；prepare/commit/abort 语义见合约注释 |
| SparrowCoordinator | 0（每「库存+意图」分片一套） | 组批 + 并行 prepare、统一 2PC 收尾；构造时 `fruitStoreShardId` 与本分片一致，`wallet`/`points` 填 shard1 地址，`walletShardId`/`pointsShardId` 为 `1` |
| SparrowIntentShop | 与 Coordinator 同分片 | 用户侧单笔意图，仅事件；`buyer` 为构造内置 10 账户之一（自增 id 对 10 取模轮询），与 `msg.sender` 解耦 |

## 部署顺序

### 单分片（仅 shard0 有 Store / Coordinator / Intent）

1. Shard 0：部署 `SparrowFruitStore(50, 8)`（或按需库存）
2. Shard 1：部署 `SparrowWallet`、`SparrowPoints`（按需 `setBalance` / `setPoints`）
3. Shard 0：部署 `SparrowCoordinator(fruitStore, wallet, points, 0, 1, 1)`
4. Shard 0：部署 `SparrowIntentShop`（与 Coordinator 同分片）

### 双分片（shard0 / shard1 各一套 Store + Coordinator + Intent；Wallet/Points 仍只在 shard1）

1. Shard 0：`SparrowFruitStore(25, 4)` → `SparrowCoordinator(store0, wallet, points, 0, 1, 1)` → `SparrowIntentShop`
2. Shard 1：`SparrowFruitStore(25, 4)` → `SparrowCoordinator(store1, wallet, points, 1, 1, 1)` → `SparrowIntentShop`
3. Shard 1：部署 **一份** `SparrowWallet`、`SparrowPoints`，地址写入两个 Coordinator

## Relayer 批处理（实验用）

单分片：

```bash
go run ./cmd/sparrow-batcher \
  --rpc http://127.0.0.1:9500 \
  --intent-shop <INTENT_SHOP> \
  --coordinator <COORDINATOR> \
  --private-key <HEX> \
  --batch-interval-ms 500 \
  --max-batch 32
```

多分片（每分片独立 buffer；格式与 `joyue-metrics --rpcs` 一致）：

```bash
go run ./cmd/sparrow-batcher \
  --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501 \
  --intent-shops 0=<SHOP0>,1=<SHOP1> \
  --coordinators 0=<COORD0>,1=<COORD1> \
  --private-key <HEX> \
  --batch-interval-ms 500 \
  --max-batch 32
```

**Intent 与 Coordinator 不同分片**（例如仅 shard0 部署 Coordinator / FruitStore，shard1 仍有 Intent）：加 **`--coordinator-rpc`** 指向 Coordinator 所在分片 RPC；监听仍用 `--rpcs` 各分片。`--coordinators` 里两路可填 **同一** Coordinator 地址。多路 worker 会向同一提交链发交易，程序内对 **nonce + 发送** 做了互斥。

```bash
go run ./cmd/sparrow-batcher \
  --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501 \
  --intent-shops 0=<SHOP0>,1=<SHOP1> \
  --coordinators 0=<COORD_ON_S0>,1=<COORD_ON_S0> \
  --coordinator-rpc http://127.0.0.1:9500 \
  --private-key <HEX> \
  --batch-interval-ms 500 \
  --max-batch 32
```

单分片若 Intent 在 shard1、Coordinator 在 shard0：`--rpc http://127.0.0.1:9501 ... --coordinator-rpc http://127.0.0.1:9500`。

## 用 joyue-call 发请求（测试）

在项目根目录执行。`--sig` 格式：`函数名(参数类型…)(返回类型…)`；无返回值写 `()`。

**读：Shard 1 上查某地址余额（`SparrowWallet`）**

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9501 \
  --to <WALLET_ADDR> \
  --sig "balance(address)(uint256)" \
  --arg 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266
```

**写：Shard 0 上发购买意图（`SparrowIntentShop`，仅事件；需配合 `sparrow-batcher` 才会进 2PC）**

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to <INTENT_SHOP_ADDR> \
  --sig "buyFruitIntentByName(string,uint256)()" \
  --arg apple --arg 1 \
  --send --private-key <HEX> --gas 200000 --wait
```

**写：跳过 batcher，直连协调者单笔波（`SparrowCoordinator`；需跨分片 Relayer 已跑）**

```bash
go run ./cmd/joyue-call \
  --rpc http://127.0.0.1:9500 \
  --to <COORDINATOR_ADDR> \
  --sig "buyFruitWave1(address,string,uint256)()" \
  --arg 0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266 \
  --arg apple --arg 1 \
  --send --private-key <HEX> --gas 1200000 --wait
```

`buyer` 须与 `--private-key` 对应地址一致（否则扣的是别人余额）。Hardhat 默认第 0 个账户地址见上例；`apple` / `banana` 为水果名（与合约里 `keccak256` 一致）。

## 指标（tx_id 串联）

1. **Intent**：`SparrowBuyIntent` 的 **`intentId`** 即 metrics 用的 **`tx_id`**（与 `BuyItem.txId` 一致）。
2. **`joyue-trigger`**：`--metrics-output` 时，仅对 **Intent 交易 receipt** 解析 **`SparrowBuyIntent`** 取 **`intentId` → `tx_id`**。**不调 Coordinator 写 sent**：`buyFruitWave` 由 **batcher** 代发；手动 `buyFruitWave1/2` 为测试（`txId=0`）不写 sent。
3. **`sparrow-metrics`**：轮询 **`SparrowWaveFinished(txId, committed)`**，**跳过 `txId==0`**。

单分片：

```bash
go run ./cmd/sparrow-metrics \
  --rpc http://127.0.0.1:9500 \
  --coordinator <COORDINATOR> \
  --output ./sparrow-metrics.csv --from-block 0
```

多分片（**单个 CSV**，列与 `joyue-metrics` 相同；`shard_id` 区分分片）：

```bash
go run ./cmd/sparrow-metrics \
  --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501 \
  --coordinators 0=<COORD0>,1=<COORD1> \
  --output ./sparrow-metrics.csv --from-block 0
```

离线将 sent JSONL 与 `sparrow-metrics` 按 **`tx_id` + `shard_id`** 合并（与 `joyue-metrics` 列对齐）。

## Trigger

- **直连协调者**（手动测试）：`cmd/joyue-trigger/trigger-sparrow-coordinator.yaml`；`buyFruitWave1/2` 的 **`txId=0`** 不进指标。
- **意图流（单分片）**：`cmd/joyue-trigger/trigger-sparrow-intent.yaml` — **`agent` = IntentShop 地址，`coordinator` 留空**（勿把 Intent 填进 `coordinator`，否则 `is2PC=true` 无法从 receipt 解析 `SparrowBuyIntent`）。加 `--metrics-output` 写 sent（含 **`shard_id`**），并**同时运行** `sparrow-batcher`。
- **意图流（多分片）**：`cmd/joyue-trigger/trigger-sparrow-intent-multishard.yaml` — `shards` 下为各分片配置 `rpc` + `agent`（IntentShop）；`coordinator` 留空。与多分片 `sparrow-batcher` / `sparrow-metrics` 配合使用。
