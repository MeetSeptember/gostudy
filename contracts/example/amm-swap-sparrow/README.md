# amm-swap-sparrow（对照 2PC AMM / JOYUE）

协调者与 Pool、WalletA、WalletB **可不同分片**；跨分片通过 `TwoPhaseLib.emitCrossShardRequest`（与 `wallet-transfer-sparrow` 一致）。

## 流程（实验语义）

- 协调者 **`swapWave`**：对每行 **view** `pool.quoteSwap` 仅用于组装 Pool 的 `newReserveA/B[]` 与 Wallet 的 `amountOut`（**不**在协调者判版本）；任一行 `quoteSwap` 失败则本波立即结束（不发 prepare）。
- **并行 emit** 三类 prepare（不等待彼此回执）：
  - **Pool**（`poolShardId`）：`prepareReadWriteBatch(batchId, newReserveAs[], newReserveBs[], clientVersions[])`，顺序即隐式 index；Pool **信任** calldata，**不**再重算校验；首行版本匹配则 `batchId` 锁池、写 pending、**`ammVersion++`**，后续行仅 `PoolPrepareItem(..., false)`。
  - **WalletA**：`batchId + user` 一批，`prepareBatch`。
  - **WalletB**：`batchId + user` 一批，`prepareCreditBatch`。
- `pending = 1 + nD + nC`，各 prepare 回执进 **`onPrepareResponse`**；`pending==0` 时 finalize：**含 Pool 在内的参与方** 全成则 **commit**，否则 **abort**（含 Pool 的 `commitBatch`/`abortBatch`，亦走跨分片 emit）。
- **Wallet**：本波内按 **`keccak256(waveKey, kind, user)`** 的 `batchId` + **user** 聚合，**所有 item** 的 `amountIn` / `poolOut` 按顺序进入对应 user 的 `amounts[]`（与 `wallet-transfer-sparrow` 相同维度）。
- **Finalize**：每个 item 须 **`_poolLineOk[i]`** 且该 user 批内对应行的 debit/credit 均为 true；**任一 item 不满**则整波 **`anyIncludedFail` → abort**（与转账 Sparrow 的「每条 transfer 都须成」一致）。当前 Pool 仅首行写池，故 **i≥1 的 `_poolLineOk[i]` 恒 false**，多 item 同波通常会 finalize abort（除非后续改 Pool 多行语义）。

## 合约与部署

| 合约 | 说明 |
|------|------|
| `SparrowAmmPool` | `quoteSwap` + `prepareReadWriteBatch` + `commitBatch` / `abortBatch` |
| `SparrowAmmWalletA` / `SparrowAmmWalletB` | 批处理扣/加款 |
| `SparrowAmmCoordinator` | 构造：`pool, poolShardId, walletA, walletB, walletAShardId, walletBShardId` |
| `SparrowAmmIntentShop` | `swapIntentExplicit(user,amountIn,minOut,clientVersion)`；`clientVersion==0` 时 Pool 首行用入口 `ammVersion` 快照代填 |

部署顺序：先部署 **`SparrowAmmPool()`**、空构造的 **`SparrowAmmWalletA` / `SparrowAmmWalletB`**，再部署 **`SparrowAmmCoordinator(pool, poolShardId, walletA, walletB, ...)`**（构造传入 Pool 地址与分片；Pool 实验合约无鉴权）。压测前对 **`SparrowAmmWalletA`** 跑 **`bootstrap-wallet-balances-from-csv -kind amm-sparrow-wallet-a`**（一般不必对 B 再灌同一 CSV）。

## 编译

```bash
cd contracts && solc --base-path . example/amm-swap-sparrow/*.sol
```

## 依赖

Relayer + 0x6D/0x74：Pool 与 WalletA/B 均可跨分片。

## 链下 batcher（Intent → swapWave）

`cmd/sparrow-amm-batcher`：监听 **`SparrowAmmSwapIntent`**，按 `--batch-interval-ms` / `--max-batch` 聚批后调用 **`SparrowAmmCoordinator.swapWave`**，`intentId` 写入每条的 **`txId`**（供 `SparrowWaveStarted` / `sparrow-metrics` 关联）。

单分片示例：

```bash
go run ./cmd/sparrow-amm-batcher \
  --rpc http://127.0.0.1:9500 \
  --intent-shop 0x<IntentShop> \
  --coordinator 0x<Coordinator> \
  --private-key <hex>
```

多分片、Intent 与 Coordinator 不同分片时参数风格与 **`sparrow-transfer-batcher`** 相同（`--rpcs`、`--intent-shops`、`--coordinators`、`--coordinator-rpc`）。  
**通路径压测**建议 **`--max-batch 1`**（当前 Pool 仅首行成功，同波多 item 易 finalize abort）。

## 指标

- **`csv-trigger`**：`-scenario wallet-amm-sparrow`（别名 `amm-sparrow`），`-to=SparrowAmmIntentShop`，CSV 单列 user，`-amount` / `-min-amount-out` / `-client-version`（默认 `0`）；收据按 **`SparrowAmmSwapIntent`** 解析 `intentId`（`is2PC=false`）。
- **`joyue-trigger`**：`trigger-sparrow-amm-intent.yaml` 发 IntentShop（`coordinator` 留空），`--metrics-output` 从 receipt 解析 **`SparrowAmmSwapIntent`** 的 `intentId`；`swapWave` 由 batcher 发送后，sent 侧依赖协调者交易里的 **`SparrowWaveStarted(txIds[])`**（与水果 Sparrow 相同逻辑）。
- **`sparrow-metrics`**：`--coordinator` 指向 **`SparrowAmmCoordinator`**，订阅 **`SparrowWaveFinished`**；`txId==0` 仍跳过。
