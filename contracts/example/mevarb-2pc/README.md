# mevarb-2pc（传统 2PC MEV 对照 `mevarb-joyue`）

用 **应用层 2PC** 实现与 `mevarb-joyue` 相近的套利语义：**Flash 借 A → 低价池 A→B → 高价池 B→A → 还贷 → 净利润记入利润钱包**；储备初值与 Joyue 版 **PoolLow / PoolHigh / Flash** 对齐，便于与 JOYUE 单意图对照实验。

## 合约

| 合约 | 说明 |
|------|------|
| `MevArbFlashLender2PC` | `prepareLend` / `commit` / `abort`；**`accountLock[user]`**（非合约级单锁）；`INITIAL_AVAILABLE = 10**30`；`pendingLendCount` 为 0 时才可 `setAvailable` |
| `MevArbPoolLow2PC` | A→B；`INITIAL_RESERVE_A=10**22`、`INITIAL_RESERVE_B=10**24` |
| `MevArbPoolHigh2PC` | B→A；`prepareSwapBToA`；`INITIAL_RESERVE_A=10**24`、`INITIAL_RESERVE_B=10**22` |
| `MevArbProfitWallet2PC` | `prepareLock` → `sealCredit` → `commit` / `abort`；可选 **`setBalance` / `setBalances`**（与 erc20 bootstrap 同 ABI，便于测试灌账） |
| `PeerMevArbCoordinator2PC` | 波次 1 并行 Flash+Low → 预览 `outHigh` → High → Profit lock → seal → 四端 `commit`；事件 **`PeerMevArb2PCStarted/Committed/Aborted`**；**`startArb(user, borrowA, minNetProfitA)`** 由调用方指定 `user`（**非零地址**），不再依赖模拟用户表 |

## 2PC 波次（概要）

1. **Started**：`startArb` 发出 `PeerMevArb2PCStarted(txId, user, borrowA, minNetProfitA)`。
2. **波次 1（并行 prepare）**：`prepareLend`、`prepareSwapAToB`（`minOutB` 当前固定为 `1`，与 Joyue 侧宽松检查类似）。
3. 两路成功后从低价池 prepare **回调 `ret`** 解出 `outLow`；**不**在协调者上对 poolHigh `staticcall` 预览；直接 **`prepareSwapBToA(..., minOutA = borrowA + minNetProfitA)`**，由高价池在自身分片校验；不满足则 prepare **revert**，回调 `ok=false` → abort。
4. 高价池成功后从 **回调 `ret`** 取 `outHigh`；再 **利润** `prepareLock` → `sealCredit(netProfit)`。
5. 全成则四端 **`commit`**；任阶段失败对已 prepare 端 **`abort`**。

依赖 **Relayer + 预编译 0x6D / 0x74**，与 `amm-swap-2pc` 相同。

## 编译

```bash
cd contracts && solc --base-path . example/mevarb-2pc/*.sol
```

## 部署

- 各参与者部署在构造参数对应分片；`PeerMevArbCoordinator2PC(flash, poolLow, poolHigh, profit, shardFlash, shardLow, shardHigh, shardProfit)`。
- **`outLow` / `outHigh`**：均由对应池子 prepare 的 **Executor 回调 `ret`** 解出，协调者与 `poolLow` / `poolHigh` **无需**同分片，也**无需**对池子做 staticcall。
- **利润门槛**：由 `prepareSwapBToA` 的 `minOutA`（`borrowA + minNetProfitA`）在**高价池分片**上强制，不再依赖协调者预览储备。

## 压测与指标

- **csv-trigger**：`-scenario wallet-mevarb-2pc`（别名 `mevarb-2pc`），`-to` 为 **`PeerMevArbCoordinator2PC`**；CSV 单列 **user**（表头 `sender` / `from` / `address`）；`-amount` = **borrowA**，`-min-amount-out` = **minNetProfitA**（可为 `0`）；`is2PC=true`，receipt 解析 **`PeerMevArb2PCStarted`** 得 `tx_id`（`internal/joyuetrigger` 已支持）。
- **joyue-trigger**：YAML 中目标填 **`coordinator`**（`PeerMevArbCoordinator2PC`），`is2PC=true`；同上事件解析。
- **twopc-metrics**：`--coordinator` / `--coordinators` 填协调者地址；订阅 **`PeerMevArb2PCCommitted` / `PeerMevArb2PCAborted`**（`cmd/twopc-metrics` 已支持）。

示例见 `cmd/joyue-trigger/trigger-2pc-mevarb.example.yaml`（需替换地址）；单列用户 CSV 亦可复用 `cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv` 等。

### 可选：利润钱包灌账

正常套利路径**不必**预灌利润侧余额。若需测试记账展示，可对 **`MevArbProfitWallet2PC`** 使用 **`bootstrap-wallet-balances-from-csv -kind mevarb-2pc-profit-wallet`**（与 erc20 相同 **`setBalances(users, v)`** ABI）。

## 与 `mevarb-joyue` 关系

目录独立；**不**再共享 `PeerSimulatedUsers`。指标输出字段与现有 2PC CSV/JSONL 一致，可与 `joyue-trigger --metrics-output` 按 `tx_id` 关联。
