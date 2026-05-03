/*
csv-trigger：按 CSV 逐行构造 calldata、向指定合约发交易；场景由 -scenario 选择。

-scenario：wallet-2pc（-to=Coordinator，is2PC=true）；amm-2pc（-to=PeerAmmSwapCoordinator2PC，is2PC=true，CSV 单列 user，-amount=amountIn，-min-amount-out）；wallet-mevarb-2pc / mevarb-2pc（-to=PeerMevArbCoordinator2PC，is2PC=true，同上 CSV，-amount=borrowA，-min-amount-out=minNetProfitA 可为 0）；wallet-mevarb-chainspace / mevarb-chainspace（-to=MevBotChainspaceUserClient，is2PC=false，同上 CSV，-amount=borrowAmount，-min-amount-out=minProfitA 可为 0；须 bootstrap -kind mevarb-chainspace-wallet）；wallet-mevarb-sparrow / mevarb-sparrow（-to=SparrowMevArbIntentShop，is2PC=false，mevIntentExplicit，须 sparrow-mev-batcher；可选 bootstrap -kind mevarb-sparrow-profit-wallet）；wallet-mevarb-joyue / mevarb-joyue（-to=MevArbBotAgentV2，is2PC=false，arbExplicit，同上 CSV；-amount=borrowA；-min-amount-out=minNetProfitA 可为 0；可选 bootstrap -kind mevarb-joyue-profit-wallet；与 wallet-amm-joyue 相同可用 -joyue-rpcs/-joyue-agents 多分片 Agent）；nft-2pc / wallet-nft-2pc（-to=PeerNftPurchaseCoordinator2PC，is2PC=true，CSV sender/from/address 列买家，-amount=每笔记购买份数 quantity；须 bootstrap -kind nft-2pc-wallet）；nft-chainspace / wallet-nft-chainspace（-to=NftPurchaseChainspaceUserClient，is2PC=false，同上 CSV，-amount=quantity；须 bootstrap -kind nft-chainspace-payment 灌 NFTPaymentSimulator）；wallet-nft-sparrow / nft-sparrow（-to=SparrowNftIntentShop，is2PC=false，buyNftIntentExplicit；须 bootstrap -kind nft-sparrow-wallet + sparrow-nft-batcher）；wallet-nft-joyue / nft-joyue（-to=NftJoyueShopAgentV2，is2PC=false，buyExplicit；须 bootstrap -kind nft-joyue-wallet；与 wallet-joyue / wallet-amm-joyue 相同可用 -joyue-rpcs/-joyue-agents 多分片 Agent）；wallet-amm-chainspace（-to=AmmChainspaceUserClient，is2PC=false，同上 CSV/flags）；wallet-amm-sparrow / amm-sparrow（-to=SparrowAmmIntentShop，is2PC=false，swapIntentExplicit，须 bootstrap SparrowAmmWalletA + sparrow-amm-batcher）；wallet-amm-joyue / amm-joyue（-to=AmmSwapAgentV2，is2PC=false，swapExplicit；须 bootstrap amm-joyue-wallet-a；可与 wallet-joyue 相同使用 -joyue-rpcs/-joyue-agents 多分片 Agent）；wallet-chainspace / chainspace-wallet（-to=ChainspaceUserClient，is2PC=false）；wallet-sparrow / sparrow-wallet（-to=SparrowTransferIntentShop，is2PC=false）；wallet-joyue / joyue-wallet（-to=PeerTransferAgentV2 或 -joyue-rpcs/-joyue-agents 多片，is2PC=false，calldata=transferExplicit）。

执行顺序概览：
 1. 解析 -scenario → 得到 encode 函数与 is2PC（决定收据里如何取 tx_id）。
 2. 解析发送目标：默认单 -rpc + -to；wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 多片时可配置等长的 -joyue-rpcs 与 -joyue-agents，按 -joyue-pick 选目标；每目标独立 nonce / chainId。
 3. 解析 CSV → []addressPair（wallet-2pc：两列；amm-2pc / wallet-mevarb-2pc / wallet-mevarb-chainspace / wallet-mevarb-sparrow / wallet-mevarb-joyue / nft-2pc / nft-chainspace / wallet-nft-sparrow / wallet-nft-joyue / wallet-amm-chainspace / wallet-amm-sparrow / wallet-amm-joyue：sender/from/address 列；wallet-chainspace / wallet-sparrow / wallet-joyue：两列）。
 4. 应用 -start-row / -max-rows 切片。
 5. 对每一行：encode → 对选中 RPC 取 nonce → 组 EIP-1559 或 legacy 交易 → 签名 → SendTransaction。
 6. -metrics-output：默认 -metrics-async=true，后台按每笔 tx 的 RPC 拉 receipt 写 JSONL；多片时 shard_id 为各目标 Label（0,1,… 或单目标时的 -shard-id）；wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 多片时同。
 7. 无 metrics 时仅当 -wait 为真才对每笔 WaitMined。
 8. -interval-ms 在行间 sleep（可选）。

示例 wallet-2pc（-to = PeerTransferCoordinator2PC）：

	go run ./cmd/csv-trigger -scenario wallet-2pc -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/erc20/ERC20.csv -amount 1 -metrics-output ./sent.jsonl

示例 wallet-chainspace（-to = ChainspaceUserClient；须先对 **UserClient 绑定的 ChainspaceWalletSimulator** 跑 bootstrap -kind chainspace；勿写过小 -gas）：

	go run ./cmd/csv-trigger -scenario wallet-chainspace -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/erc20/ERC20.csv -amount 1 -metrics-output ./cs-sent.jsonl

示例 wallet-sparrow（-to = SparrowTransferIntentShop；先 bootstrap -kind sparrow 灌 SparrowTransferWallet；须 sparrow-batcher 聚批）：

	go run ./cmd/csv-trigger -scenario wallet-sparrow -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/erc20/ERC20.csv -amount 1 -metrics-output ./sparrow-sent.jsonl

示例 wallet-joyue 单分片（-to = PeerTransferAgentV2；各分片 PeerWalletMasterV2 须 bootstrap -kind joyue）：

	go run ./cmd/csv-trigger -scenario wallet-joyue -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/erc20/ERC20.csv -amount 1 -metrics-output ./joyue-sent.jsonl

示例 wallet-joyue 多片（逗号分隔，与 Agent 一一对应；-joyue-pick=random|round-robin）：

	go run ./cmd/csv-trigger -scenario wallet-joyue \
	  -joyue-rpcs http://127.0.0.1:9500,http://127.0.0.1:9501 \
	  -joyue-agents 0xAgentShard0,0xAgentShard1 \
	  -joyue-pick round-robin \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/erc20/ERC20.csv -amount 1 -metrics-output ./joyue-ms.jsonl

示例 amm-2pc（-to = PeerAmmSwapCoordinator2PC；须先 bootstrap -kind amm-2pc-wallet-a / amm-2pc-wallet-b；CSV 含 sender 或 address 列）：

	go run ./cmd/csv-trigger -scenario amm-2pc -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 1000 -min-amount-out 1 -metrics-output ./amm2pc-sent.jsonl

示例 wallet-mevarb-2pc（-to = PeerMevArbCoordinator2PC，is2PC=true；CSV 单列 user；-amount=borrowA；-min-amount-out=minNetProfitA，可为 0；利润钱包不必预灌）：

	go run ./cmd/csv-trigger -scenario wallet-mevarb-2pc -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 1000 -min-amount-out 0 -metrics-output ./mevarb2pc-sent.jsonl

示例 wallet-mevarb-chainspace（-to = MevBotChainspaceUserClient；须先 bootstrap -kind mevarb-chainspace-wallet 灌 MevBotWalletSimulator；CSV 单列 user；-amount=borrowAmount；-min-amount-out=minProfitA，可为 0）：

	go run ./cmd/csv-trigger -scenario wallet-mevarb-chainspace -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 100000 -min-amount-out 0 -metrics-output ./mevarb-cs-sent.jsonl

示例 wallet-mevarb-sparrow（-to = SparrowMevArbIntentShop；须 sparrow-mev-batcher；可选 bootstrap -kind mevarb-sparrow-profit-wallet 灌 MevArbProfitWalletSparrow；CSV 单列 user；-amount=borrowA；-min-amount-out=minNetProfitA，可为 0）：

	go run ./cmd/csv-trigger -scenario wallet-mevarb-sparrow -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 1000 -min-amount-out 0 -metrics-output ./mevarb-sparrow-sent.jsonl

示例 wallet-mevarb-joyue 单分片（-to = MevArbBotAgentV2；可选 bootstrap -kind mevarb-joyue-profit-wallet 灌 MevArbProfitWalletMasterV2；CSV 单列 user；-amount=borrowA；-min-amount-out=minNetProfitA，可为 0）：

	go run ./cmd/csv-trigger -scenario wallet-mevarb-joyue -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 100000000000000000000 -min-amount-out 0 -metrics-output ./mevarb-joyue-sent.jsonl

示例 wallet-mevarb-joyue 多分片（各分片部署 MevArbBotAgentV2；与 wallet-amm-joyue 相同 -joyue-rpcs / -joyue-agents）：

	go run ./cmd/csv-trigger -scenario wallet-mevarb-joyue \
	  -joyue-rpcs http://127.0.0.1:9500,http://127.0.0.1:9501 \
	  -joyue-agents 0xMevArbAgentShard0,0xMevArbAgentShard1 \
	  -joyue-pick round-robin \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 100000000000000000000 -min-amount-out 0 -metrics-output ./mevarb-joyue-ms.jsonl

示例 nft-2pc（-to = PeerNftPurchaseCoordinator2PC；须 bootstrap -kind nft-2pc-wallet 灌 NftWallet2PC；NFT.csv 等含 sender 列，-amount 为每笔 quantity）：

	go run ./cmd/csv-trigger -scenario nft-2pc -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/nft/NFT.csv \
	  -amount 1 -metrics-output ./nft2pc-sent.jsonl

示例 nft-chainspace（-to = NftPurchaseChainspaceUserClient；须 bootstrap -kind nft-chainspace-payment 灌 NFTPaymentSimulator；CSV 含 sender/from/address，-amount 为每笔 quantity）：

	go run ./cmd/csv-trigger -scenario nft-chainspace -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/nft/nft_user_addresses.csv \
	  -amount 1 -metrics-output ./nft-cs-sent.jsonl

示例 wallet-nft-sparrow（-to = SparrowNftIntentShop；须 bootstrap -kind nft-sparrow-wallet；须 sparrow-nft-batcher；CSV 含 sender/from/address，-amount 为每笔 quantity）：

	go run ./cmd/csv-trigger -scenario wallet-nft-sparrow -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/nft/nft_user_addresses.csv \
	  -amount 1 -metrics-output ./nft-sparrow-sent.jsonl

示例 wallet-amm-chainspace（-to = AmmChainspaceUserClient；须先 bootstrap -kind amm-chainspace-wallet-a 灌 WalletA）：

	go run ./cmd/csv-trigger -scenario wallet-amm-chainspace -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/AMM.csv \
	  -amount 1000 -min-amount-out 1 -metrics-output ./amm-cs-sent.jsonl

示例 wallet-amm-sparrow（-to = SparrowAmmIntentShop；须 bootstrap -kind amm-sparrow-wallet-a 灌 SparrowAmmWalletA；须 sparrow-amm-batcher 聚批）：

	go run ./cmd/csv-trigger -scenario wallet-amm-sparrow -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 1000 -min-amount-out 1 -client-version 0 -metrics-output ./amm-sparrow-sent.jsonl

示例 wallet-amm-joyue 单分片（-to = AmmSwapAgentV2；各分片 AmmWalletATokenMaster 须 bootstrap -kind amm-joyue-wallet-a）：

	go run ./cmd/csv-trigger -scenario wallet-amm-joyue -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 1000 -min-amount-out 1 -metrics-output ./amm-joyue-sent.jsonl

示例 wallet-amm-joyue 多分片（各分片部署 AmmSwapAgentV2；与 wallet-joyue 相同 -joyue-rpcs / -joyue-agents）：

	go run ./cmd/csv-trigger -scenario wallet-amm-joyue \
	  -joyue-rpcs http://127.0.0.1:9500,http://127.0.0.1:9501 \
	  -joyue-agents 0xAmmAgentShard0,0xAmmAgentShard1 \
	  -joyue-pick round-robin \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -amount 1000 -min-amount-out 1 -metrics-output ./amm-joyue-ms.jsonl

示例 wallet-nft-joyue 单分片（-to = NftJoyueShopAgentV2；各分片 NftJoyueWalletMasterV2 须 bootstrap -kind nft-joyue-wallet）：

	go run ./cmd/csv-trigger -scenario wallet-nft-joyue -rpc http://127.0.0.1:9500 -to 0x... \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/nft/nft_user_addresses.csv \
	  -amount 1 -metrics-output ./nft-joyue-sent.jsonl

示例 wallet-nft-joyue 多分片（各分片部署 NftJoyueShopAgentV2；与 wallet-joyue 相同 -joyue-rpcs / -joyue-agents）：

	go run ./cmd/csv-trigger -scenario wallet-nft-joyue \
	  -joyue-rpcs http://127.0.0.1:9500,http://127.0.0.1:9501 \
	  -joyue-agents 0xNftJoyueAgentShard0,0xNftJoyueAgentShard1 \
	  -joyue-pick round-robin \
	  -private-key <hex> -csv cmd/joyue-trigger/triggerdata/nft/nft_user_addresses.csv \
	  -amount 1 -metrics-output ./nft-joyue-ms.jsonl
*/
package main

