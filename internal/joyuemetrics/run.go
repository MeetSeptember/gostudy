package joyuemetrics

import (
	"context"
	"encoding/csv"
	"flag"
	"log"
	"math/big"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Options 命令行入口。
type Options struct {
	ProgramName string
	// MultiAddrPerShard 为 true（joyue-metrics-v2）时，--master/--agent 每分片可用分号分隔多个地址。
	MultiAddrPerShard bool
}

// Main 解析 flag 并运行（与历史 joyue-metrics 参数兼容）。
func Main(opt Options) {
	if opt.ProgramName == "" {
		opt.ProgramName = "joyue-metrics"
	}

	rpcs := flag.String("rpcs", "", "分片 RPC，格式 0=http://...,1=http://...")
	master := flag.String("master", "", "Master 地址（AgentResultEmitted）；v2 同一分片多地址用分号：0=0xA;0xB,1=0xC")
	agent := flag.String("agent", "", "Agent 地址（IntentRejected）；v2 格式同 --master")
	output := flag.String("output", "", "输出文件路径（CSV）")
	format := flag.String("format", "csv", "输出格式：csv / jsonl")
	fromBlock := flag.Uint64("from-block", 0, "起始区块（所有分片统一），0 表示从最新开始")
	pollInterval := flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
	debug := flag.Bool("debug", false, "打印调试日志（每轮轮询的区块范围、事件数量）")
	scanAll := flag.Bool("scan-all", false, "诊断模式：不按地址过滤，扫描所有 AgentResultEmitted 事件并打印 log.Address")
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

	rpcMap := ParseKv(*rpcs)
	if len(rpcMap) == 0 {
		log.Fatal("--rpcs 解析失败")
	}
	if *format != "csv" && *format != "jsonl" {
		log.Fatalf("--format 必须为 csv 或 jsonl，当前为 %q", *format)
	}

	if *scanAll {
		RunDiagnose(*fromBlock, rpcMap)
		return
	}

	var masterMulti map[string][]common.Address
	var agentMulti map[string][]common.Address
	if opt.MultiAddrPerShard {
		masterMulti = ParseMultiAddrMap(*master)
		agentMulti = ParseMultiAddrMap(*agent)
	} else {
		masterMulti = SingleAddrMapToMulti(ParseAddrMap(*master))
		agentMulti = SingleAddrMapToMulti(ParseAddrMap(*agent))
	}

	log.Printf("[INFO] %s 启动: output=%s, format=%s, from-block=%d, multi-addr=%v",
		opt.ProgramName, *output, *format, *fromBlock, opt.MultiAddrPerShard)

	outFile, err := os.Create(*output)
	if err != nil {
		log.Fatalf("创建输出文件失败: %v", err)
	}
	defer outFile.Close()
	log.Printf("[INFO] 输出文件已创建: %s", *output)

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
	var processedMu sync.Mutex
	var writeMu sync.Mutex
	startFromBlock := *fromBlock

	for shardIDStr, rpcURL := range rpcMap {
		shardID := shardIDStr
		masters := resolveAddrsForShard(shardID, masterMulti)
		agents := resolveAddrsForShard(shardID, agentMulti)
		if len(masters) == 0 || len(agents) == 0 {
			log.Printf("[WARN] 分片 %s 缺少 master 或 agent 地址，跳过", shardID)
			continue
		}

		log.Printf("[INFO] 分片 %s 启动轮询: rpc=%s, masters=%d, agents=%d", shardID, rpcURL, len(masters), len(agents))
		go func(sid, url string, mst, ag []common.Address) {
			PollShard(ctx, url, sid, mst, ag, &processed, &processedMu, &writeMu, writer, *format, outFile, startFromBlock, *pollInterval, *debug)
		}(shardID, rpcURL, masters, agents)
	}

	<-ctx.Done()
	log.Printf("[INFO] %s 已停止", opt.ProgramName)
}

func resolveAddrsForShard(shardID string, multi map[string][]common.Address) []common.Address {
	if addrs, ok := multi[shardID]; ok && len(addrs) > 0 {
		return addrs
	}
	if addrs, ok := multi["0"]; ok && len(addrs) > 0 {
		return addrs
	}
	return nil
}

// RunDiagnose 不按合约地址过滤，扫描 AgentResultEmitted。
func RunDiagnose(fromBlock uint64, rpcMap map[string]string) {
	ctx := context.Background()
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
