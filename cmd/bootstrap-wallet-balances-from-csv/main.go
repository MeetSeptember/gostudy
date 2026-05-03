/*
在已部署的模拟 Wallet 上按 CSV 地址列表分批写入：

  - -kind erc20（默认）：PeerTransferWallet2PC.setBalances(users, v)
  - -kind joyue：PeerWalletMasterV2.setBalances(users, v)（ABI 与 erc20/sparrow 相同；每分片各部署一份 Master 后分别 bootstrap）
  - -kind amm-2pc-wallet-a / amm-2pc-wallet-b：AmmWalletA2PC / AmmWalletB2PC.setBalances(users, v)（须先部署空构造钱包；CSV 首列表头为 address 或 sender）
  - -kind nft-2pc-wallet：NftWallet2PC.setBalances(users, v)（与 erc20 同 ABI；须空构造；csv-trigger nft-2pc 前灌买家余额）
  - -kind mevarb-2pc-profit-wallet：MevArbProfitWallet2PC.setBalances(users, v)（与 erc20 同 ABI；须空构造；可选，在压测前给利润侧记账余额；正常套利路径可不预灌）
  - -kind mevarb-sparrow-profit-wallet：MevArbProfitWalletSparrow.setBalances(users, v)（与 erc20 同 ABI；须空构造；csv-trigger wallet-mevarb-sparrow 前可选灌利润侧余额）
  - -kind mevarb-joyue-profit-wallet：MevArbProfitWalletMasterV2.setBalances(users, v)（与 erc20 同 ABI；须空构造；各分片灌 Master 后 csv-trigger wallet-mevarb-joyue 前可选灌利润侧）
  - -kind nft-sparrow-wallet：SparrowNftWallet.setBalances(users, v)（与 erc20 / nft-2pc-wallet 同 ABI；须空构造；NFT Sparrow 压测前灌 SparrowNftWallet）
  - -kind amm-sparrow-wallet-a：SparrowAmmWalletA.setBalances(users, v)（压测一般只灌 A；B 由 swap commit 入账；须空构造后再跑）
  - -kind amm-joyue-wallet-a / amm-joyue-wallet-b：AmmWalletATokenMaster / AmmWalletBTokenMaster.setBalances(users, v)（与 erc20 同 ABI；须空构造后再跑；csv-trigger wallet-amm-joyue 至少灌 A）
  - -kind nft-joyue-wallet：NftJoyueWalletMasterV2.setBalances(users, v)（与 erc20 / joyue 同 ABI；须空构造；各分片灌 Master 后 csv-trigger wallet-nft-joyue）
  - -kind chainspace：ChainspaceWalletSimulator.mintInitialBatch(users, amount)，每人一张 ACTIVE note（须先部署空构造的 Simulator，再跑本工具）
  - -kind amm-chainspace-wallet-a：AmmWalletASimulator.mintInitialBatch(users, amount)（与 chainspace 同 ABI；-wallet 为 WalletA 地址；须空构造后再跑）
  - -kind nft-chainspace-payment：NFTPaymentSimulator.mintInitialBatch(users, amount)（与 chainspace 同 ABI；-wallet 为 Payment Simulator；须空构造后再跑）
  - -kind mevarb-chainspace-wallet：MevBotWalletSimulator.mintInitialBatch(users, amount)（与 chainspace 同 ABI；-wallet 为 Wallet 分片上的 MevBotWalletSimulator；csv-trigger wallet-mevarb-chainspace 前灌初始 A note）

默认不在每批之间 WaitMined：连发多笔后并行等待全部 receipt。

示例 2PC：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/erc20/erc20_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 Chainspace（-wallet 为 ChainspaceWalletSimulator 地址）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind chainspace \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/erc20/erc20_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 Sparrow（-wallet 为 SparrowTransferWallet；再部署 IntentShop(wallet)）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind sparrow \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/erc20/erc20_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 JOYUE（-wallet 为该分片上的 PeerWalletMasterV2）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind joyue \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/erc20/erc20_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 AMM 2PC 钱包（须对 A、B 各跑一次；CSV 可用 cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind amm-2pc-wallet-a \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind amm-2pc-wallet-b \
	  -rpc http://127.0.0.1:9501 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 JOYUE AMM WalletA / B（-wallet 为各分片上 AmmWalletATokenMaster / AmmWalletBTokenMaster；与 erc20 同 ABI）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind amm-joyue-wallet-a \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind amm-joyue-wallet-b \
	  -rpc http://127.0.0.1:9501 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 NFT 2PC 钱包（-wallet 为 NftWallet2PC；与 amm-2pc-wallet-a 同 ABI）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind nft-2pc-wallet \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/nft/nft_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 MEV 2PC 利润钱包（-wallet 为 MevArbProfitWallet2PC；与 erc20 同 ABI；可选）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind mevarb-2pc-profit-wallet \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/erc20/erc20_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 MEV Sparrow 利润钱包（-wallet 为 MevArbProfitWalletSparrow；与 erc20 同 setBalances ABI；可选）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind mevarb-sparrow-profit-wallet \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/erc20/erc20_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 MEV JOYUE 利润钱包（-wallet 为各分片 MevArbProfitWalletMasterV2；与 erc20 同 setBalances ABI；可选；供 csv-trigger wallet-mevarb-joyue）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind mevarb-joyue-profit-wallet \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 NFT Sparrow 钱包（-wallet 为 SparrowNftWallet；与 nft-2pc-wallet 同 ABI）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind nft-sparrow-wallet \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/nft/nft_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 NFT JOYUE 钱包（-wallet 为各分片 NftJoyueWalletMasterV2；与 joyue / nft-2pc-wallet 同 ABI）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind nft-joyue-wallet \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/nft/nft_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 Sparrow AMM WalletA（-wallet 为 SparrowAmmWalletA；与 amm-2pc-wallet-a 同 ABI）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind amm-sparrow-wallet-a \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 1000000 \
	  -batch 20

示例 AMM Chainspace WalletA（每人一张 A 侧 ACTIVE note，面值 -balance）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind amm-chainspace-wallet-a \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 10000000 \
	  -batch 10

示例 MevBot Chainspace Wallet（-wallet 为 MevBotWalletSimulator；与 chainspace 同 mintInitialBatch ABI；供 csv-trigger wallet-mevarb-chainspace）：

	go run ./cmd/bootstrap-wallet-balances-from-csv \
	  -kind mevarb-chainspace-wallet \
	  -rpc http://127.0.0.1:9500 \
	  -wallet 0x... \
	  -private-key <hex> \
	  -csv cmd/joyue-trigger/triggerdata/amm/amm_user_addresses.csv \
	  -balance 10000000 \
	  -batch 10
*/
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const erc20WalletABIJSON = `[{"name":"setBalance","type":"function","stateMutability":"nonpayable","inputs":[{"name":"user","type":"address"},{"name":"v","type":"uint256"}],"outputs":[]},{"name":"setBalances","type":"function","stateMutability":"nonpayable","inputs":[{"name":"users","type":"address[]"},{"name":"v","type":"uint256"}],"outputs":[]}]`