import (
	"context"
	"flag"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/harmony-one/harmony/internal/joyuetrigger"
)

func main() {
	scenarioName := flag.String("scenario", "wallet-2pc", "场景：wallet-2pc | amm-2pc | wallet-mevarb-2pc | wallet-mevarb-chainspace | wallet-mevarb-sparrow | wallet-mevarb-joyue | nft-2pc | …（别名 mevarb-2pc / mevarb-chainspace / mevarb-sparrow / mevarb-joyue / …）")
	rpcURL := flag.String("rpc", "http://127.0.0.1:9500", "HTTP RPC（单目标或与 wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 单 -to 联用）")
	toHex := flag.String("to", "", "交易 To：wallet-2pc=Coordinator；amm-2pc=PeerAmmSwapCoordinator2PC；wallet-mevarb-2pc=PeerMevArbCoordinator2PC；wallet-mevarb-chainspace=MevBotChainspaceUserClient；wallet-mevarb-sparrow=SparrowMevArbIntentShop；wallet-mevarb-joyue=MevArbBotAgentV2；nft-2pc=PeerNftPurchaseCoordinator2PC；nft-chainspace=NftPurchaseChainspaceUserClient；wallet-nft-sparrow=SparrowNftIntentShop；wallet-nft-joyue=NftJoyueShopAgentV2；wallet-amm-chainspace=AmmChainspaceUserClient；wallet-amm-sparrow=SparrowAmmIntentShop；wallet-amm-joyue=AmmSwapAgentV2；wallet-chainspace=ChainspaceUserClient；wallet-sparrow=SparrowTransferIntentShop；wallet-joyue=PeerTransferAgentV2（wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 多片时可省略若已配 -joyue-agents）")
	legacyCoordinator := flag.String("coordinator", "", "已弃用别名：若 -to 为空则使用本参数作为 -to")
	privHex := flag.String("private-key", "", "发送交易私钥 hex（必填）")
	csvPath := flag.String("csv", "", "CSV 路径（必填）")
	amountStr := flag.String("amount", "1", "每笔 amount（十进制，全表相同）；amm-2pc / wallet-mevarb-2pc / wallet-mevarb-chainspace / wallet-mevarb-sparrow / wallet-mevarb-joyue / wallet-amm-chainspace / wallet-amm-sparrow / wallet-amm-joyue：amountIn、borrowA 或 borrowAmount；nft-2pc / nft-chainspace / wallet-nft-sparrow / wallet-nft-joyue：购买份数 quantity；chainspace：≤from 的 note 面值；sparrow：≤balance[from]")
	minAmountOutStr := flag.String("min-amount-out", "0", "amm-2pc / wallet-amm-chainspace / wallet-amm-sparrow / wallet-amm-joyue 必填且 >0：minOut；wallet-mevarb-2pc / wallet-mevarb-sparrow / wallet-mevarb-joyue：minNetProfitA（>=0）；wallet-mevarb-chainspace：minProfitA（>=0）；其它 scenario 忽略")
	clientVersionStr := flag.String("client-version", "0", "wallet-amm-sparrow：swapIntentExplicit 的 clientVersion（十进制）；0 表示由协调者以池子权威版本代填；其它 scenario 忽略")
	gasLimit := flag.Uint64("gas", 800_000, "gas limit；wallet-chainspace / amm-2pc / wallet-mevarb-2pc / wallet-mevarb-chainspace / wallet-mevarb-sparrow / nft-2pc / nft-chainspace / wallet-nft-sparrow / wallet-amm-chainspace / wallet-amm-sparrow 若低于约 2.5M 会自动抬高；wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 若低于约 1.8M 会自动抬高（JOYUE 意图路径易 OOG）")
	gasTipGwei := flag.Int64("gas-tip-gwei", 1, "EIP-1559 tip（gwei）")
	intervalMs := flag.Uint("interval-ms", 0, "两笔发送之间间隔毫秒")
	metricsOut := flag.String("metrics-output", "", "JSONL；非空则写 tx_id（默认异步收 receipt，见 -metrics-async）")
	metricsAsync := flag.Bool("metrics-async", true, "有 -metrics-output 时：true=后台轮询 receipt、发送全速（同 joyue-trigger）；false=每笔 WaitMined（慢，约一块一笔）")
	metricsPollMs := flag.Uint("metrics-poll-ms", 1500, "异步 metrics 时轮询 receipt 间隔（毫秒）")
	waitReceipt := flag.Bool("wait", false, "无 metrics 时是否每笔等待 receipt")
	shardID := flag.String("shard-id", "0", "metrics JSONL 的 shard_id（非 wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 多目标时生效；单 Agent 时也可用其覆盖默认 0）")
	joyueRPCs := flag.String("joyue-rpcs", "", "wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 多片：逗号分隔 RPC，与 -joyue-agents 等长；空则用 -rpc/-to 单目标")
	joyueAgents := flag.String("joyue-agents", "", "wallet-joyue 多片：逗号分隔 PeerTransferAgentV2；wallet-amm-joyue 多片：逗号分隔 AmmSwapAgentV2；wallet-nft-joyue 多片：逗号分隔 NftJoyueShopAgentV2；wallet-mevarb-joyue 多片：逗号分隔 MevArbBotAgentV2；与 -joyue-rpcs 等长")
	joyuePick := flag.String("joyue-pick", "round-robin", "wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 多目标选片：round-robin | random")
	joyueSeed := flag.Int64("joyue-seed", 0, "wallet-joyue / wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 且 -joyue-pick=random 时的随机种子；0 表示非确定性")
	startRow := flag.Uint("start-row", 0, "跳过前 N 条数据行（不含表头）")
	maxRows := flag.Uint("max-rows", 0, "最多处理行数，0 不限制")
	dryRun := flag.Bool("dry-run", false, "只解析并预览，不发交易")
	flag.Parse()

	toStr := strings.TrimSpace(*toHex)
	if toStr == "" {
		toStr = strings.TrimSpace(*legacyCoordinator)
	}
	if strings.TrimSpace(*privHex) == "" || strings.TrimSpace(*csvPath) == "" {
		log.Fatal("需要 -private-key 与 -csv")
	}

	minAmountOut := new(big.Int)
	if _, ok := minAmountOut.SetString(strings.TrimSpace(*minAmountOutStr), 10); !ok {
		log.Fatalf("invalid -min-amount-out: %s", *minAmountOutStr)
	}

	clientVer := new(big.Int)
	if _, ok := clientVer.SetString(strings.TrimSpace(*clientVersionStr), 10); !ok || clientVer.Sign() < 0 {
		log.Fatalf("invalid -client-version: %s", *clientVersionStr)
	}

	sc, err := scenarioByFlag(*scenarioName, minAmountOut, clientVer)
	if err != nil {
		log.Fatal(err)
	}

	targets, err := buildSendTargets(sc, strings.TrimSpace(*rpcURL), toStr, strings.TrimSpace(*joyueRPCs), strings.TrimSpace(*joyueAgents), strings.TrimSpace(*shardID))
	if err != nil {
		log.Fatal(err)
	}

	amount := new(big.Int)
	if _, ok := amount.SetString(strings.TrimSpace(*amountStr), 10); !ok || amount.Sign() <= 0 {
		log.Fatalf("invalid -amount: %s", *amountStr)
	}

	priv, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(*privHex), "0x"))
	if err != nil {
		log.Fatalf("private-key: %v", err)
	}

	var rows []addressPair
	if sc.name == "amm-2pc" || sc.name == "wallet-mevarb-2pc" || sc.name == "wallet-mevarb-chainspace" || sc.name == "wallet-mevarb-sparrow" || sc.name == "wallet-mevarb-joyue" || sc.name == "wallet-amm-chainspace" || sc.name == "wallet-amm-sparrow" || sc.name == "wallet-amm-joyue" || sc.name == "nft-2pc" || sc.name == "nft-chainspace" || sc.name == "wallet-nft-sparrow" || sc.name == "wallet-nft-joyue" {
		rows, err = parseSenderColumnCSV(*csvPath)
	} else {
		rows, err = parseAddressPairCSV(*csvPath)
	}
	if err != nil {
		log.Fatalf("csv: %v", err)
	}
	if len(rows) == 0 {
		log.Fatal("csv: no data rows")
	}

	skip := int(*startRow)
	if skip > len(rows) {
		log.Fatalf("-start-row=%d 超过行数 %d", skip, len(rows))
	}
	rows = rows[skip:]
	if *maxRows > 0 && int(*maxRows) < len(rows) {
		rows = rows[:*maxRows]
	}

	log.Printf("[csv-trigger] scenario=%s rows=%d targets=%d amount=%s is2PC=%v dry_run=%v",
		sc.name, len(rows), len(targets), amount.String(), sc.is2PC, *dryRun)
	if sc.name == "amm-2pc" || sc.name == "wallet-mevarb-2pc" || sc.name == "wallet-mevarb-chainspace" || sc.name == "wallet-mevarb-sparrow" || sc.name == "wallet-mevarb-joyue" || sc.name == "wallet-amm-chainspace" || sc.name == "wallet-amm-sparrow" || sc.name == "wallet-amm-joyue" {
		log.Printf("[csv-trigger] %s min_out=%s", sc.name, minAmountOut.String())
	}
	if sc.name == "wallet-amm-sparrow" {
		log.Printf("[csv-trigger] wallet-amm-sparrow client_version=%s", clientVer.String())
	}

	if *dryRun {
		for i, r := range rows {
			if i >= 5 {
				log.Printf("... 省略其余 %d 行", len(rows)-5)
				break
			}
			cd, err := sc.encode(r, amount)
			if err != nil {
				log.Fatalf("dry-run encode: %v", err)
			}
			log.Printf("dry-run[%d] calldata_len=%d pair=(%s,%s)", i, len(cd), r.A.Hex(), r.B.Hex())
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()

	pool := newRPCClientPool()
	defer pool.Close()

	chainIDCache := make(map[string]*big.Int)
	var rr uint32
	joyueRnd := joyuePickRng(*joyuePick, *joyueSeed)

	var metricsFile *os.File
	if strings.TrimSpace(*metricsOut) != "" {
		metricsFile, err = os.OpenFile(strings.TrimSpace(*metricsOut), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("metrics-output: %v", err)
		}
		defer metricsFile.Close()
		log.Printf("[csv-trigger] metrics-output: async=%v poll_ms=%d", *metricsAsync, *metricsPollMs)
	}

	sender := crypto.PubkeyToAddress(priv.PublicKey)

	useAsyncMetrics := metricsFile != nil && *metricsAsync
	// 同步等收据：显式关异步 metrics，或没有 metrics 但开了 -wait
	needWaitPerTx := (metricsFile != nil && !*metricsAsync) || (metricsFile == nil && *waitReceipt)

	var pendingCh chan pendingMetric
	var collectorWg sync.WaitGroup
	if useAsyncMetrics {
		pendingCh = make(chan pendingMetric, 4096)
		collectorWg.Add(1)
		go func() {
			defer collectorWg.Done()
			runReceiptCollector(ctx, pool, pendingCh, metricsFile, sc.is2PC, strings.TrimSpace(*shardID), time.Duration(*metricsPollMs)*time.Millisecond)
		}()
	}

	txGasLimit := effectiveGasLimit(sc, *gasLimit)

	for i, pair := range rows {
		t := targets[0]
		if len(targets) > 1 && joyueMultishardScenario(sc.name) {
			t = pickJoyueTarget(targets, *joyuePick, &rr, joyueRnd)
		}

		client, err := pool.Get(ctx, t.RPC)
		if err != nil {
			log.Fatalf("row %d dial %q: %v", i, t.RPC, err)
		}
		chainID, err := chainIDFor(ctx, chainIDCache, t.RPC, client)
		if err != nil {
			log.Fatalf("row %d chainID %q: %v", i, t.RPC, err)
		}
		signer := types.LatestSignerForChainID(chainID)
		toAddr := t.Agent

		calldata, err := sc.encode(pair, amount)
		if err != nil {
			log.Fatalf("row %d encode: %v", i, err)
		}

		nonce, err := client.PendingNonceAt(ctx, sender)
		if err != nil {
			log.Fatalf("row %d nonce: %v", i, err)
		}

		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Fatalf("row %d header: %v", i, err)
		}

		tip := new(big.Int).Mul(big.NewInt(*gasTipGwei), big.NewInt(1_000_000_000))
		var tx *types.Transaction
		if head.BaseFee != nil {
			feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
			tx = types.NewTx(&types.DynamicFeeTx{
				ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
				Gas: txGasLimit, To: &toAddr, Value: big.NewInt(0), Data: calldata,
			})
		} else {
			gp, err := client.SuggestGasPrice(ctx)
			if err != nil {
				log.Fatalf("row %d gasPrice: %v", i, err)
			}
			tx = types.NewTx(&types.LegacyTx{
				Nonce: nonce, GasPrice: gp, Gas: txGasLimit, To: &toAddr, Value: big.NewInt(0), Data: calldata,
			})
		}

		stx, err := types.SignTx(tx, signer, priv)
		if err != nil {
			log.Fatalf("row %d sign: %v", i, err)
		}

		sendTime := time.Now().UnixMilli()
		if err := client.SendTransaction(ctx, stx); err != nil {
			log.Fatalf("row %d send: %v", i, err)
		}
		log.Printf("row %d/%d sent %s shard=%s rpc=%s to=%s pair=(%s,%s)", i+1, len(rows), stx.Hash().Hex(), t.Label, t.RPC, toAddr.Hex(), pair.A.Hex(), pair.B.Hex())

		if useAsyncMetrics {
			select {
			case pendingCh <- pendingMetric{txHash: stx.Hash().Hex(), sendTime: sendTime, rpcURL: t.RPC, shardLabel: t.Label}:
			case <-ctx.Done():
				log.Fatal(ctx.Err())
			}
		} else if needWaitPerTx {
			rec, err := bind.WaitMined(ctx, client, stx)
			if err != nil {
				log.Fatalf("row %d wait %s: %v", i, stx.Hash().Hex(), err)
			}
			if rec.Status != types.ReceiptStatusSuccessful {
				log.Fatalf("row %d reverted %s", i, stx.Hash().Hex())
			}
			if metricsFile != nil {
				txID := joyuetrigger.ParseTxIDFromReceipt(rec, sc.is2PC)
				var blockTime uint64
				if blk, err := client.BlockByHash(ctx, rec.BlockHash); err == nil && blk != nil {
					blockTime = blk.Time()
				}
				var blockNum uint64
				if rec.BlockNumber != nil {
					blockNum = rec.BlockNumber.Uint64()
				}
				shard := t.Label
				if shard == "" {
					shard = strings.TrimSpace(*shardID)
				}
				writeSentMetricsJSONL(metricsFile, stx.Hash().Hex(), txID, shard, sendTime, blockNum, blockTime)
			}
		}

		if *intervalMs > 0 && i+1 < len(rows) {
			select {
			case <-ctx.Done():
				log.Fatal(ctx.Err())
			case <-time.After(time.Duration(*intervalMs) * time.Millisecond):
			}
		}
	}

	if pendingCh != nil {
		close(pendingCh)
		collectorWg.Wait()
	}

	log.Printf("[csv-trigger] done scenario=%s txs=%d", sc.name, len(rows))
}

