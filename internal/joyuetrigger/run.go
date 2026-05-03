package joyuetrigger

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"gopkg.in/yaml.v2"
)

// Options 命令行入口选项。
type Options struct {
	// RejectTargets 为 true 时（joyue-trigger）：若 YAML 含分片级 targets 则退出并提示使用 v2。
	RejectTargets bool
	// ProgramName 用于日志，如 "joyue-trigger" / "joyue-trigger-v2"。
	ProgramName string
}

// Main 解析 --config / --private-key / --metrics-output 并运行；与历史 joyue-trigger 行为一致。
func Main(opt Options) {
	if opt.ProgramName == "" {
		opt.ProgramName = "joyue-trigger"
	}

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

	if opt.RejectTargets {
		for sid, sc := range cfg.Shards {
			if len(sc.Targets) > 0 {
				log.Fatalf("分片 %s 配置了 targets（多合约并行）；请使用 joyue-trigger-v2 运行此配置", sid)
			}
		}
	}

	if len(cfg.Shards) == 0 {
		log.Fatal("配置中 shards 为空")
	}

	jobs, err := ExpandToJobs(&cfg)
	if err != nil {
		log.Fatal(err)
	}
	if len(jobs) == 0 {
		log.Printf("[WARN] 未展开任何发送任务（各分片可能缺少 sig/calls）")
		return
	}
	log.Printf("[INFO] %s：已展开 %d 个并行发送任务", opt.ProgramName, len(jobs))

	if cfg.Repeat == 0 {
		log.Printf("[INFO] repeat=0，持续模式，按 Ctrl+C 停止")
	}
	if cfg.IntervalMs > 0 {
		log.Printf("[INFO] interval_ms=%d，限速模式（约 %.1f TPS/任务）", cfg.IntervalMs, 1000.0/float64(cfg.IntervalMs))
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
			log.Printf("[INFO] 收到停止信号，停止所有任务")
			cancel()
		}()
	}

	var wg sync.WaitGroup
	for _, job := range jobs {
		j := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("[INFO] [shard %s] 启动 call: %s (2PC=%v)", j.ShardID, j.Label, j.Is2PC)
			runShard(ctx, j.ShardID, j.RPC, j.To, privKey, j.Calldata, cfg.Gas, cfg.GasTip, cfg.Repeat, cfg.IntervalMs, pendingCh, j.Is2PC)
		}()
	}

	wg.Wait()
	log.Printf("[INFO] 全部完成")
}
