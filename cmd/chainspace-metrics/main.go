/*
Chainspace Metrics — 采集 UserClient 的 **整笔意图** 完成事件（全部 leg 回调聚合后），与 twopc-metrics 的 completion_type 对齐，便于与 joyue-trigger JSONL 离线对比。

支持合约与事件（均在 UserClient 合约所在分片）；`finalReason` 与合约 `ChainspaceReason` 对齐（OK=1 …），`completion_type`：1 成功，2 泛失败，3 锁冲突，4 业务规则，5 follower abort。
  - ChainspaceUserClient：ChainspaceIntentFinalized(bytes32 indexed intentId, bool success, uint8 finalReason)（仍兼容旧版 bool-only 的 topic）
  - NftPurchaseChainspaceUserClient：NftChainspaceIntentFinalized(bytes32, bool, uint8)（兼容旧 topic）
  - AmmChainspaceUserClient：AmmChainspaceIntentFinalized(bytes32, bool, uint8)（兼容旧 topic）
  - MevBotChainspaceUserClient：MevBotChainspaceIntentFinalized(bytes32, bool, uint8)（兼容旧 topic）

tx_id 与 joyue-trigger 从对应 Started 事件（Chainspace / Nft / Amm / MevBot 的 *IntentStarted）解析的 tx_id 一致。

各 Simulator 上的 per-leg 日志仍以各自键为准；本工具只轮询 UserClient 的 Finalized。

单分片：

	go run ./cmd/chainspace-metrics/main.go --rpc http://127.0.0.1:9500 \
	  --user-client 0x... --output ./chainspace-metrics.csv --from-block 0

多分片（UserClient 部署在分片 0 等）：

	go run ./cmd/chainspace-metrics/main.go \
	  --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501 \
	  --user-clients 0=0xUserClient... \
	  --output ./chainspace-metrics.csv --from-block 0
*/
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var chainspaceIntentFinalizedSigLegacy = crypto.Keccak256Hash([]byte("ChainspaceIntentFinalized(bytes32,bool)"))
var chainspaceIntentFinalizedSig = crypto.Keccak256Hash([]byte("ChainspaceIntentFinalized(bytes32,bool,uint8)"))
var nftChainspaceIntentFinalizedSigLegacy = crypto.Keccak256Hash([]byte("NftChainspaceIntentFinalized(bytes32,bool)"))
var nftChainspaceIntentFinalizedSig = crypto.Keccak256Hash([]byte("NftChainspaceIntentFinalized(bytes32,bool,uint8)"))
var ammChainspaceIntentFinalizedSigLegacy = crypto.Keccak256Hash([]byte("AmmChainspaceIntentFinalized(bytes32,bool)"))
var ammChainspaceIntentFinalizedSig = crypto.Keccak256Hash([]byte("AmmChainspaceIntentFinalized(bytes32,bool,uint8)"))
var mevBotChainspaceIntentFinalizedSigLegacy = crypto.Keccak256Hash([]byte("MevBotChainspaceIntentFinalized(bytes32,bool)"))
var mevBotChainspaceIntentFinalizedSig = crypto.Keccak256Hash([]byte("MevBotChainspaceIntentFinalized(bytes32,bool,uint8)"))

// userClientWatch 单分片/多分片轮询任务
type userClientWatch struct {
	shardKey string
	rpcURL   string
	addr     common.Address
}

