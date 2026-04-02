/*
JOYUE Trigger - 多分片并行触发工具

功能：按配置文件，并行向多个分片的合约发送调用交易。
用于模拟多人同时在不同分片触发操作（如 buyFruit）。

配置优先：通过 --config 指定 YAML 配置文件。
支持 repeat=0 表示持续发送直到 Ctrl+C。
*/

package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"gopkg.in/yaml.v2"
)

var sigRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\(([^)]*)\)(?:\(([^)]*)\))?$`)

// IntentSent(bytes32 indexed txId, address indexed user, bytes32 fruitType, uint256 quantity) - Agent 发出，Topics[1]=txId
var intentSentSig = crypto.Keccak256Hash([]byte("IntentSent(bytes32,address,bytes32,uint256)"))

// IntentRejected(bytes32 indexed txId, uint8 reason) - Agent 发出（验证失败），Topics[1]=txId
var intentRejectedSig = crypto.Keccak256Hash([]byte("IntentRejected(bytes32,uint8)"))

// TwoPCStarted(bytes32 indexed txId, bytes32 itemType, uint256 quantity, address buyer) - Coordinator 发出，Topics[1]=txId
var twoPCStartedSig = crypto.Keccak256Hash([]byte("TwoPCStarted(bytes32,bytes32,uint256,address)"))

// SparrowBuyIntent（非 2PC）：data 中 intentId 即 metrics 用 tx_id（与 Coordinator BuyItem.txId 一致）
var sparrowBuyIntentSig = crypto.Keccak256Hash([]byte("SparrowBuyIntent(address,bytes32,uint256,bytes32)"))

type ShardConfig struct {
	RPC         string   `yaml:"rpc"`
	Agent       string   `yaml:"agent"`
	Coordinator string   `yaml:"coordinator"`
	Sig         string   `yaml:"sig"`  // 可选：该分片专用调用（覆盖全局）
	Args        []string `yaml:"args"` // 可选：该分片专用参数
}

type CallSpec struct {
	Sig  string   `yaml:"sig"`
	Args []string `yaml:"args"`
}

type Config struct {
	Shards     map[string]ShardConfig `yaml:"shards"`
	Sig        string                 `yaml:"sig"`   // 单调用模式
	Args       []string               `yaml:"args"`  // 单调用模式
	Calls      []CallSpec             `yaml:"calls"` // 多调用模式（并行发送，用于锁竞争测试）
	Repeat     uint                   `yaml:"repeat"`
	IntervalMs uint                   `yaml:"interval_ms"` // 每笔发送后间隔（ms），0=不限速
	Gas        uint64                 `yaml:"gas"`
	GasTip     int64                  `yaml:"gas_tip_gwei"`
}

// pendingTx 待确认的 tx，供 receiptCollector 异步解析 txId
type pendingTx struct {
	TxHash   string
	ShardID  string
	SendTime int64
	RpcURL   string
	Is2PC    bool // true=协调者合约(2PC 或 Sparrow)，false=Agent(JOYUE)
}

// metricsWriter 用于向 joyue-metrics 输出 JSONL（tx_hash, tx_id, send_time, shard_id, block_number）
type metricsWriter struct {
	mu   sync.Mutex
	file *os.File
}

// writeSentWithTxId 写入包含 tx_id 的记录，供离线关联 metrics
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
		rec["receipt_block_time"] = blockTime * 1000 // 出块时间 Unix 戳，作为交易的接收/确认时间
	}
	enc := json.NewEncoder(m.file)
	_ = enc.Encode(rec)
}

// receiptCollector 异步轮询 receipt，解析 IntentSent 获取 txId，写入 JSONL
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

// parseTxIdFromReceipt 从 receipt 解析 txId：2PC 用 TwoPCStarted；Sparrow Intent 用 SparrowBuyIntent；JOYUE 用 IntentSent/Rejected。Sparrow 调 Coordinator 的发送侧不写 metrics（由 Intent 已写 tx_id）。
func parseTxIdFromReceipt(receipt *ethtypes.Receipt, is2PC bool) string {
	if is2PC {
		for _, l := range receipt.Logs {
			if len(l.Topics) >= 2 && l.Topics[0] == twoPCStartedSig {
				return l.Topics[1].Hex()
			}
		}
	} else {
		for _, l := range receipt.Logs {
			if len(l.Topics) == 3 && l.Topics[0] == sparrowBuyIntentSig && len(l.Data) >= 64 {
				return common.BytesToHash(l.Data[32:64]).Hex()
			}
			if len(l.Topics) >= 2 && (l.Topics[0] == intentSentSig || l.Topics[0] == intentRejectedSig) {
				return l.Topics[1].Hex()
			}
		}
	}
	return ""
}

func main() {
	configPath := flag.String("config", "", "配置文件路径（YAML）")
	privateKeyHex := flag.String("private-key", "", "私钥 hex（不带 0x）")
	metricsOutput := flag.String("metrics-output", "", "指标输出文件（JSONL），供 joyue-metrics 读取，空则不输出")
	flag.Parse()

	if *configPath == "" {
		log.Fatal("缺少 --config，指定 YAML 配置文件路径")
	}
	if *privateKeyHex == "" {
		log.Fatal("缺少 --private-key")
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("解析配置失败: %v", err)
	}

	if len(cfg.Shards) == 0 {
		log.Fatal("配置中 shards 为空")
	}
	useCalls := len(cfg.Calls) > 0
	hasGlobalSig := cfg.Sig != ""
	hasPerShardSig := false
	for _, sc := range cfg.Shards {
		if sc.Sig != "" {
			hasPerShardSig = true
			break
		}
	}
	if !useCalls && !hasGlobalSig && !hasPerShardSig {
		log.Fatal("配置中需指定 sig、calls，或各分片的 sig")
	}
	if cfg.Repeat == 0 {
		log.Printf("[INFO] repeat=0，持续模式，按 Ctrl+C 停止")
	}
	if cfg.IntervalMs > 0 {
		log.Printf("[INFO] interval_ms=%d，限速模式（约 %.1f TPS/分片）", cfg.IntervalMs, 1000.0/float64(cfg.IntervalMs))
	}
	if cfg.Gas == 0 {
		cfg.Gas = 300000
	}
	if cfg.GasTip == 0 {
		cfg.GasTip = 1
	}

	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*privateKeyHex, "0x"))
	if err != nil {
		log.Fatalf("私钥格式错误: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mw *metricsWriter
	var pendingCh chan pendingTx
	if *metricsOutput != "" {
		f, err := os.OpenFile(*metricsOutput, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("打开 metrics-output 失败: %v", err)
		}
		defer f.Close()
		mw = &metricsWriter{file: f}
		pendingCh = make(chan pendingTx, 500)
		var collectorWg sync.WaitGroup
		collectorWg.Add(1)
		go func() {
			defer collectorWg.Done()
			receiptCollector(ctx, pendingCh, mw, 1500*time.Millisecond)
		}()
		defer func() {
			close(pendingCh)
			collectorWg.Wait()
		}()
		log.Printf("[INFO] metrics-output=%s，将输出 JSONL 供 joyue-metrics 读取", *metricsOutput)
	}

	if cfg.Repeat == 0 {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			log.Printf("[INFO] 收到停止信号，停止所有分片")
			cancel()
		}()
	}

	var wg sync.WaitGroup
	for shardID, sc := range cfg.Shards {
		target := sc.Coordinator
		is2PC := target != ""
		if target == "" {
			target = sc.Agent
		}
		if sc.RPC == "" || target == "" {
			log.Printf("[WARN] 跳过分片 %s：rpc 或 agent/coordinator 为空", shardID)
			continue
		}
		targetAddr := common.HexToAddress(target)

		sig := sc.Sig
		args := sc.Args
		if sig == "" {
			sig = cfg.Sig
			args = cfg.Args
		}

		if useCalls && sig == "" {
			for i, c := range cfg.Calls {
				calldata, err := encodeCalldata(c.Sig, c.Args)
				if err != nil {
					log.Fatalf("calls[%d] 编码失败: %v", i, err)
				}
				sigStr := c.Sig
				wg.Add(1)
				go func(sid string, rpc string, to common.Address, cd []byte, sig string, twoPC bool) {
					defer wg.Done()
					log.Printf("[INFO] [shard %s] 启动 call: %s (2PC=%v)", sid, sig, twoPC)
					runShard(ctx, sid, rpc, to, privKey, cd, cfg.Gas, cfg.GasTip, cfg.Repeat, cfg.IntervalMs, pendingCh, twoPC)
				}(shardID, sc.RPC, targetAddr, calldata, sigStr, is2PC)
			}
		} else if sig != "" {
			calldata, err := encodeCalldata(sig, args)
			if err != nil {
				log.Fatalf("[shard %s] 编码失败: %v", shardID, err)
			}
			wg.Add(1)
			go func(sid string, rpc string, to common.Address, cd []byte, s string, twoPC bool) {
				defer wg.Done()
				log.Printf("[INFO] [shard %s] 启动 call: %s (2PC=%v)", sid, s, twoPC)
				runShard(ctx, sid, rpc, to, privKey, cd, cfg.Gas, cfg.GasTip, cfg.Repeat, cfg.IntervalMs, pendingCh, twoPC)
			}(shardID, sc.RPC, targetAddr, calldata, sig, is2PC)
		} else {
			log.Printf("[WARN] 跳过分片 %s：无 sig 或 calls", shardID)
		}
	}

	wg.Wait()
	log.Printf("[INFO] 全部完成")
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

		// interval_ms > 0 时限速（低负载实验用）
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

func encodeCalldata(signature string, args []string) ([]byte, error) {
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
