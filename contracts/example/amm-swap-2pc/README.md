# amm-swap-2pc（与 JOYUE `amm-swap-joyue` 对照）

用 **应用层严格 2PC** 实现与 JOYUE AMM 实验相近的语义：**用户用 WalletA 余额付 `amountIn`，从池子换出 `amountOut` 记入 WalletB**；池子为 **恒定乘积**（与 `AmmSwapAgentV2` 同式）。

## 合约

| 合约 | 说明 |
|------|------|
| `AmmPool2PC` | 单地址池；初始储备 `10**24`（与 `AmmPoolMasterV2` 一致）；`prepareSwap` / `commit` / `abort` |
| `AmmWalletA2PC` | 付币侧：`balanceKey`、`setBalance` / **`setBalances`**（链下 `bootstrap-wallet-balances-from-csv -kind amm-2pc-wallet-a`）；仅 `prepareDebit` / `commit` / `abort` |
| `AmmWalletB2PC` | 收币侧：同上 bootstrap **`-kind amm-2pc-wallet-b`**；`prepareLock` → `sealCredit` → `commit`/`abort`；亦可单笔 `prepareCredit`（测试用） |
| `PeerAmmSwapCoordinator2PC` | 同上；多笔可并发发起，**池子串行**由 `AmmPool2PC.lockedByTxId` 保证 |

## 2PC 顺序

1. 链下调用协调者 **`startSwap(user, amountIn, minAmountOut)`**（仅 **协调者所在分片** 发交易）；可用 **`cmd/csv-trigger -scenario amm-2pc`**（CSV 单列 `sender` / `from` / `address`，`-amount` = amountIn，`-min-amount-out` = minAmountOut）。
2. **阶段一（并行 prepare）**：同时对 **Pool `prepareSwap`**、**WalletA `prepareDebit`**、**WalletB `prepareLock`** 发跨分片调用——两钱包侧用户账户均被本笔 `txId` 占用，其它交易无法再动该用户（与 `setBalance` 互斥）。
3. 三路均成功则协调者根据 Pool 的 **returnData** 得到 `amountOut`，再对 WalletB 发 **`sealCredit(txId, user, amountOut)`**（阶段二）。
4. `sealCredit` 成功则 **阶段三** 对 Pool、WalletA、WalletB 发 `commit`；任一步失败则对三端发 `abort`。

`user` 与 `amountIn` 记在协调者 storage，**不要求** Pool 与协调者同分片（无需跨分片读 `pendingSwaps`）。

## 编译

```bash
cd contracts && solc --base-path . example/amm-swap-2pc/*.sol
```

## 部署参数

- **shardA** 部署 `AmmWalletA2PC`；**shardB** 部署 `AmmWalletB2PC`（与 JOYUE 双 Master 分片分工一致；**不**使用 `wallet-transfer-2pc`）。
- `AmmPool2PC()`：无参构造，初始储备与 `AmmPoolMasterV2` 相同（`INITIAL_RESERVE = 10**24`）。
- `PeerAmmSwapCoordinator2PC(walletA, walletB, pool, shardA, shardB, poolShard)`：`walletA` / `walletB` 分别为 `AmmWalletA2PC` / `AmmWalletB2PC` 地址。

链上需 **Relayer + 预编译 0x6D/0x74**，与 `PeerTransferCoordinator2PC` 相同。

## 压测与指标

- **只向部署了协调者的分片** 发送交易（见 `cmd/joyue-trigger/trigger-2pc-amm-twoshards.yaml`）。
- **`csv-trigger`**：`-scenario amm-2pc`，`-to` = 协调者；`joyue-trigger --metrics-output`：从 receipt 解析 `PeerAmmSwap2PCStarted` 的 `tx_id`。
- `twopc-metrics`：已支持 `PeerAmmSwap2PCCommitted` / `PeerAmmSwap2PCAborted`。

## 排障（Started 后很快 Aborted）

链上查看 **`PeerAmmSwap2PCAbortReason(txId, reason, poolOk, walletAOk, walletBOk)`**（与 `Aborted` 同笔或相邻回调交易）：

| reason | 含义 |
|--------|------|
| 1 | 阶段一未全成：看 `poolOk/walletAOk/walletBOk` 哪一路为 false（多为 `shardA/shardB/poolShard` 与部署不一致或 Relayer 失败） |
| 2 | 三路皆 true 但 `amountOut` 仍为 0：本协调者分片上 `pool` 地址读不到 pending（**Pool 必须部署在与协调者同一分片**，否则无法 staticcall 读储备） |
| 3 | `sealCredit` 跨分片请求发出失败 |
| 4 | `sealCredit` 在 WalletB 分片执行失败 |

其它说明：

1. 修改协调者逻辑后必须 **重新编译并重新部署** `PeerAmmSwapCoordinator2PC`，旧字节码不会变。
2. Relayer / 0x6D、0x74 未就绪时，阶段一易出现 reason **1**。

## 与 `amm-swap-joyue` 关系

- 目录独立；2PC 路径**不再依赖** `PeerSimulatedUsers`，用户与初始余额由 **`bootstrap-wallet-balances-from-csv`**（amm-2pc-wallet-a / amm-2pc-wallet-b）与 CSV 对齐。
