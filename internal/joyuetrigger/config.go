package joyuetrigger

import (
	"fmt"
	"log"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// TargetSpec 分片内多合约目标（仅 joyue-trigger-v2 使用）。
type TargetSpec struct {
	To   string   `yaml:"to"` // 可选；空则与 v1 相同：优先 coordinator，否则 agent
	Sig  string   `yaml:"sig"`
	Args []string `yaml:"args"`
}

// ShardConfig 单分片 RPC 与合约地址；可选 targets 展开为多笔任务。
type ShardConfig struct {
	RPC         string       `yaml:"rpc"`
	Agent       string       `yaml:"agent"`
	Coordinator string       `yaml:"coordinator"`
	Sig         string       `yaml:"sig"`
	Args        []string     `yaml:"args"`
	Targets     []TargetSpec `yaml:"targets"`
}

// CallSpec 全局多调用模式（与 v1 一致）。
type CallSpec struct {
	Sig  string   `yaml:"sig"`
	Args []string `yaml:"args"`
}

// Config 与 cmd/joyue-trigger 顶层 YAML 一致，并可选分片级 targets。
type Config struct {
	Shards     map[string]ShardConfig `yaml:"shards"`
	Sig        string                 `yaml:"sig"`
	Args       []string               `yaml:"args"`
	Calls      []CallSpec             `yaml:"calls"`
	Repeat     uint                   `yaml:"repeat"`
	IntervalMs uint                   `yaml:"interval_ms"`
	Gas        uint64                 `yaml:"gas"`
	GasTip     int64                  `yaml:"gas_tip_gwei"`
}

// Job 展开后的一笔发送任务。
type Job struct {
	ShardID  string
	RPC      string
	To       common.Address
	Calldata []byte
	Is2PC    bool
	Label    string // 日志用
}

func defaultToStr(sc ShardConfig) string {
	if sc.Coordinator != "" {
		return sc.Coordinator
	}
	return sc.Agent
}

// sameAddr 比较两个 hex 地址（忽略大小写、0x）。
func sameAddr(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(common.HexToAddress(a).Hex(), common.HexToAddress(b).Hex())
}

// ExpandToJobs 将配置展开为并行任务列表，语义与 joyue-trigger v1 一致；若分片含 targets 则仅由 targets 产生该分片的任务。
func ExpandToJobs(cfg *Config) ([]Job, error) {
	if len(cfg.Shards) == 0 {
		return nil, fmt.Errorf("配置中 shards 为空")
	}
	useCalls := len(cfg.Calls) > 0
	hasGlobalSig := cfg.Sig != ""
	hasPerShardSig := false
	hasAnyTargets := false
	for _, sc := range cfg.Shards {
		if sc.Sig != "" {
			hasPerShardSig = true
		}
		if len(sc.Targets) > 0 {
			hasAnyTargets = true
		}
	}
	if !useCalls && !hasGlobalSig && !hasPerShardSig && !hasAnyTargets {
		return nil, fmt.Errorf("配置中需指定 sig、calls、各分片 sig 或 targets")
	}

	var jobs []Job
	for shardID, sc := range cfg.Shards {
		if sc.RPC == "" {
			log.Printf("[WARN] 跳过分片 %s：rpc 为空", shardID)
			continue
		}

		if len(sc.Targets) > 0 {
			defTo := defaultToStr(sc)
			for i, t := range sc.Targets {
				if t.Sig == "" {
					return nil, fmt.Errorf("分片 %s targets[%d] 缺少 sig", shardID, i)
				}
				toStr := strings.TrimSpace(t.To)
				if toStr == "" {
					if defTo == "" {
						return nil, fmt.Errorf("分片 %s targets[%d] 需指定 to（分片无默认 agent/coordinator）", shardID, i)
					}
					toStr = defTo
				}
				is2PC := sc.Coordinator != "" && sameAddr(toStr, sc.Coordinator)
				cd, err := EncodeCalldata(t.Sig, t.Args)
				if err != nil {
					return nil, fmt.Errorf("分片 %s targets[%d] 编码失败: %w", shardID, i, err)
				}
				jobs = append(jobs, Job{
					ShardID:  shardID,
					RPC:      sc.RPC,
					To:       common.HexToAddress(toStr),
					Calldata: cd,
					Is2PC:    is2PC,
					Label:    t.Sig,
				})
			}
			continue
		}

		target := sc.Coordinator
		is2PC := target != ""
		if target == "" {
			target = sc.Agent
		}
		if target == "" {
			log.Printf("[WARN] 跳过分片 %s：agent/coordinator 为空", shardID)
			continue
		}

		sig := sc.Sig
		args := sc.Args
		if sig == "" {
			sig = cfg.Sig
			args = cfg.Args
		}

		if useCalls && sig == "" {
			for i, c := range cfg.Calls {
				cd, err := EncodeCalldata(c.Sig, c.Args)
				if err != nil {
					return nil, fmt.Errorf("calls[%d] 编码失败: %w", i, err)
				}
				jobs = append(jobs, Job{
					ShardID:  shardID,
					RPC:      sc.RPC,
					To:       common.HexToAddress(target),
					Calldata: cd,
					Is2PC:    is2PC,
					Label:    c.Sig,
				})
			}
		} else if sig != "" {
			cd, err := EncodeCalldata(sig, args)
			if err != nil {
				return nil, fmt.Errorf("分片 %s 编码失败: %w", shardID, err)
			}
			jobs = append(jobs, Job{
				ShardID:  shardID,
				RPC:      sc.RPC,
				To:       common.HexToAddress(target),
				Calldata: cd,
				Is2PC:    is2PC,
				Label:    sig,
			})
		} else {
			log.Printf("[WARN] 跳过分片 %s：无 sig 或 calls", shardID)
		}
	}

	return jobs, nil
}
