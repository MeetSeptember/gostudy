package joyuetrigger

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var sigRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\(([^)]*)\)(?:\(([^)]*)\))?$`)

// IntentSent(bytes32 indexed txId, address indexed user, bytes32 fruitType, uint256 quantity) - FruitShopAgentV2，Topics[1]=txId
var intentSentSig = crypto.Keccak256Hash([]byte("IntentSent(bytes32,address,bytes32,uint256)"))

// IntentSent(bytes32 indexed txId, address indexed from, address indexed to, uint256 amount) - PeerTransferAgentV2，Topics[1]=txId
var intentSentPeerTransferSig = crypto.Keccak256Hash([]byte("IntentSent(bytes32,address,address,uint256)"))

// IntentSent(bytes32 indexed txId, address indexed user, uint256 amountIn, uint256 amountOut, uint256 minAmountOut) - AmmSwapAgentV2，Topics[1]=txId
var intentSentAmmSwapSig = crypto.Keccak256Hash([]byte("IntentSent(bytes32,address,uint256,uint256,uint256)"))

// IntentSent(bytes32 indexed txId, address indexed buyer, uint256 quantity) - NftJoyueShopAgentV2，Topics[1]=txId，共 3 个 topic
var intentSentNftJoyueSig = crypto.Keccak256Hash([]byte("IntentSent(bytes32,address,uint256)"))

// IntentRejected(bytes32 indexed txId, uint8 reason) - Agent 发出（验证失败），Topics[1]=txId
var intentRejectedSig = crypto.Keccak256Hash([]byte("IntentRejected(bytes32,uint8)"))

// TwoPCStarted(bytes32 indexed txId, bytes32 itemType, uint256 quantity, address buyer) - TwoPhaseCoordinator（水果/书），Topics[1]=txId
var twoPCStartedSig = crypto.Keccak256Hash([]byte("TwoPCStarted(bytes32,bytes32,uint256,address)"))

// PeerTransfer2PCStarted(bytes32 indexed txId, address indexed from, address indexed to, uint256 amount) - PeerTransferCoordinator2PC，Topics[1]=txId
var peerTransfer2PCStartedSig = crypto.Keccak256Hash([]byte("PeerTransfer2PCStarted(bytes32,address,address,uint256)"))

// PeerAmmSwap2PCStarted(bytes32 indexed txId, address indexed user, uint256 amountIn, uint256 minAmountOut) - PeerAmmSwapCoordinator2PC，Topics[1]=txId
var peerAmmSwap2PCStartedSig = crypto.Keccak256Hash([]byte("PeerAmmSwap2PCStarted(bytes32,address,uint256,uint256)"))

// PeerNftPurchase2PCStarted(bytes32 indexed txId, address indexed buyer, uint256 quantity, uint256 totalCost) - PeerNftPurchaseCoordinator2PC，Topics[1]=txId
var peerNftPurchase2PCStartedSig = crypto.Keccak256Hash([]byte("PeerNftPurchase2PCStarted(bytes32,address,uint256,uint256)"))

// PeerMevArb2PCStarted(bytes32 indexed txId, address indexed user, uint256 borrowA, uint256 minNetProfitA) - PeerMevArbCoordinator2PC，Topics[1]=txId
var peerMevArb2PCStartedSig = crypto.Keccak256Hash([]byte("PeerMevArb2PCStarted(bytes32,address,uint256,uint256)"))

// SparrowWaveStarted(bytes32 indexed waveId, uint256 mainTxCount, bytes32[] txIds) - SparrowCoordinator / SparrowAmmCoordinator / SparrowNftCoordinator，txIds 在 data 中
var sparrowWaveStartedSig = crypto.Keccak256Hash([]byte("SparrowWaveStarted(bytes32,uint256,bytes32[])"))

// SparrowAmmSwapIntent(address indexed user, uint256 amountIn, uint256 minOut, uint256 clientVersion, bytes32 intentId) - SparrowAmmIntentShop，intentId 在 data 末 32 字节
var sparrowAmmSwapIntentSig = crypto.Keccak256Hash([]byte("SparrowAmmSwapIntent(address,uint256,uint256,uint256,bytes32)"))

// SparrowBuyIntent（非 2PC）：data 中 intentId 即 metrics 用 tx_id（与 Coordinator BuyItem.txId 一致）
var sparrowBuyIntentSig = crypto.Keccak256Hash([]byte("SparrowBuyIntent(address,bytes32,uint256,bytes32)"))

// SparrowNftIntent(address indexed buyer, uint256 quantity, bytes32 intentId) — Topics[1]=buyer，data 前 32 为 quantity、后 32 为 intentId（tx_id）
var sparrowNftIntentSig = crypto.Keccak256Hash([]byte("SparrowNftIntent(address,uint256,bytes32)"))

// SparrowTransferIntent(address indexed from, address indexed to, uint256 amount, bytes32 intentId, bool precheckOk)
var sparrowTransferIntentSig = crypto.Keccak256Hash([]byte("SparrowTransferIntent(address,address,uint256,bytes32,bool)"))

// ChainspaceIntentStarted(bytes32 indexed intentId, ...) — ChainspaceUserClient，Topics[1]=intentId（与 ChainspaceIntentFinalized 同键；Simulator 上仍为 per-leg inputId）
var chainspaceIntentStartedSig = crypto.Keccak256Hash([]byte("ChainspaceIntentStarted(bytes32,uint8,bytes32,address,address,uint256)"))

// NftChainspaceIntentStarted(bytes32 indexed txId, ...) — NftPurchaseChainspaceUserClient，Topics[1]=txId（与 NftChainspaceIntentFinalized 同键）
var nftChainspaceIntentStartedSig = crypto.Keccak256Hash([]byte("NftChainspaceIntentStarted(bytes32,uint8,address,uint256,bytes32,uint256,uint256,uint256,bytes32)"))

// AmmChainspaceIntentStarted(bytes32 indexed txId, ...) — AmmChainspaceUserClient，Topics[1]=txId
var ammChainspaceIntentStartedSig = crypto.Keccak256Hash([]byte("AmmChainspaceIntentStarted(bytes32,uint8,address,uint256,uint256,uint256)"))

// MevBotChainspaceIntentStarted(bytes32 indexed txId, ...) — MevBotChainspaceUserClient，Topics[1]=txId
var mevBotChainspaceIntentStartedSig = crypto.Keccak256Hash([]byte("MevBotChainspaceIntentStarted(bytes32,uint8,address,uint256,uint256,uint256)"))

// SparrowMevArbIntent(address indexed user, uint256 borrowA, uint256 minNetProfitA, bytes32 intentId) — SparrowMevArbIntentShop
var sparrowMevArbIntentSig = crypto.Keccak256Hash([]byte("SparrowMevArbIntent(address,uint256,uint256,bytes32)"))

type pendingTx struct {
	TxHash   string
	ShardID  string
	SendTime int64
	RpcURL   string
	Is2PC    bool
}

type metricsWriter struct {
	mu   sync.Mutex
	file *os.File
}

func (m *metricsWriter) writeSentWithTxId(txHash, txId, shardID string, sendTime int64, blockNumber uint64, blockTime uint64) {
	if m == nil || m.file == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := map[string]interface{}{"event": "sent", "tx_hash": txHash, "send_time": sendTime, "shard_id": shardID}
	if txId != "" {
		rec["tx_id"] = txId
	}
	if blockNumber > 0 {
		rec["block_number"] = blockNumber
	}
	if blockTime > 0 {
		rec["receipt_block_time"] = blockTime * 1000
	}
	enc := json.NewEncoder(m.file)
	_ = enc.Encode(rec)
}

func receiptCollector(ctx context.Context, pendingCh <-chan pendingTx, mw *metricsWriter, pollInterval time.Duration) {
	pending := make([]pendingTx, 0, 256)
	clients := make(map[string]*ethclient.Client)
	defer func() {
		for _, c := range clients {
			if c != nil {
				c.Close()
			}
		}
	}()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-pendingCh:
			if !ok {
				pendingCh = nil
			} else {
				pending = append(pending, item)
			}
		case <-ticker.C:
			if len(pending) == 0 {
				continue
			}
			remaining := pending[:0]
			for _, p := range pending {
				client, err := getOrCreateClient(ctx, clients, p.RpcURL)
				if err != nil {
					remaining = append(remaining, p)
					continue
				}
				receipt, err := client.TransactionReceipt(ctx, common.HexToHash(p.TxHash))
				if err != nil || receipt == nil {
					remaining = append(remaining, p)
					continue
				}
				blockNum := uint64(0)
				blockTime := uint64(0)
				if receipt.BlockNumber != nil {
					blockNum = receipt.BlockNumber.Uint64()
				}
				if block, err := client.BlockByHash(ctx, receipt.BlockHash); err == nil && block != nil {
					blockTime = block.Time()
				}
				if mw != nil {
					if p.Is2PC {
						written := false
						for _, lg := range receipt.Logs {
							for _, h := range parseSparrowWaveTxIdsFromLog(lg) {
								if h == (common.Hash{}) {
									continue
								}
								mw.writeSentWithTxId(p.TxHash, h.Hex(), p.ShardID, p.SendTime, blockNum, blockTime)
								written = true
							}
						}
						if written {
							continue
						}
					}
					if txId := parseTxIdFromReceipt(receipt, p.Is2PC); txId != "" {
						mw.writeSentWithTxId(p.TxHash, txId, p.ShardID, p.SendTime, blockNum, blockTime)
					}
				}
			}
			pending = remaining
		}

		if pendingCh == nil && len(pending) == 0 {
			return
		}
	}
}

func getOrCreateClient(ctx context.Context, clients map[string]*ethclient.Client, rpcURL string) (*ethclient.Client, error) {
	if c, ok := clients[rpcURL]; ok && c != nil {
		return c, nil
	}
	c, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	clients[rpcURL] = c
	return c, nil
}

func parseTxIdFromReceipt(receipt *ethtypes.Receipt, is2PC bool) string {
	if is2PC {
		for _, l := range receipt.Logs {
			if len(l.Topics) >= 2 && (l.Topics[0] == twoPCStartedSig || l.Topics[0] == peerTransfer2PCStartedSig || l.Topics[0] == peerAmmSwap2PCStartedSig || l.Topics[0] == peerNftPurchase2PCStartedSig || l.Topics[0] == peerMevArb2PCStartedSig) {
				return l.Topics[1].Hex()
			}
		}
	} else {
		for _, l := range receipt.Logs {
			if len(l.Topics) == 2 && l.Topics[0] == sparrowAmmSwapIntentSig && len(l.Data) >= 128 {
				return common.BytesToHash(l.Data[96:128]).Hex()
			}
			if len(l.Topics) == 3 && l.Topics[0] == sparrowTransferIntentSig && len(l.Data) >= 64 {
				return common.BytesToHash(l.Data[32:64]).Hex() // intentId；precheckOk 在 data[64:96]（可选）
			}
			if len(l.Topics) == 3 && l.Topics[0] == sparrowBuyIntentSig && len(l.Data) >= 64 {
				return common.BytesToHash(l.Data[32:64]).Hex()
			}
			if len(l.Topics) == 2 && l.Topics[0] == sparrowNftIntentSig && len(l.Data) >= 64 {
				return common.BytesToHash(l.Data[32:64]).Hex()
			}
			if len(l.Topics) == 2 && l.Topics[0] == sparrowMevArbIntentSig && len(l.Data) >= 96 {
				return common.BytesToHash(l.Data[64:96]).Hex()
			}
			if len(l.Topics) >= 3 && l.Topics[0] == intentSentNftJoyueSig {
				return l.Topics[1].Hex()
			}
			if len(l.Topics) >= 2 && (l.Topics[0] == intentSentSig || l.Topics[0] == intentSentPeerTransferSig || l.Topics[0] == intentSentAmmSwapSig || l.Topics[0] == intentRejectedSig) {
				return l.Topics[1].Hex()
			}
			if len(l.Topics) >= 2 && l.Topics[0] == chainspaceIntentStartedSig {
				return l.Topics[1].Hex()
			}
			if len(l.Topics) >= 2 && l.Topics[0] == nftChainspaceIntentStartedSig {
				return l.Topics[1].Hex()
			}
			if len(l.Topics) >= 2 && l.Topics[0] == ammChainspaceIntentStartedSig {
				return l.Topics[1].Hex()
			}
			if len(l.Topics) >= 2 && l.Topics[0] == mevBotChainspaceIntentStartedSig {
				return l.Topics[1].Hex()
			}
		}
	}
	return ""
}

func parseSparrowWaveTxIdsFromLog(l *ethtypes.Log) []common.Hash {
	if l == nil || len(l.Topics) < 2 || l.Topics[0] != sparrowWaveStartedSig {
		return nil
	}
	if len(l.Data) < 96 {
		return nil
	}
	arrLen := new(big.Int).SetBytes(l.Data[64:96]).Uint64()
	if arrLen > 4096 {
		return nil
	}
	need := 96 + int(arrLen)*32
	if len(l.Data) < need {
		return nil
	}
	out := make([]common.Hash, 0, arrLen)
	for i := uint64(0); i < arrLen; i++ {
		off := 96 + int(i)*32
		out = append(out, common.BytesToHash(l.Data[off:off+32]))
	}
	return out
}

func runShard(ctx context.Context, shardID, rpcURL string, to common.Address, privKey *ecdsa.PrivateKey, calldata []byte, gasLimit uint64, gasTipGwei int64, repeat uint, intervalMs uint, pendingCh chan<- pendingTx, is2PC bool) {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		log.Printf("[shard %s] 连接 RPC 失败: %v", shardID, err)
		return
	}
	defer client.Close()

	from := crypto.PubkeyToAddress(privKey.PublicKey)
	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Printf("[shard %s] 获取 chainID 失败: %v", shardID, err)
		return
	}

	signer := ethtypes.LatestSignerForChainID(chainID)
	value := big.NewInt(0)
	continuous := repeat == 0

	for i := uint(0); ; i++ {
		if !continuous && i >= repeat {
			break
		}

		select {
		case <-ctx.Done():
			log.Printf("[shard %s] 已停止，共发送 %d 笔", shardID, i)
			return
		default:
		}

		nonce, err := client.PendingNonceAt(ctx, from)
		if err != nil {
			log.Printf("[shard %s] tx #%d 获取 nonce 失败: %v", shardID, i+1, err)
			continue
		}

		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Printf("[shard %s] tx #%d 获取 header 失败: %v", shardID, i+1, err)
			continue
		}

		var tx *ethtypes.Transaction
		if head.BaseFee != nil {
			tipCap := new(big.Int).Mul(big.NewInt(gasTipGwei), big.NewInt(1_000_000_000))
			feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tipCap)
			tx = ethtypes.NewTx(&ethtypes.DynamicFeeTx{
				ChainID:   chainID,
				Nonce:     nonce,
				GasTipCap: tipCap,
				GasFeeCap: feeCap,
				Gas:       gasLimit,
				To:        &to,
				Value:     value,
				Data:      calldata,
			})
		} else {
			gasPrice, err := client.SuggestGasPrice(ctx)
			if err != nil {
				log.Printf("[shard %s] tx #%d 获取 gasPrice 失败: %v", shardID, i+1, err)
				continue
			}
			tx = ethtypes.NewTx(&ethtypes.LegacyTx{
				Nonce:    nonce,
				GasPrice: gasPrice,
				Gas:      gasLimit,
				To:       &to,
				Value:    value,
				Data:     calldata,
			})
		}

		signedTx, err := ethtypes.SignTx(tx, signer, privKey)
		if err != nil {
			log.Printf("[shard %s] tx #%d 签名失败: %v", shardID, i+1, err)
			continue
		}
		if err := client.SendTransaction(ctx, signedTx); err != nil {
			log.Printf("[shard %s] tx #%d 发送失败: %v", shardID, i+1, err)
			continue
		}

		log.Printf("[shard %s] tx #%d sent: %s", shardID, i+1, signedTx.Hash().Hex())
		if pendingCh != nil {
			pendingCh <- pendingTx{
				TxHash:   signedTx.Hash().Hex(),
				ShardID:  shardID,
				SendTime: time.Now().UnixMilli(),
				RpcURL:   rpcURL,
				Is2PC:    is2PC,
			}
		}

		if intervalMs > 0 {
			d := time.Duration(intervalMs) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}
	}

	log.Printf("[shard %s] done: %d txs", shardID, repeat)
}

// EncodeCalldata 将 sig + 字符串参数编码为 calldata（与 joyue-trigger 一致）。
func EncodeCalldata(signature string, args []string) ([]byte, error) {
	name, inTypes, _, ok := parseSig(strings.TrimSpace(signature))
	if !ok {
		return nil, fmt.Errorf("sig 格式不对: %q", signature)
	}
	sel := crypto.Keccak256([]byte(fmt.Sprintf("%s(%s)", name, strings.Join(inTypes, ","))))[:4]
	arguments, err := buildArguments(inTypes)
	if err != nil {
		return nil, err
	}
	values, err := parseArgs(inTypes, args)
	if err != nil {
		return nil, err
	}
	enc, err := arguments.Pack(values...)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+len(enc))
	out = append(out, sel...)
	out = append(out, enc...)
	return out, nil
}

func parseSig(s string) (string, []string, []string, bool) {
	m := sigRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", nil, nil, false
	}
	name := m[1]
	in := splitCSV(m[2])
	out := splitCSV(m[3])
	return name, in, out, true
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func buildArguments(types []string) (abi.Arguments, error) {
	args := make(abi.Arguments, 0, len(types))
	for _, t := range types {
		ty, err := abi.NewType(t, "", nil)
		if err != nil {
			return nil, err
		}
		args = append(args, abi.Argument{Type: ty})
	}
	return args, nil
}

func parseArgs(types []string, args []string) ([]interface{}, error) {
	if len(types) != len(args) {
		return nil, fmt.Errorf("参数数量不匹配：sig 需要 %d 个，实际给了 %d 个", len(types), len(args))
	}
	out := make([]interface{}, 0, len(args))
	for i := range types {
		v, err := parseOne(types[i], args[i])
		if err != nil {
			return nil, fmt.Errorf("arg[%d] 解析失败: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func parseOne(typ, s string) (interface{}, error) {
	typ = strings.TrimSpace(typ)
	s = strings.TrimSpace(s)
	switch typ {
	case "address":
		return common.HexToAddress(s), nil
	case "bool":
		if s == "true" || s == "1" {
			return true, nil
		}
		if s == "false" || s == "0" {
			return false, nil
		}
		return nil, fmt.Errorf("bool 只能是 true/false/1/0")
	case "string":
		return s, nil
	case "bytes":
		return decodeHexBytes(s)
	}
	if strings.HasPrefix(typ, "bytes") && typ != "bytes" {
		var b []byte
		var err error
		if typ == "bytes32" && !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
			b = crypto.Keccak256([]byte(s))
		} else {
			b, err = decodeHexBytes(s)
			if err != nil {
				return nil, err
			}
		}
		var n int
		if typ == "bytes32" {
			n = 32
		} else if _, err := fmt.Sscanf(typ, "bytes%d", &n); err != nil {
			return nil, fmt.Errorf("无法解析 bytesN 类型: %s", typ)
		}
		if len(b) > n {
			return nil, fmt.Errorf("bytes%d 参数太长", n)
		}
		if n == 32 {
			var hash common.Hash
			copy(hash[:], b)
			return hash, nil
		}
		return nil, fmt.Errorf("暂不支持 bytes%d", n)
	}
	if strings.HasPrefix(typ, "uint") || strings.HasPrefix(typ, "int") {
		z := new(big.Int)
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			b, err := decodeHexBytes(s)
			if err != nil {
				return nil, err
			}
			z.SetBytes(b)
		} else {
			if _, ok := z.SetString(s, 10); !ok {
				return nil, fmt.Errorf("整数参数解析失败")
			}
		}
		switch typ {
		case "uint8":
			return uint8(z.Uint64()), nil
		case "uint16":
			return uint16(z.Uint64()), nil
		case "uint32":
			return uint32(z.Uint64()), nil
		case "uint64":
			return z.Uint64(), nil
		default:
			return z, nil
		}
	}
	return nil, fmt.Errorf("暂不支持的类型: %s", typ)
}

func decodeHexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if s == "" {
		return nil, fmt.Errorf("empty hex")
	}
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

// ParseTxIDFromReceipt 从收据解析 tx_id，语义与 joyue-trigger 内 parseTxIdFromReceipt 一致（供链下工具复用）。
func ParseTxIDFromReceipt(receipt *ethtypes.Receipt, is2PC bool) string {
	return parseTxIdFromReceipt(receipt, is2PC)
}
