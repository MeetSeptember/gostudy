# nft-joyue（JOYUE 简化「份数」销售）

方案 **B**：仅全局 **`nft.joyue.remainingSupply`** 递减 + 从 **`nft.joyue.wallet.balance:<user>`** 扣 **`nft.joyue.unitPrice * quantity`**。无 tokenId、无按用户 NFT 持有计数。

命名统一：**合约** `NftJoyue*`，**缓存 key** `nft.joyue.*`。

## 合约

| 合约 | 说明 |
|------|------|
| `NftJoyueSalesMasterV2` | `remainingSupply`（默认 500）、`unitPrice`（默认 100） |
| `NftJoyueWalletMasterV2` | 空构造；余额由 **`bootstrap-wallet-balances-from-csv -kind nft-joyue-wallet`** 调 `setBalances` / `setBalance` 写入 `nft.joyue.wallet.balance:<user>` |
| `NftJoyueShopAgentV2` | **`buyExplicit(buyer, quantity)`**（`buyer` 须非零）；`recomputeIntent` |

## 部署

1. 部署 `NftJoyueSalesMasterV2`、`NftJoyueWalletMasterV2`（顺序不限）。
2. `NftJoyueShopAgentV2(sales, wallet, shardSales, shardWallet)` —— 分片 ID 与实际部署一致。

## 编译

```bash
cd contracts && solc --base-path . example/nft-joyue/*.sol
```

## 指标

- **csv-trigger**：`-scenario wallet-nft-joyue` / `nft-joyue`，`-to` 为各分片 **`NftJoyueShopAgentV2`**；与 **`wallet-joyue`** 相同可使用 **`-joyue-rpcs`**、**`-joyue-agents`** 多分片轮询/随机选 Agent。
- **joyue-trigger**：`internal/joyuetrigger` 已识别 `NftJoyueShopAgentV2` 的 `IntentSent(bytes32,address,uint256)`，`--metrics-output` JSONL 中的 `tx_id` 与水果/转账/Amm 一致（从 receipt 的 `Topics[1]` 解析）。
- **joyue-metrics**：仍订阅 **Master** 的 `AgentResultEmitted` 与 **Agent** 的 `IntentRejected`；将 `--master` 设为 `NftJoyueSalesMasterV2`、`--agent` 设为 `NftJoyueShopAgentV2`（各分片地址）即可，与其它 JOYUE 场景相同。