const chainspaceSimulatorABIJSON = `[{"name":"mintInitialBatch","type":"function","stateMutability":"nonpayable","inputs":[{"name":"users","type":"address[]"},{"name":"amount","type":"uint256"}],"outputs":[]},{"name":"mintInitial","type":"function","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bytes32"}]}]`

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:9500", "RPC URL")
	kind := flag.String("kind", "erc20", "erc20=...；mevarb-sparrow-profit-wallet=...；mevarb-joyue-profit-wallet=MevArbProfitWalletMasterV2.setBalances；mevarb-chainspace-wallet=...；mevarb-2pc-profit-wallet=...；chainspace=...；sparrow=...")
	walletAddr := flag.String("wallet", "", "合约地址（mevarb-sparrow-profit-wallet=MevArbProfitWalletSparrow；mevarb-joyue-profit-wallet=MevArbProfitWalletMasterV2；mevarb-chainspace-wallet=MevBotWalletSimulator；mevarb-2pc-profit-wallet=MevArbProfitWallet2PC；…）")
	privHex := flag.String("private-key", "", "发送交易的私钥 hex（必填）")
	csvPath := flag.String("csv", "cmd/joyue-trigger/triggerdata/erc20/erc20_user_addresses.csv", "CSV：首列表头 address 或 sender，单列地址")
	balanceStr := flag.String("balance", "1000000", "erc20/sparrow/joyue/amm-2pc-wallet-* / nft-2pc-wallet / mevarb-2pc-profit-wallet / mevarb-sparrow-profit-wallet / mevarb-joyue-profit-wallet / mevarb-chainspace-wallet / nft-sparrow-wallet / nft-joyue-wallet / amm-joyue-wallet-* / amm-sparrow-wallet-a：每地址余额 v；chainspace/amm-chainspace-wallet-a/nft-chainspace-payment/mevarb-chainspace-wallet：每张 mint note 的面值 amount（十进制，整批相同）")
	batchSize := flag.Int("batch", 10, "每笔交易的地址个数（>=1）")
	gasLimit := flag.Uint64("gas", 0, "单笔交易 gas limit；0 则按 kind/batch 估算")
	gasTipGwei := flag.Int64("gas-tip-gwei", 1, "EIP-1559 tip（gwei）；legacy 链忽略 tip 仅用 gasPrice")
	waitEachBatch := flag.Bool("wait-each-batch", false, "每批 WaitMined 后再发下一批；默认 false 为连发后并行等收据")
	flag.Parse()

	kindNorm := strings.ToLower(strings.TrimSpace(*kind))
	if kindNorm == "" {
		kindNorm = "erc20"
	}

	if strings.TrimSpace(*walletAddr) == "" || strings.TrimSpace(*privHex) == "" {
		flag.Usage()
		log.Fatal("需要 -wallet 与 -private-key")
	}
	wallet := common.HexToAddress(strings.TrimSpace(*walletAddr))
	if !common.IsHexAddress(*walletAddr) {
		log.Fatalf("invalid -wallet: %s", *walletAddr)
	}

	if *batchSize < 1 {
		log.Fatal("-batch 必须 >= 1")
	}

	balance := new(big.Int)
	if _, ok := balance.SetString(strings.TrimSpace(*balanceStr), 10); !ok || balance.Sign() < 0 {
		log.Fatalf("invalid -balance: %s", *balanceStr)
	}

	priv, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(*privHex), "0x"))
	if err != nil {
		log.Fatalf("private-key: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	client, err := ethclient.DialContext(ctx, strings.TrimSpace(*rpcURL))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer client.Close()

	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Fatalf("chainID: %v", err)
	}

	addrs, err := readAddressesCSV(*csvPath)
	if err != nil {
		log.Fatalf("csv: %v", err)
	}
	if len(addrs) == 0 {
		log.Fatal("csv: no addresses")
	}

	var contractABI abi.ABI
	var packMethod string
	switch kindNorm {
	case "erc20":
		contractABI, err = abi.JSON(strings.NewReader(erc20WalletABIJSON))
		if err != nil {
			log.Fatalf("abi: %v", err)
		}
		packMethod = "setBalances"
	case "chainspace", "amm-chainspace-wallet-a", "nft-chainspace-payment", "mevarb-chainspace-wallet":
		contractABI, err = abi.JSON(strings.NewReader(chainspaceSimulatorABIJSON))
		if err != nil {
			log.Fatalf("abi: %v", err)
		}
		packMethod = "mintInitialBatch"
	case "sparrow", "joyue", "amm-2pc-wallet-a", "amm-2pc-wallet-b", "amm-sparrow-wallet-a", "amm-joyue-wallet-a", "amm-joyue-wallet-b", "nft-2pc-wallet", "mevarb-2pc-profit-wallet", "mevarb-sparrow-profit-wallet", "mevarb-joyue-profit-wallet", "nft-sparrow-wallet", "nft-joyue-wallet":
		contractABI, err = abi.JSON(strings.NewReader(erc20WalletABIJSON))
		if err != nil {
			log.Fatalf("abi: %v", err)
		}
		packMethod = "setBalances"
	default:
		log.Fatalf("未知 -kind %q（支持 erc20、joyue、amm-2pc-wallet-a、amm-2pc-wallet-b、nft-2pc-wallet、mevarb-2pc-profit-wallet、mevarb-sparrow-profit-wallet、mevarb-joyue-profit-wallet、mevarb-chainspace-wallet、nft-sparrow-wallet、nft-joyue-wallet、amm-joyue-wallet-a、amm-joyue-wallet-b、amm-sparrow-wallet-a、amm-chainspace-wallet-a、nft-chainspace-payment、chainspace、sparrow）", *kind)
	}

	gasPerTx := *gasLimit
	autoGas := gasPerTx == 0
	if gasPerTx == 0 {
		if kindNorm == "chainspace" {
			// mintInitialBatch：多笔冷存储 SSTORE，默认给足余量；仍可在循环内 EstimateGas 收紧/抬高
			gasPerTx = 250_000 + uint64(*batchSize)*95_000
		} else if kindNorm == "joyue" || kindNorm == "nft-joyue-wallet" || kindNorm == "mevarb-joyue-profit-wallet" {
			// PeerWalletMasterV2 / NftJoyueWalletMasterV2 / MevArbProfitWalletMasterV2.setBalances：每用户 _setUint + StateBroadcast，冷 key 时远高于 erc20 的线性近似
			gasPerTx = 150_000 + uint64(*batchSize)*90_000
		} else {
			gasPerTx = 80_000 + uint64(*batchSize)*25_000
		}
		if gasPerTx < 200_000 {
			gasPerTx = 200_000
		}
		if (kindNorm == "chainspace" || kindNorm == "amm-chainspace-wallet-a" || kindNorm == "nft-chainspace-payment" || kindNorm == "mevarb-chainspace-wallet") && gasPerTx < 2_500_000 {
			gasPerTx = 2_500_000
		}
		if (kindNorm == "joyue" || kindNorm == "amm-joyue-wallet-a" || kindNorm == "amm-joyue-wallet-b" || kindNorm == "nft-joyue-wallet" || kindNorm == "mevarb-joyue-profit-wallet") && gasPerTx < 1_800_000 {
			gasPerTx = 1_800_000
		}
	}

	from := crypto.PubkeyToAddress(priv.PublicKey)
	log.Printf("[bootstrap] kind=%s wallet=%s sender=%s addresses=%d batch=%d balance=%s gas=%d wait_each_batch=%v",
		kindNorm, wallet.Hex(), from.Hex(), len(addrs), *batchSize, balance.String(), gasPerTx, *waitEachBatch)

	signer := types.LatestSignerForChainID(chainID)
	tip := new(big.Int).Mul(big.NewInt(*gasTipGwei), big.NewInt(1_000_000_000))

	if *waitEachBatch {
		runSyncBatches(ctx, client, wallet, from, priv, signer, tip, chainID, gasPerTx, autoGas, kindNorm, contractABI, packMethod, balance, addrs, *batchSize)
		log.Printf("[bootstrap] done %d addresses", len(addrs))
		return
	}

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		log.Fatalf("nonce: %v", err)
	}

	var signed []*types.Transaction
	txNum := 0
	for start := 0; start < len(addrs); start += *batchSize {
		end := start + *batchSize
		if end > len(addrs) {
			end = len(addrs)
		}
		chunk := addrs[start:end]
		txNum++

		calldata, err := contractABI.Pack(packMethod, chunk, balance)
		if err != nil {
			log.Fatalf("pack %s: %v", packMethod, err)
		}

		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Fatalf("header: %v", err)
		}

		txGas := gasPerTx
		if autoGas && (kindNorm == "chainspace" || kindNorm == "amm-chainspace-wallet-a" || kindNorm == "nft-chainspace-payment" || kindNorm == "mevarb-chainspace-wallet" || kindNorm == "joyue" || kindNorm == "nft-joyue-wallet" || kindNorm == "mevarb-joyue-profit-wallet" || kindNorm == "amm-joyue-wallet-a" || kindNorm == "amm-joyue-wallet-b") {
			txGas = gasWithEstimate(ctx, client, from, wallet, calldata, head, tip, gasPerTx)
		}

		var tx *types.Transaction
		if head.BaseFee != nil {
			feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
			tx = types.NewTx(&types.DynamicFeeTx{
				ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
				Gas: txGas, To: &wallet, Value: big.NewInt(0), Data: calldata,
			})
		} else {
			gp, err := client.SuggestGasPrice(ctx)
			if err != nil {
				log.Fatalf("gasPrice: %v", err)
			}
			tx = types.NewTx(&types.LegacyTx{
				Nonce: nonce, GasPrice: gp, Gas: txGas, To: &wallet, Value: big.NewInt(0), Data: calldata,
			})
		}

		stx, err := types.SignTx(tx, signer, priv)
		if err != nil {
			log.Fatalf("sign: %v", err)
		}
		if err := client.SendTransaction(ctx, stx); err != nil {
			log.Fatalf("[tx %d] send [%d,%d): %v", txNum, start, end, err)
		}
		log.Printf("[tx %d] sent [%d,%d) n=%d gas=%d hash=%s", txNum, start, end, len(chunk), txGas, stx.Hash().Hex())
		signed = append(signed, stx)
		nonce++
	}

	log.Printf("[bootstrap] waiting %d receipts...", len(signed))
	waitAllReceipts(ctx, client, signed)
	log.Printf("[bootstrap] done kind=%s %d addresses in %d txs", kindNorm, len(addrs), len(signed))
}