// wallet-chainspace / amm-2pc / nft-2pc / nft-chainspace / wallet-nft-sparrow / wallet-amm-chainspace / wallet-amm-sparrow 走跨分片预编译路径，500k 级 gas 极易 out-of-gas（receipt status=0）。
const minGasWalletChainspace = 2_500_000

// wallet-amm-joyue / wallet-nft-joyue / wallet-mevarb-joyue 发 Intent 跨分片，与 bootstrap -kind joyue 同量级，默认 gas 易偏紧。
const minGasWalletAmmJoyue = 1_800_000

func effectiveGasLimit(sc scenario, userGas uint64) uint64 {
	if sc.name == "wallet-chainspace" || sc.name == "amm-2pc" || sc.name == "wallet-mevarb-2pc" || sc.name == "wallet-mevarb-chainspace" || sc.name == "wallet-mevarb-sparrow" || sc.name == "nft-2pc" || sc.name == "nft-chainspace" || sc.name == "wallet-nft-sparrow" || sc.name == "wallet-amm-chainspace" || sc.name == "wallet-amm-sparrow" {
		if userGas < minGasWalletChainspace {
			log.Printf("[csv-trigger] %s: -gas=%d < %d，易全体 OOG；已改用 %d", sc.name, userGas, minGasWalletChainspace, minGasWalletChainspace)
			return minGasWalletChainspace
		}
		return userGas
	}
	if sc.name == "wallet-amm-joyue" || sc.name == "wallet-nft-joyue" || sc.name == "wallet-mevarb-joyue" {
		if userGas < minGasWalletAmmJoyue {
			log.Printf("[csv-trigger] %s: -gas=%d < %d，JOYUE 意图路径易 OOG；已改用 %d", sc.name, userGas, minGasWalletAmmJoyue, minGasWalletAmmJoyue)
			return minGasWalletAmmJoyue
		}
		return userGas
	}
	return userGas
}
