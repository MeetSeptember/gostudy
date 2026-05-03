/*
2PC Metrics — 统一采集各类 2PC Coordinator 的「开始 / 提交 / 中止」相关日志

完成事件（写入 CSV/JSONL，可与 joyue-trigger JSONL 按 tx_id 关联）：
  - TwoPhaseCoordinator：TwoPCCommitted(bytes32)、TwoPCAborted(bytes32)
  - PeerTransferCoordinator2PC：PeerTransfer2PCCommitted(bytes32)、PeerTransfer2PCAborted(bytes32)
  - PeerAmmSwapCoordinator2PC：PeerAmmSwap2PCCommitted(bytes32)、PeerAmmSwap2PCAborted(bytes32)
  - PeerNftPurchaseCoordinator2PC：PeerNftPurchase2PCCommitted(bytes32)、PeerNftPurchase2PCAborted(bytes32)
  - PeerMevArbCoordinator2PC：PeerMevArb2PCCommitted(bytes32)、PeerMevArb2PCAborted(bytes32)

调试（--debug）额外扫「已开始」事件，便于确认链上是否触发 2PC：
  - TwoPCStarted(bytes32,bytes32,uint256,address)
  - PeerTransfer2PCStarted(bytes32,address,address,uint256)
  - PeerAmmSwap2PCStarted(bytes32,address,uint256,uint256)
  - PeerNftPurchase2PCStarted(bytes32,address,uint256,uint256)
  - PeerMevArb2PCStarted(bytes32,address,uint256,uint256)

单分片：

	twopc-metrics --rpc http://127.0.0.1:9500 --coordinator 0x... --output ./2pc-metrics.csv --from-block 0

多分片（每键一个 Coordinator 地址）：

	twopc-metrics --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9502 \
	  --coordinators 0=0x...,1=0x... --output ./2pc-metrics.csv --from-block 0
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

var (
	twoPCStartedSig   = crypto.Keccak256Hash([]byte("TwoPCStarted(bytes32,bytes32,uint256,address)"))
	twoPCCommittedSig = crypto.Keccak256Hash([]byte("TwoPCCommitted(bytes32)"))
	twoPCAbortedSig   = crypto.Keccak256Hash([]byte("TwoPCAborted(bytes32)"))

	peerTransfer2PCStartedSig   = crypto.Keccak256Hash([]byte("PeerTransfer2PCStarted(bytes32,address,address,uint256)"))
	peerTransfer2PCCommittedSig = crypto.Keccak256Hash([]byte("PeerTransfer2PCCommitted(bytes32)"))
	peerTransfer2PCAbortedSig   = crypto.Keccak256Hash([]byte("PeerTransfer2PCAborted(bytes32)"))

	peerAmmSwap2PCStartedSig   = crypto.Keccak256Hash([]byte("PeerAmmSwap2PCStarted(bytes32,address,uint256,uint256)"))
	peerAmmSwap2PCCommittedSig = crypto.Keccak256Hash([]byte("PeerAmmSwap2PCCommitted(bytes32)"))
	peerAmmSwap2PCAbortedSig   = crypto.Keccak256Hash([]byte("PeerAmmSwap2PCAborted(bytes32)"))

	peerNftPurchase2PCStartedSig   = crypto.Keccak256Hash([]byte("PeerNftPurchase2PCStarted(bytes32,address,uint256,uint256)"))
	peerNftPurchase2PCCommittedSig = crypto.Keccak256Hash([]byte("PeerNftPurchase2PCCommitted(bytes32)"))
	peerNftPurchase2PCAbortedSig   = crypto.Keccak256Hash([]byte("PeerNftPurchase2PCAborted(bytes32)"))

	peerMevArb2PCStartedSig   = crypto.Keccak256Hash([]byte("PeerMevArb2PCStarted(bytes32,address,uint256,uint256)"))
	peerMevArb2PCCommittedSig = crypto.Keccak256Hash([]byte("PeerMevArb2PCCommitted(bytes32)"))
	peerMevArb2PCAbortedSig   = crypto.Keccak256Hash([]byte("PeerMevArb2PCAborted(bytes32)"))
)

func main() {
	rpcs := flag.String("rpcs", "", "多分片 RPC：0=http://...,1=http://...")
	coordinators := flag.String("coordinators", "", "多分片：0=0x...,1=0x...")
	rpc := flag.String("rpc", "", "单分片：Coordinator 所在分片 RPC")
	coordinator := flag.String("coordinator", "", "单分片：Coordinator 合约地址")
	shardID := flag.String("shard-id", "0", "单分片：输出中的 shard_id")
	output := flag.String("output", "", "输出文件路径（CSV/JSONL）")
	format := flag.String("format", "csv", "输出格式：csv / jsonl")
	fromBlock := flag.Uint64("from-block", 0, "起始区块，0 表示从当前最高块开始")
	pollInterval := flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
	debug := flag.Bool("debug", false, "打印调试日志（含 Started 类事件）")
	flag.Parse()

	rpcMulti := strings.TrimSpace(*rpcs)
	coordMulti := strings.TrimSpace(*coordinators)
	rpcSingle := strings.TrimSpace(*rpc)
	coordSingle := strings.TrimSpace(*coordinator)

	if rpcMulti == "" && rpcSingle != "" && looksLikeShardRPCList(rpcSingle) {
		log.Printf("[WARN] 多分片 RPC 应使用 --rpcs；已自动从 --rpc 解析")
		rpcMulti, rpcSingle = rpcSingle, ""
	}
	if rpcMulti != "" && coordMulti == "" && coordSingle != "" && strings.Contains(coordSingle, "=") {
		log.Printf("[WARN] 多分片协调者应使用 --coordinators；已自动从 --coordinator 解析")
		coordMulti, coordSingle = coordSingle, ""
	}

	rpcMap := make(map[string]string)
	coordMap := make(map[string]common.Address)

	if rpcMulti != "" {
		if coordMulti == "" {
			flag.Usage()
			log.Fatal("多分片需同时指定 --rpcs 与 --coordinators")
		}
		rpcMap = parseKv(rpcMulti)
		coordMap = parseAddrMap(coordMulti)
		if len(rpcMap) == 0 {
			log.Fatal("--rpcs 解析失败")
		}
	} else {
		if rpcSingle == "" || coordSingle == "" || *output == "" {
			flag.Usage()
			log.Fatal("单分片：--rpc、--coordinator、--output 必填；或多分片用 --rpcs + --coordinators")
		}
		sid := strings.TrimSpace(*shardID)
		if sid == "" {
			sid = "0"
		}
		rpcMap[sid] = rpcSingle
		coordMap[sid] = common.HexToAddress(coordSingle)
	}

	if *output == "" {
		log.Fatal("缺少 --output")
	}
	if *format != "csv" && *format != "jsonl" {
		log.Fatalf("--format 必须为 csv 或 jsonl，当前为 %q", *format)
	}

	log.Printf("[INFO] twopc-metrics 启动: output=%s format=%s from-block=%d shards=%v",
		*output, *format, *fromBlock, keysOf(rpcMap))

	outFile, err := os.Create(*output)
	if err != nil {
		log.Fatalf("创建输出文件失败: %v", err)
	}
	defer outFile.Close()

	var writer *csv.Writer
	if *format == "csv" {
		writer = csv.NewWriter(outFile)
		_ = writer.Write([]string{"tx_hash", "tx_id", "shard_id", "completion_type", "success", "send_time", "completion_time", "latency_ms", "block_number", "created_at"})
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
	debugSeenStarted := make(map[string]bool)
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
			log.Printf("[WARN] 分片 %s 缺少 coordinator，跳过", shardKey)
			continue
		}
		log.Printf("[INFO] 分片 %s: rpc=%s coordinator=%s", shardKey, rpcURL, coordAddr.Hex())
		go poll2PC(ctx, rpcURL, shardKey, coordAddr, &processed, &processedMu, &writeMu, writer, *format, outFile, startFrom, *pollInterval, *debug, &debugSeenStarted)
	}

	<-ctx.Done()
	log.Println("[INFO] twopc-metrics 已停止")
}

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

func poll2PC(ctx context.Context, rpcURL, shardID string, coordAddr common.Address,
	processed *map[string]bool, processedMu *sync.Mutex, writeMu *sync.Mutex, writer *csv.Writer, format string, outFile *os.File,
	fromBlock uint64, pollInterval time.Duration, debug bool, debugSeenStarted *map[string]bool) {
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
				if debug {
					log.Printf("[DEBUG] 分片 %s 等待块高 current=%d from=%d", shardID, currentBlock, fromBlock)
				}
				continue
			}

			if debug && debugSeenStarted != nil {
				qStarted := ethereum.FilterQuery{
					FromBlock: big.NewInt(int64(fromBlock)),
					ToBlock:   big.NewInt(int64(currentBlock)),
					Addresses: []common.Address{coordAddr},
					Topics:    [][]common.Hash{{twoPCStartedSig, peerTransfer2PCStartedSig, peerAmmSwap2PCStartedSig, peerNftPurchase2PCStartedSig, peerMevArb2PCStartedSig}},
				}
				logsSt, errSt := client.FilterLogs(ctx, qStarted)
				if errSt == nil && len(logsSt) > 0 {
					for _, l := range logsSt {
						key := fmt.Sprintf("dbg:%s:%d:%d", l.TxHash.Hex(), l.BlockNumber, l.Index)
						processedMu.Lock()
						seen := (*debugSeenStarted)[key]
						if !seen {
							(*debugSeenStarted)[key] = true
						}
						processedMu.Unlock()
						if seen {
							continue
						}
						txId := ""
						if len(l.Topics) >= 2 {
							txId = l.Topics[1].Hex()
						}
						name := "TwoPCStarted"
						if l.Topics[0] == peerTransfer2PCStartedSig {
							name = "PeerTransfer2PCStarted"
						}
						if l.Topics[0] == peerAmmSwap2PCStartedSig {
							name = "PeerAmmSwap2PCStarted"
						}
						if l.Topics[0] == peerNftPurchase2PCStartedSig {
							name = "PeerNftPurchase2PCStarted"
						}
						if l.Topics[0] == peerMevArb2PCStartedSig {
							name = "PeerMevArb2PCStarted"
						}
						log.Printf("[DEBUG] 分片 %s 发现 %s: txId=%s block=%d (若长期无 Committed/Aborted 说明 2PC 未完成)", shardID, name, txId, l.BlockNumber)
					}
				}
			}

			qFin := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(currentBlock)),
				Addresses: []common.Address{coordAddr},
				Topics: [][]common.Hash{{
					twoPCCommittedSig,
					peerTransfer2PCCommittedSig,
					peerAmmSwap2PCCommittedSig,
					peerNftPurchase2PCCommittedSig,
					peerMevArb2PCCommittedSig,
					peerAmmSwap2PCStartedSig,
					peerNftPurchase2PCStartedSig,
					peerMevArb2PCStartedSig,
					twoPCAbortedSig,
					peerTransfer2PCAbortedSig,
					peerAmmSwap2PCAbortedSig,
					peerNftPurchase2PCAbortedSig,
					peerMevArb2PCAbortedSig,
				}},
			}
			logsFin, err := client.FilterLogs(ctx, qFin)
			if err != nil {
				log.Printf("[WARN] 分片 %s FilterLogs 错误 (区块 %d-%d): %v", shardID, fromBlock, currentBlock, err)
				continue
			}

			for _, l := range logsFin {
				if len(l.Topics) < 2 {
					continue
				}
				txID := l.Topics[1]
				if txID == (common.Hash{}) {
					continue
				}
				txIdHex := txID.Hex()

				if l.Topics[0] == peerAmmSwap2PCStartedSig || l.Topics[0] == peerNftPurchase2PCStartedSig || l.Topics[0] == peerMevArb2PCStartedSig {
					continue
				}

				var committed bool
				var ct uint8
				switch l.Topics[0] {
				case twoPCCommittedSig, peerTransfer2PCCommittedSig, peerAmmSwap2PCCommittedSig, peerNftPurchase2PCCommittedSig, peerMevArb2PCCommittedSig:
					committed = true
					ct = 1
				case twoPCAbortedSig, peerTransfer2PCAbortedSig, peerAmmSwap2PCAbortedSig, peerNftPurchase2PCAbortedSig, peerMevArb2PCAbortedSig:
					committed = false
					ct = 2
				default:
					continue
				}

				key := fmt.Sprintf("%s:%s:%d:%s", shardID, txIdHex, l.Index, l.Topics[0].Hex())
				processedMu.Lock()
				if (*processed)[key] {
					processedMu.Unlock()
					continue
				}
				(*processed)[key] = true
				processedMu.Unlock()

				block, _ := client.BlockByNumber(ctx, big.NewInt(int64(l.BlockNumber)))
				completionTime := int64(0)
				if block != nil && block.Time() > 0 {
					completionTime = int64(block.Time()) * 1000
				}
				completionTimeStr := ""
				if completionTime > 0 {
					completionTimeStr = strconv.FormatInt(completionTime, 10)
				}

				writeMu.Lock()
				writeRecord(writer, format, outFile, l.TxHash.Hex(), txIdHex, shardID, ct, committed, "", completionTimeStr, "", l.BlockNumber)
				writeMu.Unlock()

				evName := "TwoPCCommitted"
				if !committed {
					evName = "TwoPCAborted"
				}
				if l.Topics[0] == peerTransfer2PCCommittedSig || l.Topics[0] == peerTransfer2PCAbortedSig {
					if committed {
						evName = "PeerTransfer2PCCommitted"
					} else {
						evName = "PeerTransfer2PCAborted"
					}
				}
				if l.Topics[0] == peerAmmSwap2PCCommittedSig || l.Topics[0] == peerAmmSwap2PCAbortedSig {
					if committed {
						evName = "PeerAmmSwap2PCCommitted"
					} else {
						evName = "PeerAmmSwap2PCAborted"
					}
				}
				if l.Topics[0] == peerNftPurchase2PCCommittedSig || l.Topics[0] == peerNftPurchase2PCAbortedSig {
					if committed {
						evName = "PeerNftPurchase2PCCommitted"
					} else {
						evName = "PeerNftPurchase2PCAborted"
					}
				}
				if l.Topics[0] == peerMevArb2PCCommittedSig || l.Topics[0] == peerMevArb2PCAbortedSig {
					if committed {
						evName = "PeerMevArb2PCCommitted"
					} else {
						evName = "PeerMevArb2PCAborted"
					}
				}
				log.Printf("[INFO] 分片 %s 写入 %s: txId=%s block=%d", shardID, evName, txIdHex, l.BlockNumber)
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
			_ = writer.Write([]string{txHash, txId, shardID, strconv.Itoa(int(completionType)), successStr, sendTimeStr, completionTimeStr, latencyStr, strconv.FormatUint(blockNum, 10), strconv.FormatInt(createdAt, 10)})
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
			_ = json.NewEncoder(outFile).Encode(rec)
		}
	default:
		log.Printf("[WARN] 未知 format=%q，跳过写入", format)
	}
}
