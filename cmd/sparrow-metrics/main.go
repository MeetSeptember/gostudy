/*
Sparrow Metrics — Sparrow 系协调者完成事件采集

轮询 SparrowWaveFinished(bytes32 indexed txId, bool committed)；txId==0（buyFruitWave1/2 / buyNftWave1/2 测试）跳过，不参与指标。
SparrowCoordinator、SparrowAmmCoordinator、SparrowNftCoordinator、SparrowMevArbCoordinator 事件签名相同（SparrowWaveFinished），仅需把 --coordinator 指向对应部署地址。
与 joyue-trigger / batcher 的 sent JSONL 按 tx_id、shard_id 离线合并。

单分片（与旧版兼容）：

	sparrow-metrics --rpc http://127.0.0.1:9500 --coordinator 0x... --output ./sparrow-metrics.csv --from-block 0

多分片（单 CSV，写法对齐 joyue-metrics）：

	sparrow-metrics --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501 \
	  --coordinators 0=0x...,1=0x... --output ./sparrow-metrics.csv --from-block 0
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
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	sparrowWaveFinishedSig = crypto.Keccak256Hash([]byte("SparrowWaveFinished(bytes32,bool)"))
)

func main() {
	rpcs := flag.String("rpcs", "", "多分片 RPC：0=http://...,1=http://...（与 joyue-metrics --rpcs 同格式）")
	coordinators := flag.String("coordinators", "", "多分片协调者：0=0x...,1=0x...（与 --rpcs 同用）")
	rpc := flag.String("rpc", "", "单分片：Coordinator 所在分片 RPC")
	coordinator := flag.String("coordinator", "", "单分片：SparrowCoordinator 地址")
	shardID := flag.String("shard-id", "0", "单分片：输出中的分片 ID")
	output := flag.String("output", "", "输出文件路径（CSV/JSONL）")
	format := flag.String("format", "csv", "输出格式：csv / jsonl")
	fromBlock := flag.Uint64("from-block", 0, "起始区块（所有分片统一），0 表示从最新开始")
	pollInterval := flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
	debug := flag.Bool("debug", false, "打印调试日志")
	flag.Parse()

	// 常见误用：把 0=http://...,1=http://... 写在 --rpc、把 0=0x...,1=0x... 写在 --coordinator
	rpcMulti := strings.TrimSpace(*rpcs)
	coordMulti := strings.TrimSpace(*coordinators)
	rpcSingle := strings.TrimSpace(*rpc)
	coordSingle := strings.TrimSpace(*coordinator)
	if rpcMulti == "" && rpcSingle != "" && looksLikeShardRPCList(rpcSingle) {
		log.Printf("[WARN] 多分片 RPC 应使用 --rpcs（不是 --rpc）；已按多分片自动解析当前 --rpc 值")
		rpcMulti, rpcSingle = rpcSingle, ""
	}
	if rpcMulti != "" && coordMulti == "" && coordSingle != "" && strings.Contains(coordSingle, "=") {
		log.Printf("[WARN] 多分片协调者应使用 --coordinators（不是 --coordinator）；已自动解析当前 --coordinator 值")
		coordMulti, coordSingle = coordSingle, ""
	}

	rpcMap := make(map[string]string)
	coordMap := make(map[string]common.Address)

	if rpcMulti != "" {
		if coordMulti == "" {
			flag.Usage()
			log.Fatal("多分片模式需同时指定 --rpcs 与 --coordinators（若误用了 --coordinator 写列表，程序会尝试自动迁移；否则请检查参数）")
		}
		rpcMap = parseKv(rpcMulti)
		coordMap = parseAddrMap(coordMulti)
		if len(rpcMap) == 0 {
			log.Fatal("--rpcs 解析失败")
		}
	} else {
		if rpcSingle == "" || coordSingle == "" || *output == "" {
			flag.Usage()
			log.Fatal("单分片：--rpc 填单个 URL、--coordinator 填单个 0x 地址；多分片：--rpcs 0=http://...,1=... 与 --coordinators 0=0x...,1=...")
		}
		sid := strings.TrimSpace(*shardID)
		if sid == "" {
			sid = "0"
		}
		rpcMap[sid] = rpcSingle
		coordMap[sid] = common.HexToAddress(coordSingle)
	}

	if *output == "" {
		flag.Usage()
		log.Fatal("缺少 --output")
	}
	if *format != "csv" && *format != "jsonl" {
		log.Fatalf("--format 必须为 csv 或 jsonl，当前为 %q", *format)
	}

	log.Printf("[INFO] sparrow-metrics 启动: output=%s, format=%s, from-block=%d, shards=%v",
		*output, *format, *fromBlock, keysOf(rpcMap))

	outFile, err := os.Create(*output)
	if err != nil {
		log.Fatalf("创建输出文件失败: %v", err)
	}
	defer outFile.Close()

	var writer *csv.Writer
	if *format == "csv" {
		writer = csv.NewWriter(outFile)
		writer.Write([]string{"tx_hash", "tx_id", "shard_id", "completion_type", "success", "send_time", "completion_time", "latency_ms", "block_number", "created_at"})
		writer.Flush()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[INFO] 收到停止信号")
		cancel()
	}()

	processed := make(map[string]bool)
	var processedMu sync.Mutex
	var writeMu sync.Mutex

	startFrom := *fromBlock

	for sid, rpcURL := range rpcMap {
		shardKey := sid
		coordAddr := coordMap[shardKey]
		if coordAddr == (common.Address{}) {
			coordAddr = coordMap["0"]
		}
		if coordAddr == (common.Address{}) {
			log.Printf("[WARN] 分片 %s 缺少 coordinator 地址，跳过", shardKey)
			continue
		}
		log.Printf("[INFO] 分片 %s: rpc=%s coordinator=%s", shardKey, rpcURL, coordAddr.Hex())
		go pollSparrow(ctx, rpcURL, shardKey, coordAddr, &processed, &processedMu, &writeMu, writer, *format, outFile, startFrom, *pollInterval, *debug)
	}

	<-ctx.Done()
	log.Println("[INFO] sparrow-metrics 已停止")
}

// looksLikeShardRPCList 识别「0=http://...,1=http://...」误写在 --rpc 的情况（避免把整串当 URL）
func looksLikeShardRPCList(s string) bool {
	kv := parseKv(s)
	if len(kv) == 0 {
		return false
	}
	for _, v := range kv {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return false
		}
	}
	return true
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func parseKv(s string) map[string]string {
	m := make(map[string]string)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		if k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

func parseAddrMap(s string) map[string]common.Address {
	m := make(map[string]common.Address)
	s = strings.TrimSpace(s)
	if s == "" {
		return m
	}
	if !strings.Contains(s, "=") {
		m["0"] = common.HexToAddress(s)
		return m
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		if k != "" && v != "" {
			m[k] = common.HexToAddress(v)
		}
	}
	return m
}

func pollSparrow(ctx context.Context, rpcURL, shardID string, coordAddr common.Address,
	processed *map[string]bool, processedMu *sync.Mutex, writeMu *sync.Mutex, writer *csv.Writer, format string, outFile *os.File,
	fromBlock uint64, pollInterval time.Duration, debug bool) {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		log.Printf("[ERROR] 分片 %s 连接 RPC 失败: %v", shardID, err)
		return
	}
	defer client.Close()

	if fromBlock == 0 {
		bn, err := client.BlockNumber(ctx)
		if err != nil {
			log.Printf("[ERROR] 分片 %s 获取 block number 失败: %v", shardID, err)
			return
		}
		fromBlock = bn
		log.Printf("[INFO] 分片 %s 从区块 %d 开始", shardID, fromBlock)
	}

	tb, _ := abi.NewType("bool", "", nil)
	boolArgs := abi.Arguments{{Type: tb}}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentBlock, err := client.BlockNumber(ctx)
			if err != nil {
				continue
			}
			if currentBlock < fromBlock {
				continue
			}

			query := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(currentBlock)),
				Addresses: []common.Address{coordAddr},
				Topics:    [][]common.Hash{{sparrowWaveFinishedSig}},
			}
			logs, err := client.FilterLogs(ctx, query)
			if err != nil {
				log.Printf("[WARN] 分片 %s FilterLogs 错误 (区块 %d-%d): %v", shardID, fromBlock, currentBlock, err)
				continue
			}

			for _, l := range logs {
				if len(l.Topics) < 2 {
					continue
				}
				txID := l.Topics[1]
				if txID == (common.Hash{}) {
					if debug {
						log.Printf("[DEBUG] 分片 %s 跳过 txId=0 tx=%s", shardID, l.TxHash.Hex())
					}
					continue
				}
				key := fmt.Sprintf("%s:%s:%d", shardID, txID.Hex(), l.Index)
				processedMu.Lock()
				if (*processed)[key] {
					processedMu.Unlock()
					continue
				}
				(*processed)[key] = true
				processedMu.Unlock()

				committed := false
				if len(l.Data) > 0 {
					vals, err := boolArgs.Unpack(l.Data)
					if err == nil && len(vals) > 0 {
						if b, ok := vals[0].(bool); ok {
							committed = b
						}
					}
				}

				block, _ := client.BlockByNumber(ctx, big.NewInt(int64(l.BlockNumber)))
				completionTime := int64(0)
				if block != nil && block.Time() > 0 {
					completionTime = int64(block.Time()) * 1000
				}
				completionTimeStr := ""
				if completionTime > 0 {
					completionTimeStr = strconv.FormatInt(completionTime, 10)
				}

				ct := uint8(2)
				if committed {
					ct = 1
				}

				writeMu.Lock()
				writeRecord(writer, format, outFile, l.TxHash.Hex(), txID.Hex(), shardID, ct, committed, "", completionTimeStr, "", l.BlockNumber)
				writeMu.Unlock()
				if debug {
					log.Printf("[DEBUG] 分片 %s SparrowWaveFinished tx_id=%s committed=%v block=%d", shardID, txID.Hex(), committed, l.BlockNumber)
				}
			}

			fromBlock = currentBlock + 1
			if debug || ((fromBlock-1)%50 == 0 && fromBlock > 1) {
				log.Printf("[INFO] 分片 %s 已扫描至区块 %d", shardID, fromBlock-1)
			}
		}
	}
}

func writeRecord(writer *csv.Writer, format string, outFile *os.File, txHash, txId, shardID string, completionType uint8, success bool, sendTimeStr string, completionTimeStr string, latencyStr string, blockNum uint64) {
	createdAt := time.Now().UnixMilli()
	successStr := "false"
	if success {
		successStr = "true"
	}

	switch format {
	case "csv":
		if writer != nil {
			writer.Write([]string{txHash, txId, shardID, strconv.Itoa(int(completionType)), successStr, sendTimeStr, completionTimeStr, latencyStr, strconv.FormatUint(blockNum, 10), strconv.FormatInt(createdAt, 10)})
			writer.Flush()
		}
	case "jsonl":
		if outFile != nil {
			rec := map[string]interface{}{
				"tx_hash":         txHash,
				"tx_id":           txId,
				"shard_id":        shardID,
				"completion_type": completionType,
				"success":         success,
				"send_time":       sendTimeStr,
				"completion_time": completionTimeStr,
				"latency_ms":      latencyStr,
				"block_number":    blockNum,
				"created_at":      createdAt,
			}
			enc := json.NewEncoder(outFile)
			_ = enc.Encode(rec)
		}
	default:
		log.Printf("[WARN] 未知 format=%q，跳过写入", format)
	}
}