func main() {
	rpcs := flag.String("rpcs", "", "多分片 RPC：0=http://...,1=http://...")
	userClients := flag.String("user-clients", "", "多分片：0=0xUserClient...（键与 --rpcs 一致）")
	rpc := flag.String("rpc", "", "单分片：UserClient 所在分片 RPC")
	userClient := flag.String("user-client", "", "单分片：Chainspace / Nft / Amm / MevBot Chainspace UserClient 地址")
	shardID := flag.String("shard-id", "0", "单分片：输出中的 shard_id")
	output := flag.String("output", "", "输出文件（CSV/JSONL）")
	format := flag.String("format", "csv", "csv / jsonl")
	fromBlock := flag.Uint64("from-block", 0, "起始区块，0 表示从当前最高块开始")
	pollInterval := flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
	debug := flag.Bool("debug", false, "调试日志")
	flag.Parse()

	rpcMulti := strings.TrimSpace(*rpcs)
	ucMulti := strings.TrimSpace(*userClients)
	rpcSingle := strings.TrimSpace(*rpc)
	ucSingle := strings.TrimSpace(*userClient)

	if rpcMulti == "" && rpcSingle != "" && looksLikeShardRPCList(rpcSingle) {
		log.Printf("[WARN] 多分片 RPC 应使用 --rpcs；已自动从 --rpc 解析")
		rpcMulti, rpcSingle = rpcSingle, ""
	}

	var watches []userClientWatch

	if rpcMulti != "" {
		if ucMulti == "" {
			flag.Usage()
			log.Fatal("多分片需 --rpcs 与 --user-clients")
		}
		rpcMap := parseKv(rpcMulti)
		ucMap := parseAddrMap(ucMulti)
		for sk, url := range rpcMap {
			if a, ok := ucMap[sk]; ok && a != (common.Address{}) {
				watches = append(watches, userClientWatch{shardKey: sk, rpcURL: url, addr: a})
			}
		}
		if len(watches) == 0 {
			log.Fatal("未解析到任何 user-clients（键需与 --rpcs 分片键对应）")
		}
	} else {
		if rpcSingle == "" || ucSingle == "" || *output == "" {
			flag.Usage()
			log.Fatal("单分片需 --rpc、--user-client、--output")
		}
		sid := strings.TrimSpace(*shardID)
		if sid == "" {
			sid = "0"
		}
		watches = append(watches, userClientWatch{shardKey: sid, rpcURL: rpcSingle, addr: common.HexToAddress(ucSingle)})
	}

	if *format != "csv" && *format != "jsonl" {
		log.Fatalf("--format 须为 csv 或 jsonl")
	}

	outFile, err := os.Create(*output)
	if err != nil {
		log.Fatalf("创建输出失败: %v", err)
	}
	defer outFile.Close()

	var writer *csv.Writer
	if *format == "csv" {
		writer = csv.NewWriter(outFile)
		_ = writer.Write([]string{"tx_hash", "tx_id", "shard_id", "completion_type", "success", "send_time", "completion_time", "latency_ms", "block_number", "created_at", "event_name"})
		writer.Flush()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[INFO] 停止 chainspace-metrics")
		cancel()
	}()

	processed := make(map[string]bool)
	var processedMu sync.Mutex
	var writeMu sync.Mutex
	startFrom := *fromBlock

	for _, wk := range watches {
		go poll(ctx, wk, &processed, &processedMu, &writeMu, writer, *format, outFile, startFrom, *pollInterval, *debug)
	}

	<-ctx.Done()
}

func poll(ctx context.Context, w userClientWatch, processed *map[string]bool, processedMu *sync.Mutex, writeMu *sync.Mutex,
	writer *csv.Writer, format string, outFile *os.File, fromBlock uint64, pollInterval time.Duration, debug bool) {

	client, err := ethclient.DialContext(ctx, w.rpcURL)
	if err != nil {
		log.Printf("[ERROR] userClient[%s] 连接失败: %v", w.shardKey, err)
		return
	}
	defer client.Close()

	if fromBlock == 0 {
		bn, err := client.BlockNumber(ctx)
		if err != nil {
			log.Printf("[ERROR] userClient[%s] block number: %v", w.shardKey, err)
			return
		}
		fromBlock = bn
		log.Printf("[INFO] userClient[%s] 从块 %d 开始", w.shardKey, fromBlock)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	topics := [][]common.Hash{{
		chainspaceIntentFinalizedSigLegacy, chainspaceIntentFinalizedSig,
		nftChainspaceIntentFinalizedSigLegacy, nftChainspaceIntentFinalizedSig,
		ammChainspaceIntentFinalizedSigLegacy, ammChainspaceIntentFinalizedSig,
		mevBotChainspaceIntentFinalizedSigLegacy, mevBotChainspaceIntentFinalizedSig,
	}}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur, err := client.BlockNumber(ctx)
			if err != nil {
				continue
			}
			if cur < fromBlock {
				continue
			}
			q := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(cur)),
				Addresses: []common.Address{w.addr},
				Topics:    topics,
			}
			logs, err := client.FilterLogs(ctx, q)
			if err != nil {
				log.Printf("[WARN] userClient[%s] FilterLogs %d-%d: %v", w.shardKey, fromBlock, cur, err)
				continue
			}
			for _, lg := range logs {
				key := fmt.Sprintf("%s:%s:%d:%d", w.shardKey, lg.TxHash.Hex(), lg.BlockNumber, lg.Index)
				processedMu.Lock()
				if (*processed)[key] {
					processedMu.Unlock()
					continue
				}
				(*processed)[key] = true
				processedMu.Unlock()

				block, _ := client.BlockByNumber(ctx, big.NewInt(int64(lg.BlockNumber)))
				txID := ""
				if len(lg.Topics) >= 2 {
					txID = lg.Topics[1].Hex()
				}
				t0 := common.Hash{}
				if len(lg.Topics) >= 1 {
					t0 = lg.Topics[0]
				}
				success, ct := parseIntentFinalizedPayload(t0, lg.Data)
				evName := "ChainspaceIntentFinalized"
				if len(lg.Topics) >= 1 {
					switch t0 {
					case nftChainspaceIntentFinalizedSigLegacy, nftChainspaceIntentFinalizedSig:
						evName = "NftChainspaceIntentFinalized"
					case ammChainspaceIntentFinalizedSigLegacy, ammChainspaceIntentFinalizedSig:
						evName = "AmmChainspaceIntentFinalized"
					case mevBotChainspaceIntentFinalizedSigLegacy, mevBotChainspaceIntentFinalizedSig:
						evName = "MevBotChainspaceIntentFinalized"
					}
				}
				completionTime := int64(0)
				if block != nil {
					completionTime = int64(block.Time()) * 1000
				}
				cts := ""
				if completionTime > 0 {
					cts = strconv.FormatInt(completionTime, 10)
				}
				writeMu.Lock()
				writeRecord(writer, format, outFile, lg.TxHash.Hex(), txID, w.shardKey, ct, success, "", cts, "", lg.BlockNumber, evName)
				writeMu.Unlock()
				if debug {
					log.Printf("[DEBUG] userClient[%s] %s tx_id=%s success=%v block=%d", w.shardKey, evName, txID, success, lg.BlockNumber)
				}
			}
			fromBlock = cur + 1
		}
	}
}

