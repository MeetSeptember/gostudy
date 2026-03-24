/*
2PC Metrics - 2PC 跨分片事务指标采集工具

功能：
- 轮询链上事件：TwoPCCommitted、TwoPCAborted（Coordinator 发出）
- 输出 completion_type、success、block 时间等，可与 trigger 的 JSONL 离线关联

Coordinator 仅存在于某一分片，故只需轮询该分片。

使用方式：
  twopc-metrics --rpc http://127.0.0.1:9500 --coordinator 0x... --output ./2pc-metrics.csv --from-block 0
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
)

func main() {
	rpc := flag.String("rpc", "", "Coordinator 所在分片的 RPC URL")
	coordinator := flag.String("coordinator", "", "Coordinator 合约地址")
	shardID := flag.String("shard-id", "0", "分片 ID（用于输出）")
	output := flag.String("output", "", "输出文件路径（CSV/JSONL）")
	format := flag.String("format", "csv", "输出格式：csv / jsonl")
	fromBlock := flag.Uint64("from-block", 0, "起始区块，0 表示从最新开始")
	pollInterval := flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
	debug := flag.Bool("debug", false, "打印调试日志")
	flag.Parse()

	if *rpc == "" || *coordinator == "" || *output == "" {
		flag.Usage()
		log.Fatal("缺少必填参数：--rpc, --coordinator, --output")
	}
	if *format != "csv" && *format != "jsonl" {
		log.Fatalf("--format 必须为 csv 或 jsonl，当前为 %q", *format)
	}

	coordAddr := common.HexToAddress(*coordinator)
	log.Printf("[INFO] 2PC metrics 启动: rpc=%s, coordinator=%s, output=%s, format=%s, from-block=%d",
		*rpc, *coordinator, *output, *format, *fromBlock)

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
	debugSeenStarted := make(map[string]bool) // 诊断用：已打印的 TwoPCStarted
	var processedMu sync.Mutex
	var writeMu sync.Mutex

	go poll2PC(ctx, *rpc, *shardID, coordAddr, &processed, &processedMu, &writeMu, writer, *format, outFile, *fromBlock, *pollInterval, *debug, &debugSeenStarted)

	<-ctx.Done()
	log.Println("[INFO] 2PC metrics 已停止")
}

func poll2PC(ctx context.Context, rpcURL, shardID string, coordAddr common.Address,
	processed *map[string]bool, processedMu *sync.Mutex, writeMu *sync.Mutex, writer *csv.Writer, format string, outFile *os.File,
	fromBlock uint64, pollInterval time.Duration, debug bool, debugSeenStarted *map[string]bool) {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		log.Printf("[ERROR] 连接 RPC 失败: %v", err)
		return
	}
	defer client.Close()

	if fromBlock == 0 {
		bn, err := client.BlockNumber(ctx)
		if err != nil {
			log.Printf("[ERROR] 获取 block number 失败: %v", err)
			return
		}
		fromBlock = bn
		log.Printf("[INFO] 从区块 %d 开始", fromBlock)
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
				continue
			}

			// 诊断：TwoPCStarted 是否有（用于确认 2PC 是否被触发）
			if debug && debugSeenStarted != nil {
				queryStarted := ethereum.FilterQuery{
					FromBlock: big.NewInt(int64(fromBlock)),
					ToBlock:   big.NewInt(int64(currentBlock)),
					Addresses: []common.Address{coordAddr},
					Topics:    [][]common.Hash{{twoPCStartedSig}},
				}
				logsStarted, err := client.FilterLogs(ctx, queryStarted)
				if err == nil && len(logsStarted) > 0 {
					for _, l := range logsStarted {
						key := fmt.Sprintf("%s:%d:%d", l.TxHash.Hex(), l.BlockNumber, l.Index)
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
						log.Printf("[DEBUG] 发现 TwoPCStarted: txId=%s block=%d (若长期无 TwoPCCommitted/Aborted，说明 2PC 未完成)", txId, l.BlockNumber)
					}
				}
			}

			// TwoPCCommitted(bytes32 indexed txId)
			queryCommitted := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(currentBlock)),
				Addresses: []common.Address{coordAddr},
				Topics:    [][]common.Hash{{twoPCCommittedSig}},
			}
			logsCommitted, err := client.FilterLogs(ctx, queryCommitted)
			if err != nil {
				log.Printf("[WARN] TwoPCCommitted FilterLogs 错误 (区块 %d-%d): %v", fromBlock, currentBlock, err)
				continue
			}
			for _, l := range logsCommitted {
				txId := ""
				if len(l.Topics) >= 2 {
					txId = l.Topics[1].Hex()
				}
				key := fmt.Sprintf("commit:%s:%d", txId, l.Index)
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
				writeRecord(writer, format, outFile, txId, txId, shardID, 1, true, "", completionTimeStr, "", l.BlockNumber)
				writeMu.Unlock()
				log.Printf("[INFO] 写入 TwoPCCommitted: txId=%s block=%d", txId, l.BlockNumber)
			}

			// TwoPCAborted(bytes32 indexed txId)
			queryAborted := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(currentBlock)),
				Addresses: []common.Address{coordAddr},
				Topics:    [][]common.Hash{{twoPCAbortedSig}},
			}
			logsAborted, err := client.FilterLogs(ctx, queryAborted)
			if err != nil {
				log.Printf("[WARN] TwoPCAborted FilterLogs 错误 (区块 %d-%d): %v", fromBlock, currentBlock, err)
				continue
			}
			for _, l := range logsAborted {
				txId := ""
				if len(l.Topics) >= 2 {
					txId = l.Topics[1].Hex()
				}
				key := fmt.Sprintf("abort:%s:%d", txId, l.Index)
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
				writeRecord(writer, format, outFile, txId, txId, shardID, 2, false, "", completionTimeStr, "", l.BlockNumber)
				writeMu.Unlock()
				log.Printf("[INFO] 写入 TwoPCAborted: txId=%s block=%d", txId, l.BlockNumber)
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
			enc.Encode(rec)
		}
	default:
		log.Printf("[WARN] 未知 format=%q，跳过写入", format)
	}
}
