/*
JOYUE Metrics - 指标采集工具

功能：
- 订阅链上事件：AgentResultEmitted（Master 发出）、IntentRejected（Agent 发出）
- 输出 completion_type、success、block 时间等，不依赖 Trigger 文件

使用方式：
  joyue-metrics --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9502 \
    --master 0x61a049be2326C44637b6d6AfdF92480f67DCf076 \
    --agent 0=0x61a049be2326C44637b6d6AfdF92480f67DCf076,1=0x61a049be2326C44637b6d6AfdF92480f67DCf076 \
    --output ./metrics.csv --from-block 0
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
	agentResultEmittedSig = crypto.Keccak256Hash([]byte("AgentResultEmitted(bytes32,bool,uint8)"))
	intentRejectedSig     = crypto.Keccak256Hash([]byte("IntentRejected(bytes32,uint8)"))
)

func main() {
	rpcs := flag.String("rpcs", "", "分片 RPC，格式 0=http://...,1=http://...")
	master := flag.String("master", "", "Master 合约地址（FruitShopMasterV2 等，继承 JoyueCoordinator，发出 AgentResultEmitted），格式 0=0x... 或 0x...")
	agent := flag.String("agent", "", "Agent 合约地址（FruitShopAgentV2 等，发出 IntentRejected），格式 0=0x...,1=0x... 或 0x...")
	output := flag.String("output", "", "输出文件路径（CSV）")
	format := flag.String("format", "csv", "输出格式：csv / jsonl")
	fromBlock := flag.Uint64("from-block", 0, "起始区块（所有分片统一），0 表示从最新开始")
	pollInterval := flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
	debug := flag.Bool("debug", false, "打印调试日志（每轮轮询的区块范围、事件数量）")
	scanAll := flag.Bool("scan-all", false, "诊断模式：不按地址过滤，扫描所有 AgentResultEmitted 事件并打印 log.Address，用于确认 Master 实际地址")
	flag.Parse()

	if *rpcs == "" {
		flag.Usage()
		log.Fatal("缺少必填参数：--rpcs")
	}
	if !*scanAll {
		if *output == "" {
			flag.Usage()
			log.Fatal("缺少必填参数：--output")
		}
		if *master == "" || *agent == "" {
			flag.Usage()
			log.Fatal("缺少必填参数：--master, --agent")
		}
	}

	rpcMap := parseKv(*rpcs)
	masterMap := parseAddrMap(*master)
	agentMap := parseAddrMap(*agent)

	if len(rpcMap) == 0 {
		log.Fatal("--rpcs 解析失败")
	}
	if *format != "csv" && *format != "jsonl" {
		log.Fatalf("--format 必须为 csv 或 jsonl，当前为 %q", *format)
	}

	if *scanAll {
		runDiagnose(*fromBlock, rpcMap)
		return
	}

	log.Printf("[INFO] joyue-metrics 启动: master=%s, agent=%s, output=%s, format=%s, from-block=%d",
		*master, *agent, *output, *format, *fromBlock)

	// 打开输出文件
	outFile, err := os.Create(*output)
	if err != nil {
		log.Fatalf("创建输出文件失败: %v", err)
	}
	defer outFile.Close()
	log.Printf("[INFO] 输出文件已创建: %s", *output)

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

	// 已处理的完成事件，避免重复
	processed := make(map[string]bool)
	var processedMu sync.Mutex

	// 输出写入锁（多分片 goroutine 并发写）
	var writeMu sync.Mutex

	startFromBlock := *fromBlock

	for shardIDStr, rpcURL := range rpcMap {
		shardID := shardIDStr
		masterAddr := masterMap[shardID]
		if masterAddr == (common.Address{}) {
			masterAddr = masterMap["0"]
		}
		agentAddr := agentMap[shardID]
		if agentAddr == (common.Address{}) {
			agentAddr = agentMap["0"]
		}
		if masterAddr == (common.Address{}) || agentAddr == (common.Address{}) {
			log.Printf("[WARN] 分片 %s 缺少 master 或 agent 地址，跳过", shardID)
			continue
		}

		log.Printf("[INFO] 分片 %s 启动轮询: rpc=%s, master=%s, agent=%s", shardID, rpcURL, masterAddr.Hex(), agentAddr.Hex())
		go func(sid, url string, mst, ag common.Address) {
			pollShard(ctx, url, sid, mst, ag, &processed, &processedMu, &writeMu, writer, *format, outFile, startFromBlock, *pollInterval, *debug)
		}(shardID, rpcURL, masterAddr, agentAddr)
	}

	// 等待
	<-ctx.Done()
	log.Println("[INFO] joyue-metrics 已停止")
}

func runDiagnose(fromBlock uint64, rpcMap map[string]string) {
	ctx := context.Background()
	agentResultEmittedSig := crypto.Keccak256Hash([]byte("AgentResultEmitted(bytes32,bool,uint8)"))
	log.Printf("[DIAGNOSE] 扫描所有分片的 AgentResultEmitted 事件（不按地址过滤）")
	for shardID, rpcURL := range rpcMap {
		client, err := ethclient.DialContext(ctx, rpcURL)
		if err != nil {
			log.Printf("[DIAGNOSE] 分片 %s 连接失败: %v", shardID, err)
			continue
		}
		toBlock, err := client.BlockNumber(ctx)
		if err != nil {
			log.Printf("[DIAGNOSE] 分片 %s 获取区块失败: %v", shardID, err)
			client.Close()
			continue
		}
		start := fromBlock
		if start == 0 {
			// 0 表示只扫最近 10 个区块
			if toBlock > 10 {
				start = toBlock - 10
			} else {
				start = 1
			}
		}
		query := ethereum.FilterQuery{
			FromBlock: big.NewInt(int64(start)),
			ToBlock:   big.NewInt(int64(toBlock)),
			Topics:    [][]common.Hash{{agentResultEmittedSig}},
		}
		logs, err := client.FilterLogs(ctx, query)
		client.Close()
		if err != nil {
			log.Printf("[DIAGNOSE] 分片 %s FilterLogs 失败: %v", shardID, err)
			continue
		}
		log.Printf("[DIAGNOSE] 分片 %s 区块 %d-%d: 找到 %d 条 AgentResultEmitted", shardID, start, toBlock, len(logs))
		for i, l := range logs {
			txId := ""
			if len(l.Topics) >= 2 {
				txId = l.Topics[1].Hex()
			}
			success, ct := parseAgentResultEmittedData(l.Data)
			log.Printf("[DIAGNOSE]   [%d] address=%s block=%d txId=%s success=%v completionType=%d",
				i+1, l.Address.Hex(), l.BlockNumber, txId, success, ct)
		}
	}
	log.Printf("[DIAGNOSE] 完成。若找到事件，请用 address 列的值作为 --master 参数")
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
		addr := common.HexToAddress(s)
		m["0"] = addr
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

func pollShard(ctx context.Context, rpcURL, shardID string, masterAddr, agentAddr common.Address,
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

			// 1. AgentResultEmitted（Master 合约发出）
			queryMaster := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(currentBlock)),
				Addresses: []common.Address{masterAddr},
				Topics:    [][]common.Hash{{agentResultEmittedSig}},
			}
			logsCoord, err := client.FilterLogs(ctx, queryMaster)
			if err != nil {
				log.Printf("[WARN] 分片 %s AgentResultEmitted FilterLogs 错误 (区块 %d-%d): %v", shardID, fromBlock, currentBlock, err)
				continue
			}
			if len(logsCoord) > 0 {
				log.Printf("[INFO] 分片 %s 区块 %d-%d: AgentResultEmitted %d 条", shardID, fromBlock, currentBlock, len(logsCoord))
			}
			for _, l := range logsCoord {
				txHashTopic := common.Hash{}
				if len(l.Topics) >= 2 {
					txHashTopic = l.Topics[1]
				}
				txId := txHashTopic.Hex()
				success, completionType := parseAgentResultEmittedData(l.Data)
				key := fmt.Sprintf("%s:%s:%d", shardID, txId, l.Index)
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
				writeRecord(writer, format, outFile, txId, txId, shardID, completionType, success, "", completionTimeStr, "", l.BlockNumber)
				writeMu.Unlock()
				log.Printf("[INFO] 分片 %s 写入 AgentResultEmitted: txId=%s type=%d success=%v block=%d", shardID, txId, completionType, success, l.BlockNumber)
			}

			// 2. IntentRejected（Agent，类型 6）
			queryAgent := ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(fromBlock)),
				ToBlock:   big.NewInt(int64(currentBlock)),
				Addresses: []common.Address{agentAddr},
				Topics:    [][]common.Hash{{intentRejectedSig}},
			}
			logsAgent, err := client.FilterLogs(ctx, queryAgent)
			if err != nil {
				log.Printf("[WARN] 分片 %s IntentRejected FilterLogs 错误 (区块 %d-%d): %v", shardID, fromBlock, currentBlock, err)
				continue
			}
			if len(logsAgent) > 0 {
				log.Printf("[INFO] 分片 %s 区块 %d-%d: IntentRejected %d 条", shardID, fromBlock, currentBlock, len(logsAgent))
			}
			for _, l := range logsAgent {
				txIdTopic := common.Hash{}
				if len(l.Topics) >= 2 {
					txIdTopic = l.Topics[1]
				}
				txId := txIdTopic.Hex()
				key := fmt.Sprintf("%s:reject:%s:%d", shardID, txId, l.Index)
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
				writeRecord(writer, format, outFile, txId, txId, shardID, 6, false, "", completionTimeStr, "", l.BlockNumber)
				writeMu.Unlock()
				log.Printf("[INFO] 分片 %s 写入 IntentRejected: txId=%s block=%d", shardID, txId, l.BlockNumber)
			}

			fromBlock = currentBlock + 1
			if debug || ((fromBlock-1)%50 == 0 && fromBlock > 1) {
				log.Printf("[INFO] 分片 %s 已扫描至区块 %d", shardID, fromBlock-1)
			}
		}
	}
}

func parseAgentResultEmittedData(data []byte) (bool, uint8) {
	if len(data) < 64 {
		return false, 0
	}
	success := data[31] != 0
	completionType := data[63]
	return success, completionType
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
