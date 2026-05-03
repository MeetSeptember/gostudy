package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// joyueSendTarget 描述一条「RPC + Agent 合约」；多片时每行按策略择一发送。
type joyueSendTarget struct {
	RPC   string
	Agent common.Address
	Label string // 写入 metrics JSONL 的 shard_id
}

func splitCSVNonEmpty(s string) []string {
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

// joyueMultishardScenario 支持单 -rpc/-to 或多 -joyue-rpcs/-joyue-agents 等长列表（多分片各部署一份 Agent）。
func joyueMultishardScenario(name string) bool {
	return name == "wallet-joyue" || name == "wallet-amm-joyue" || name == "wallet-nft-joyue" || name == "wallet-mevarb-joyue"
}

func buildSendTargets(sc scenario, rpcSingle, toHex, joyueRPCs, joyueAgents, shardIDFallback string) ([]joyueSendTarget, error) {
	rpcSingle = strings.TrimSpace(rpcSingle)
	toHex = strings.TrimSpace(toHex)
	shardIDFallback = strings.TrimSpace(shardIDFallback)
	if !joyueMultishardScenario(sc.name) {
		if toHex == "" || !common.IsHexAddress(toHex) {
			return nil, fmt.Errorf("需要有效 -to")
		}
		if rpcSingle == "" {
			return nil, fmt.Errorf("-rpc 不能为空")
		}
		lbl := shardIDFallback
		if lbl == "" {
			lbl = "0"
		}
		return []joyueSendTarget{{RPC: rpcSingle, Agent: common.HexToAddress(toHex), Label: lbl}}, nil
	}
	rpcList := splitCSVNonEmpty(joyueRPCs)
	agentList := splitCSVNonEmpty(joyueAgents)
	if len(rpcList) > 0 && len(agentList) > 0 {
		if len(rpcList) != len(agentList) {
			return nil, fmt.Errorf("-joyue-rpcs 与 -joyue-agents 条目数须一致（当前 %d vs %d）", len(rpcList), len(agentList))
		}
		out := make([]joyueSendTarget, 0, len(rpcList))
		for i := range rpcList {
			a := strings.TrimSpace(agentList[i])
			if !common.IsHexAddress(a) {
				return nil, fmt.Errorf("-joyue-agents[%d] 无效地址: %q", i, a)
			}
			r := strings.TrimSpace(rpcList[i])
			if r == "" {
				return nil, fmt.Errorf("-joyue-rpcs[%d] 为空", i)
			}
			lbl := fmt.Sprintf("%d", i)
			if len(rpcList) == 1 && shardIDFallback != "" {
				lbl = shardIDFallback
			}
			out = append(out, joyueSendTarget{RPC: r, Agent: common.HexToAddress(a), Label: lbl})
		}
		return out, nil
	}
	if toHex == "" || !common.IsHexAddress(toHex) {
		return nil, fmt.Errorf("%s：请配置 -rpc 与 -to（单 Agent），或同时配置 -joyue-rpcs 与 -joyue-agents（多片等长列表）", sc.name)
	}
	if rpcSingle == "" {
		return nil, fmt.Errorf("%s：-rpc 不能为空", sc.name)
	}
	lbl := shardIDFallback
	if lbl == "" {
		lbl = "0"
	}
	return []joyueSendTarget{{RPC: rpcSingle, Agent: common.HexToAddress(toHex), Label: lbl}}, nil
}

type rpcClientPool struct {
	mu sync.Mutex
	m  map[string]*ethclient.Client
}

func newRPCClientPool() *rpcClientPool {
	return &rpcClientPool{m: make(map[string]*ethclient.Client)}
}

func (p *rpcClientPool) Get(ctx context.Context, rpcURL string) (*ethclient.Client, error) {
	url := strings.TrimSpace(rpcURL)
	if url == "" {
		return nil, fmt.Errorf("empty RPC URL")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		return nil, fmt.Errorf("rpc pool closed")
	}
	if c := p.m[url]; c != nil {
		return c, nil
	}
	c, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}
	p.m[url] = c
	return c, nil
}

func (p *rpcClientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.m {
		if c != nil {
			c.Close()
		}
	}
	p.m = nil
}

func chainIDFor(ctx context.Context, cache map[string]*big.Int, rpc string, client *ethclient.Client) (*big.Int, error) {
	rpc = strings.TrimSpace(rpc)
	if id, ok := cache[rpc]; ok && id != nil {
		return id, nil
	}
	id, err := client.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	cache[rpc] = id
	return id, nil
}

func joyuePickRng(mode string, seed int64) *rand.Rand {
	if strings.EqualFold(strings.TrimSpace(mode), "random") {
		if seed != 0 {
			return rand.New(rand.NewSource(seed))
		}
		return rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return nil
}

func pickJoyueTarget(targets []joyueSendTarget, joyuePick string, rr *uint32, rnd *rand.Rand) joyueSendTarget {
	if len(targets) == 1 {
		return targets[0]
	}
	mode := strings.ToLower(strings.TrimSpace(joyuePick))
	switch mode {
	case "random":
		if rnd == nil {
			rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
		}
		return targets[rnd.Intn(len(targets))]
	case "", "round-robin", "rr":
		i := int(*rr % uint32(len(targets)))
		*rr++
		return targets[i]
	default:
		log.Printf("[csv-trigger] 未知 -joyue-pick=%q，改用 round-robin", joyuePick)
		i := int(*rr % uint32(len(targets)))
		*rr++
		return targets[i]
	}
}
