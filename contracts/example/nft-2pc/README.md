# nft-2pc（与 JOYUE `nft-joyue` 对照的传统 2PC）

编排风格与 **`PeerAmmSwapCoordinator2PC`** 相同：协调者发跨分片 prepare 回调，阶段一并行 **Store + Wallet**，全成则双端 **commit**，否则 **abort**。

## 锁与语义

| Participant | 锁 | 说明 |
|-------------|-----|------|
| `NftStore2PC` | `lockedByTxId`（合约级） | 全局 `remainingSupply` 同一时刻仅一笔 prepare |
| `NftWallet2PC` | `accountLock[user]`（账户级） | 同一买家在途仅一笔 2PC；`setBalance` / `setBalances` 与未持锁 prepare 互斥 |

常量 **`INITIAL_REMAINING_SUPPLY = 500`**、**`INITIAL_UNIT_PRICE = 100`** 与 `NftJoyueSalesMasterV2` 默认值一致；协调者构造时的 **`unitPrice_` 必须与 Store 的 `unitPrice` 一致**，否则 `totalCost` 与链上校验会不一致。

## 合约

| 合约 | 说明 |
|------|------|
| `NftStore2PC` | `prepare(bytes32,uint256)` / `commit` / `abort` |
| `NftWallet2PC` | `prepareDebit` / `commit` / `abort`；`balanceKey` → `nft.2pc.wallet.balance:`；**`setBalance` / `setBalances`**（链下 **`bootstrap-wallet-balances-from-csv -kind nft-2pc-wallet`**） |
| `PeerNftPurchaseCoordinator2PC` | **`startPurchase(buyer, quantity)`**（买家须非零地址；**不再使用** `PeerSimulatedUsers`） |

## 链下压测

- **`bootstrap-wallet-balances-from-csv -kind nft-2pc-wallet`**：对 **`NftWallet2PC`** 灌余额；须 **`balance ≥ quantity * unitPrice`**（全表相同 **`-amount`** 为 quantity 时按笔计算）。
- **`csv-trigger -scenario nft-2pc`**（别名 **`wallet-nft-2pc`**）：**`-to`** = **`PeerNftPurchaseCoordinator2PC`**，**`-amount`** = 每笔购买份数，CSV 用 **`parseSenderColumnCSV`**（如 **`NFT.csv`** 的 **`sender`** 列作为 buyer）。**`is2PC=true`**，收据从 **`PeerNftPurchase2PCStarted`** 取 **tx_id**。

## 事件（链下指标）

与 AMM 2PC 同形，便于 **joyue-trigger** / **twopc-metrics** 解析：

- `PeerNftPurchase2PCStarted(bytes32 indexed txId, address indexed buyer, uint256 quantity, uint256 totalCost)`
- `PeerNftPurchase2PCCommitted(bytes32 indexed txId)`
- `PeerNftPurchase2PCAborted(bytes32 indexed txId)`
- `PeerNftPurchase2PCAbortReason(bytes32 indexed txId, uint8 reason, bool storeOk, bool walletOk)`（`reason==1`：阶段一未全成）

**joyue-trigger**：目标设为协调者、`coordinator` 与 `to` 同地址、`is_2pc: true` 时，`--metrics-output` 从 receipt 的 **Started** 取 `tx_id`。

**csv-trigger**：同上 **Started** 事件（与 **amm-2pc** 一致）。

**twopc-metrics**：`--coordinator`（或 `--coordinators`）填协调者地址，订阅 **Committed / Aborted** 写入完成记录（与 `PeerAmmSwap2PC*` 相同 CSV/JSONL 字段）。

**joyue-metrics** 面向 JOYUE 的 `AgentResultEmitted` / `IntentRejected`，**不用于**本 2PC 路径。

## 编译

```bash
cd contracts && solc --base-path . example/nft-2pc/*.sol
```

## 部署示例

- **shardStore** 部署 `NftStore2PC`；**shardWallet** 部署 `NftWallet2PC`。
- 协调者所在分片部署 `PeerNftPurchaseCoordinator2PC(wallet, store, shardWallet, shardStore, unitPrice)`，其中 **`unitPrice` 等于 `NftStore2PC` 部署后的 `unitPrice`（默认 100）**。

依赖 Relayer 与预编译 **0x6D / 0x74**，与 `PeerAmmSwapCoordinator2PC` 相同。
