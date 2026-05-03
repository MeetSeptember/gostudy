# mevbot-chainspace

四参与方 Chainspace 编排：**Flash（借 A）→ Wallet（UTXO）→ Pool1（A→B）→ Pool2（B→A）**，编排器为 **`MevBotChainspaceUserClient`**。与 `amm-chainspace` 类似，编排前在 UserClient 分片用 **view 报价** 预检；四腿 **prepare** 经预编译跨分片投递，可乱序完成。

## 入口

- **`startFlashArb(address user, uint256 borrowAmount, uint256 minProfitA)`**  
  - `user`：套利账户，须与 **`MevBotWalletSimulator`** 侧 note 的 **owner** 一致，且 **非零**。  
  - `minProfitA`：要求 `aOut2 >= repayDue + minProfitA`（否则早失败，仅发 `Started` + `Finalized(false)`）。  
- **`startIntent(address user, bytes[] calls)`**：调试/自定义四腿 calldata；`calls.length == 4`。

## 合约文件

| 合约 | 说明 |
|------|------|
| `MevBotChainspaceUserClient` | 构造注入 Flash / Wallet / Pool1 / Pool2 地址与分片 |
| `MevFlashLenderSimulator` | `prepareLend` / vault |
| `MevBotWalletSimulator` | `prepareMevBot(txId, user, …)`；**`mintInitialBatch(users, amount)`** 灌每人一张 **A** note（与 chainspace bootstrap 同 ABI） |
| `MevPoolSimulator` | 两池各一实例，CPMM `quoteSwap*` |

## 链下工具

- **bootstrap**：`bootstrap-wallet-balances-from-csv -kind mevarb-chainspace-wallet`，`-wallet` 为 **`MevBotWalletSimulator`** 地址，CSV 与用户地址列表同 **`csv-trigger`**。  
- **csv-trigger**：`-scenario wallet-mevarb-chainspace`（别名 `mevarb-chainspace`），`-to` = **`MevBotChainspaceUserClient`**，CSV 单列 user；`-amount` = `borrowAmount`，`-min-amount-out` = `minProfitA`（可 0）；**`is2PC=false`**，收据解析 **`MevBotChainspaceIntentStarted`**。

## 编译

```bash
cd contracts && solc --base-path . example/mevbot-chainspace/*.sol
```