// parseIntentFinalizedPayload 解析 (bool success, uint8 finalReason)？旧版事件仅 32 字节 bool，无 finalReason 时按泛失败归类。
func parseIntentFinalizedPayload(topic0 common.Hash, data []byte) (success bool, completionType uint8) {
	if len(data) < 32 {
		return false, 2
	}
	success = new(big.Int).SetBytes(data[0:32]).Sign() != 0
	legacy := topic0 == chainspaceIntentFinalizedSigLegacy || topic0 == nftChainspaceIntentFinalizedSigLegacy ||
		topic0 == ammChainspaceIntentFinalizedSigLegacy || topic0 == mevBotChainspaceIntentFinalizedSigLegacy
	finalReason := uint8(0)
	if len(data) >= 64 && !legacy {
		finalReason = uint8(new(big.Int).SetBytes(data[32:64]).Uint64())
	}
	return success, completionTypeFromSuccessAndReason(success, finalReason)
}

func completionTypeFromSuccessAndReason(success bool, finalReason uint8) uint8 {
	if success {
		return 1
	}
	switch finalReason {
	case 3:
		return 3
	case 4:
		return 4
	case 5:
		return 5
	default:
		return 2
	}
}

func writeRecord(writer *csv.Writer, format string, outFile *os.File, txHash, txID, shardID string, completionType uint8, success bool, sendTime, completionTime, latency string, blockNum uint64, eventName string) {
	createdAt := time.Now().UnixMilli()
	successStr := "false"
	if success {
		successStr = "true"
	}
	switch format {
	case "csv":
		if writer != nil {
			_ = writer.Write([]string{txHash, txID, shardID, strconv.Itoa(int(completionType)), successStr, sendTime, completionTime, latency, strconv.FormatUint(blockNum, 10), strconv.FormatInt(createdAt, 10), eventName})
			writer.Flush()
		}
	case "jsonl":
		if outFile != nil {
			_ = json.NewEncoder(outFile).Encode(map[string]interface{}{
				"tx_hash":         txHash,
				"tx_id":           txID,
				"shard_id":        shardID,
				"completion_type": completionType,
				"success":         success,
				"send_time":       sendTime,
				"completion_time": completionTime,
				"latency_ms":      latency,
				"block_number":    blockNum,
				"created_at":      createdAt,
				"event_name":      eventName,
			})
		}
	}
}

func looksLikeShardRPCList(s string) bool {
	m := parseKv(s)
	if len(m) == 0 {
		return false
	}
	for _, v := range m {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return false
		}
	}
	return true
}

func parseKv(s string) map[string]string {
	out := make(map[string]string)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		i := strings.Index(p, "=")
		if i < 0 {
			continue
		}
		k, v := strings.TrimSpace(p[:i]), strings.TrimSpace(p[i+1:])
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func parseAddrMap(s string) map[string]common.Address {
	out := make(map[string]common.Address)
	s = strings.TrimSpace(s)
	if s == "" {
		return out
	}
	if !strings.Contains(s, "=") {
		out["0"] = common.HexToAddress(s)
		return out
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		i := strings.Index(p, "=")
		if i < 0 {
			continue
		}
		k, v := strings.TrimSpace(p[:i]), strings.TrimSpace(p[i+1:])
		if k != "" && v != "" {
			out[k] = common.HexToAddress(v)
		}
	}
	return out
}