// gasWithEstimate 对 chainspace / joyue 等重 calldata 路径做 eth_estimateGas，并加 headroom，避免默认 gas 偏紧导致 status=0。
func gasWithEstimate(ctx context.Context, client *ethclient.Client, from, wallet common.Address, calldata []byte, head *types.Header, tip *big.Int, floor uint64) uint64 {
	msg := ethereum.CallMsg{From: from, To: &wallet, Value: big.NewInt(0), Data: calldata}
	if head.BaseFee != nil {
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		msg.GasFeeCap = feeCap
		msg.GasTipCap = tip
	} else {
		gp, err := client.SuggestGasPrice(ctx)
		if err != nil {
			return floor
		}
		msg.GasPrice = gp
	}
	est, err := client.EstimateGas(ctx, msg)
	if err != nil {
		log.Printf("[bootstrap] EstimateGas: %v（使用 floor=%d）", err, floor)
		return floor
	}
	// +25% 与固定缓冲，应对 estimate 与实际上链间状态差异
	with := est + est/4 + 150_000
	if with < floor {
		return floor
	}
	return with
}

func runSyncBatches(ctx context.Context, client *ethclient.Client, wallet, from common.Address, priv *ecdsa.PrivateKey, signer types.Signer, tip, chainID *big.Int, gasPerTx uint64, autoGas bool, kindNorm string, contractABI abi.ABI, packMethod string, balance *big.Int, addrs []common.Address, batchSize int) {
	txNum := 0
	for start := 0; start < len(addrs); start += batchSize {
		end := start + batchSize
		if end > len(addrs) {
			end = len(addrs)
		}
		chunk := addrs[start:end]
		txNum++

		calldata, err := contractABI.Pack(packMethod, chunk, balance)
		if err != nil {
			log.Fatalf("pack %s: %v", packMethod, err)
		}

		nonce, err := client.PendingNonceAt(ctx, from)
		if err != nil {
			log.Fatalf("nonce: %v", err)
		}

		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Fatalf("header: %v", err)
		}

		txGas := gasPerTx
		if autoGas && (kindNorm == "chainspace" || kindNorm == "amm-chainspace-wallet-a" || kindNorm == "nft-chainspace-payment" || kindNorm == "mevarb-chainspace-wallet" || kindNorm == "joyue" || kindNorm == "nft-joyue-wallet" || kindNorm == "mevarb-joyue-profit-wallet" || kindNorm == "amm-joyue-wallet-a" || kindNorm == "amm-joyue-wallet-b") {
			txGas = gasWithEstimate(ctx, client, from, wallet, calldata, head, tip, gasPerTx)
		}

		var tx *types.Transaction
		if head.BaseFee != nil {
			feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
			tx = types.NewTx(&types.DynamicFeeTx{
				ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
				Gas: txGas, To: &wallet, Value: big.NewInt(0), Data: calldata,
			})
		} else {
			gp, err := client.SuggestGasPrice(ctx)
			if err != nil {
				log.Fatalf("gasPrice: %v", err)
			}
			tx = types.NewTx(&types.LegacyTx{
				Nonce: nonce, GasPrice: gp, Gas: txGas, To: &wallet, Value: big.NewInt(0), Data: calldata,
			})
		}

		stx, err := types.SignTx(tx, signer, priv)
		if err != nil {
			log.Fatalf("sign: %v", err)
		}
		if err := client.SendTransaction(ctx, stx); err != nil {
			log.Fatalf("[tx %d] send [%d,%d): %v", txNum, start, end, err)
		}

		rec, err := bind.WaitMined(ctx, client, stx)
		if err != nil {
			log.Fatalf("[tx %d] wait %s: %v", txNum, stx.Hash().Hex(), err)
		}
		if rec.Status != types.ReceiptStatusSuccessful {
			log.Fatalf("[tx %d] reverted %s range [%d,%d)", txNum, stx.Hash().Hex(), start, end)
		}
		log.Printf("[tx %d] ok [%d,%d) n=%d hash=%s", txNum, start, end, len(chunk), stx.Hash().Hex())
	}
}

