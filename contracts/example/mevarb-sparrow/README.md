# mevarb-sparrow（Sparrow MEV，对齐 amm-swap-sparrow）

独立目录，与 **`mevarb-2pc` / `mevarb-joyue` 无关**。协调逻辑对齐 **`SparrowAmmCoordinator`**：顺序报价组批 → 四参与者 **各一批** 并行 `prepare`（Profit 的 `prepareLockBatch` **一次**带上各条净利润 `amounts`）→ `pending` 收齐 → 行级聚合 → 四端 `commitBatch`。

**不再依赖 `PeerSimulatedUsers`**：`ArbItem.user` 与 Intent 中的 **user** 须为任意非零地址；**`mevIntentExplicit(user, borrowA, minNetProfitA)`** 由链下/CSV 指定用户；**`mevIntentRandom`** 使用 **`msg.sender`** 作为 user（名称保留，语义为「调用方自用意图」）；**`arbWave1Random`** 使用 **`msg.sender`**。

## 事件（与 AMM Sparrow 一致）

- **`SparrowWaveStarted(bytes32 waveId, uint256 mainTxCount, bytes32[] txIds)`** — `sparrow-metrics` / `joyue-trigger`（`is2PC` 且 receipt 含此日志时）解析 `tx_id`。
- **`SparrowWaveFinished(bytes32 txId, bool committed)`** — **`sparrow-metrics`** 直接订阅，与 `SparrowAmmCoordinator` 相同签名，将 `--coordinator` 指向 `SparrowMevArbCoordinator` 即可。

## 合约

| 合约 | 说明 |
|------|------|
| `MevArbFlashLenderSparrow` | `prepareLendBatch` / `commitBatch` / `abortBatch`；批内 **用户互异**；`quoteLend` 要求 **user != address(0)** |
| `MevArbPoolLowSparrow` | `prepareReadWriteBatch`（与 `SparrowAmmPool` 同形）/ `commitBatch` / `abortBatch` |
| `MevArbPoolHighSparrow` | 同上（B→A） |
| `MevArbProfitWalletSparrow` | `prepareLockBatch` / `commitBatch` / `abortBatch`；**`setBalance` / `setBalances`**（与 erc20 bootstrap 同 ABI）；**`setProfit`** 为 `setBalance` 别名 |
| `SparrowMevArbCoordinator` | `arbWave(items)`、`arbWave1`、`arbWave1Random`（后者 user=`msg.sender`） |
| `SparrowMevArbIntentShop` | **`mevIntentExplicit(user, borrowA, minNetProfitA)`** / **`mevIntentRandom`**（user=`msg.sender`）→ **`SparrowMevArbIntent`** |

## 部署假设

协调者与 Flash、两池、Profit **同分片**，以便 `_quoteMev` 读 `reserve*`、`quoteLend`。跨分片仍通过 `shardFlash` / `shardLow` / `shardHigh` / `shardProfit` 发 Executor（测试可全填同一分片）。

## 批、版本与多笔（对齐 `SparrowAmmPool`）

- **两池**有 **`poolVersion`**：`quote*` 只读，**不**改储备与版本；**`prepareReadWriteBatch`** 仅校验 **首行** `clientVersion`（`0` 表示入口快照，与 AMM 一致），通过后 **锁池、`poolVersion++`**，并只采纳 **首行** 的 `newReserve*` 提案写入 pending；**`lineOk[0]` 可为 true，其余行恒为 false**——这是预期行为，不是 bug。
- 若准备阶段版本已被他处更新，首行校验失败 → `batchPrepared=false`，整波失败。
- 协调者 `_quoteMev` 与 AMM 组包一致：各行 `newReserve*` 均基于 **同一入口储备快照** 并行算出（非链上累加）；**仅首行在池上真正 prepare 成功**，故 **`n>1` 时 finalize 通常因第 2 行及以后池行失败而 abort**；与 AMM「Pool 仅首行写池」时 **`max-batch` 宜为 1** 同理。
- **`ArbItem` 间 user 须互异**（Flash 批）；`txId` 一般由意图 `intentId` 填入。

## 链下工具

### csv-trigger

- **`-scenario wallet-mevarb-sparrow`**（别名 **`mevarb-sparrow`**）：**`-to=SparrowMevArbIntentShop`**，**`mevIntentExplicit(user, borrowA, minNetProfitA)`**；CSV 单列 user；**`-amount`=borrowA**，**`-min-amount-out`=minNetProfitA（≥0）**；**`is2PC=false`**，收据解析 **`SparrowMevArbIntent`**。

### bootstrap-wallet-balances-from-csv

- **`-kind mevarb-sparrow-profit-wallet`**：**`-wallet`** 为 **`MevArbProfitWalletSparrow`**，**`setBalances(users, v)`**（与 erc20 同 ABI）；可选，在压测前给利润侧记账。

### sparrow-mev-batcher

监听 **`SparrowMevArbIntent`**，聚批调用 **`arbWave`**（与 `sparrow-amm-batcher` 同结构）。

```bash
go run ./cmd/sparrow-mev-batcher --rpc http://127.0.0.1:9500 \
  --intent-shop <SparrowMevArbIntentShop> --coordinator <SparrowMevArbCoordinator> --private-key <hex>
```

### joyue-trigger

- **意图路径**：`coordinator` 留空，`agent` 填 **IntentShop**；**`mevIntentExplicit(address,uint256,uint256)`** 或 **`mevIntentRandom(uint256,uint256)`**（后者 user 为 `msg.sender`）。示例：`cmd/joyue-trigger/trigger-sparrow-mev-intent.yaml`。
- receipt 从 **`SparrowMevArbIntent`** 解析 `intentId` 为 `tx_id`（`internal/joyuetrigger/ethereum.go` 已支持）。
- **直连协调者**：`coordinator` 填协调者地址、`is2PC=true`，receipt 可从 **`SparrowWaveStarted`** 解析 `tx_id` 列表。

### sparrow-metrics

无需改代码；事件与 AMM Sparrow 相同，将 coordinator 指到 **`SparrowMevArbCoordinator`**。

## 编译

```bash
cd contracts && solc --base-path . example/mevarb-sparrow/*.sol
```
