# mevarb-joyue（单 Agent 简化闪电贷 + 双池套利）

## 语义

**`MevArbBotAgentV2`** 在**一条 JOYUE 意图**内完成：

1. 从 **`MevArbFlashLenderMasterV2`** 借出 `borrowA` 单位 A（`available` 减少）。
2. **`MevArbPoolLowMasterV2`**：A→B（恒定乘积 `outB = rb*dx/(ra+dx)`）。
3. **`MevArbPoolHighMasterV2`**：B→A（`outA = ra*dy/(rb+dy)`）。
4. 向 Flash **归还** `borrowA`；净利润 **`outHigh - borrowA`**（A 单位）记入 **`MevArbProfitWalletMasterV2`**（需 `>= minNetProfitA`）。

**编排与贷款分离**：RawRequest / `emitIntentViaPrecompile` 锚在 **`MevArbBotMasterV2.executeArbIntent`**；`available` 仅由 **`MevArbFlashLenderMasterV2`** 维护。不维护中间 WalletA/WalletB，仅一个利润账本。

池子初值刻意不对称，使上述路径在默认参数下**有利可图**。

## 合约

| 合约 | 说明 |
|------|------|
| `MevArbBotMasterV2` | Bot Master 锚点：`executeArbIntent`（pure 占位） |
| `MevArbFlashLenderMasterV2` | `mevarb.joyue.flash.available`（独立贷款池） |
| `MevArbPoolLowMasterV2` | `mevarb.joyue.poolLow.reserveA/B` |
| `MevArbPoolHighMasterV2` | `mevarb.joyue.poolHigh.reserveA/B` |
| `MevArbProfitWalletMasterV2` | `mevarb.joyue.profit.balance:`（每用户净利润累计）；**不**依赖 `PeerSimulatedUsers`；`setBalance` / `setBalances` 与 erc20 bootstrap 同 ABI |
| `MevArbBotAgentV2` | `arbRandom`（user=`msg.sender`）/ `arbExplicit(user,...)`；`IntentSent` 与 `AmmSwapAgentV2` 同形；**不**依赖 `PeerSimulatedUsers` |

## 编译

```bash
cd contracts && solc --base-path . example/mevarb-joyue/*.sol
```

## 部署

1. 部署五个 Master：`BotMaster`、`FlashLender`、两池、**ProfitWallet**（顺序不限，但 **BotMaster 与 FlashLender 须为不同地址**）。
2. `MevArbBotAgentV2(botMaster, flashLender, poolLow, poolHigh, profitWallet, shardBot, shardFlash, shardLow, shardHigh, shardProfit)` —— 分片 ID 与部署一致。

## 参数建议

- `borrowA`：例如 `1e20`（在默认储备下可过）。
- `minNetProfitA`：下限利润（A 单位），可为 `0` 做连通性测试。

## 链下

`IntentSent` 与 AMM 相同，**joyue-trigger** 可按现有 **`IntentSent(bytes32,address,uint256,uint256,uint256)`** 解析 `tx_id`（`Topics[1]`）。

### csv-trigger

- **`-scenario wallet-mevarb-joyue`**（别名 **`mevarb-joyue`**）：**`-to=MevArbBotAgentV2`**（多分片时与 **`wallet-amm-joyue`** 相同使用 **`-joyue-rpcs`** 与 **`-joyue-agents`** 等长列表，**`-joyue-pick`** 选片），**`arbExplicit(user, borrowA, minNetProfitA)`**；CSV 单列 **user**；**`-amount`=borrowA**，**`-min-amount-out`=minNetProfitA（≥0）**；**`is2PC=false`**，收据解析 **`IntentSent`**。

### bootstrap（可选）

压测前若需给利润侧预置链上读数，可对各分片上的 **`MevArbProfitWalletMasterV2`** 使用 **`bootstrap-wallet-balances-from-csv -kind mevarb-joyue-profit-wallet`**（与 erc20 同 **`setBalances`** ABI）；须 **空构造** 部署后再灌。

### 多分片 Agent

各分片部署一份 **`MevArbBotAgentV2`**（及对应 Master 与分片 ID），链下用 **`csv-trigger`** 的 **`-joyue-rpcs`/`-joyue-agents`** 与 **`wallet-amm-joyue`** 相同模式轮询或随机选 Agent 发交易。