func waitAllReceipts(ctx context.Context, client *ethclient.Client, signed []*types.Transaction) {
	var wg sync.WaitGroup
	errCh := make(chan error, len(signed))
	for _, stx := range signed {
		stx := stx
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, err := bind.WaitMined(ctx, client, stx)
			if err != nil {
				errCh <- fmt.Errorf("%s: %w", stx.Hash().Hex(), err)
				return
			}
			if rec.Status != types.ReceiptStatusSuccessful {
				errCh <- fmt.Errorf("%s: reverted status=%d", stx.Hash().Hex(), rec.Status)
				return
			}
			errCh <- nil
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			log.Fatal(e)
		}
	}
}

func readAddressesCSV(path string) ([]common.Address, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	if len(header) == 0 {
		return nil, fmt.Errorf("empty csv header")
	}
	h0 := strings.TrimSpace(header[0])
	if !strings.EqualFold(h0, "address") && !strings.EqualFold(h0, "sender") {
		return nil, fmt.Errorf(`expected first column header "address" or "sender", got %q`, h0)
	}

	var out []common.Address
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) == 0 {
			continue
		}
		line := strings.TrimSpace(rec[0])
		if line == "" {
			continue
		}
		if !common.IsHexAddress(line) {
			return nil, fmt.Errorf("invalid address %q", line)
		}
		out = append(out, common.HexToAddress(line))
	}
	return out, nil
}
