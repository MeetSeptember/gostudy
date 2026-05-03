# amm-swap-joyue（与 wallet-transfer-joyue 分离）

简化 **JOYUE + AMM** 对照实验：**池子仅持状态**；**恒定乘积** `amountOut = reserveB * amountIn / (reserveA + amountIn)` 在 **Agent** 中计算；池储备用 `checkEq(当前值)` 生成 **STRICT** 版本 Guard；用户 **付 A（WalletA Master）**、**收 B（WalletB Master）**。

## 合约

| 合约 | 说明 |
|------|------|
| `AmmPoolMasterV2` | `JoyueCoordinatorV2`；`RESERVE_A_KEY` / `RESERVE_B_KEY` 初始 `10**24` |
| `AmmWalletATokenMaster` | 用户代币 A 余额（key `amm.joyue.walletA.balance:`）；`setBalance` / `setBalances`；链下 **`bootstrap-wallet-balances-from-csv -kind amm-joyue-wallet-a`** |
| `AmmWalletBTokenMaster` | 用户代币 B 余额（key `amm.joyue.walletB.balance:`）；可选 **`amm-joyue-wallet-b`**；swap 成功后会加 B |
| `AmmSwapAgentV2` | **`swapExplicit(user, amountIn, minAmountOut)`**；`recomputeIntent`；Intent 发往 **pool** 所在分片；池储备 **版本锁**（仅用户首进）：后续 swap 在版本未前进时 `IntentRejected(..., REASON_RESERVE_VERSION_LOCKED)`；解锁在版本变化或 `RESERVE_LOCK_TTL_BLOCKS`；`recomputeIntent` 不涉及锁 |

**不再依赖** `PeerSimulatedUsers`；用户地址与初始 A 余额由 bootstrap + CSV 与 `csv-trigger` 对齐。

## 编译

```bash
cd contracts && solc --base-path . example/amm-swap-joyue/*.sol
```

## 部署注意

- 构造 `AmmSwapAgentV2(pool, walletA, walletB, shardPool, shardA, shardB)`，与各 Master 实际分片一致。可在**多分片各部署一份 Agent**；压测时用 **`csv-trigger -scenario wallet-amm-joyue`** 与 **`wallet-joyue` 相同**的 **`-joyue-rpcs` / `-joyue-agents`** 列表轮询或随机选片发交易。
- 链下需 **JOYUE 预编译 / 缓存 / Relayer** 与现有 wallet-transfer 实验相同。
- 指标：`joyue-trigger --metrics-output` / **`csv-trigger -metrics-output`** 均可解析 `IntentSent(bytes32,address,uint256,uint256,uint256)`。`joyue-metrics` 的 `--master` 请指向 **实际发出 `AgentResultEmitted` 的 Master**（本实验一般为 **Pool Master**）；`--agent` 填各分片上的 `AmmSwapAgentV2`。

## 与 wallet-transfer-joyue 关系

- **目录分离**：转账示例仍在 `wallet-transfer-joyue/`；AMM 仅在 `amm-swap-joyue/`。
- 用户表与余额初始化与转账 JOYUE **解耦**：转账用 `PeerWalletMasterV2` + `-kind joyue`；AMM 用 **`AmmWalletATokenMaster` / `AmmWalletBTokenMaster`** + **`amm-joyue-wallet-a` / `amm-joyue-wallet-b`**。

后续 **2PC / Sparrow** 可对同一业务语义各实现一版，与本文档并列目录即可。
