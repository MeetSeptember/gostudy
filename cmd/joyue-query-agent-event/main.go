package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE Agent 事件查询工具：扫描 Agent 部署时 emit 的 Initialized 事件

用途：
- 查询 JoyueAgent 合约部署时发出的 Initialized 事件
- 确认 cacheAddr 和 rpcOracleAddr 是否正确设置

事件定义（需与 contracts/joyue/JoyueAgent.sol 保持一致）：
event Initialized(
  address indexed master,
  uint32 masterShardId,
  uint32 agentShardId,
  address cacheAddr,
  address rpcOracleAddr
);
*/

const agentEventABI = `[
  {"anonymous":false,"inputs":[
    {"indexed":true,"internalType":"address","name":"master","type":"address"},
    {"indexed":false,"internalType":"uint32","name":"masterShardId","type":"uint32"},
    {"indexed":false,"internalType":"uint32","name":"agentShardId","type":"uint32"},
    {"indexed":false,"internalType":"address","name":"cacheAddr","type":"address"},
    {"indexed":false,"internalType":"address","name":"rpcOracleAddr","type":"address"}
  ],"name":"Initialized","type":"event"}
]`

type agentInitialized struct {
	Master        common.Address
	MasterShardID uint32
	AgentShardID  uint32
	CacheAddr     common.Address
	RpcOracleAddr common.Address

	BlockNumber  uint64
	TxHash       common.Hash
	ContractAddr common.Address // 发出事件的合约地址（Agent 地址）
}

func main() {
	var (
		rpcURL = flag.String("rpc", "http://127.0.0.1:9500", "RPC URL")

		// 扫描范围；不填 to-block 则默认扫到最新
		fromBlock = flag.Uint64("from-block", 0, "从哪个区块开始扫（包含）")
		toBlock   = flag.Uint64("to-block", 0, "扫到哪个区块（包含，0 表示最新）")

		// 可选：只看某一个 master 合约地址（如果你已经知道）
		masterAddrStr = flag.String("master", "", "只输出指定 master 地址的事件（可选，0x...）")

		// 可选：只看某一个 agent 合约地址
		agentAddrStr = flag.String("agent", "", "只输出指定 agent 地址的事件（可选，0x...）")
	)
	flag.Parse()

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}

	abiObj, err := abi.JSON(strings.NewReader(agentEventABI))
	if err != nil {
		log.Fatalf("解析 ABI 失败: %v", err)
	}
	ev := abiObj.Events["Initialized"]

	var masterFilter *common.Address
	if strings.TrimSpace(*masterAddrStr) != "" {
		addr := common.HexToAddress(*masterAddrStr)
		masterFilter = &addr
	}

	var agentFilter *common.Address
	if strings.TrimSpace(*agentAddrStr) != "" {
		addr := common.HexToAddress(*agentAddrStr)
		agentFilter = &addr
	}

	// 确定 to-block：0 表示 latest
	to := new(big.Int)
	if *toBlock == 0 {
		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Fatalf("获取最新区块失败: %v", err)
		}
		to.Set(head.Number)
	} else {
		to.SetUint64(*toBlock)
	}
	from := new(big.Int).SetUint64(*fromBlock)

	// 调试：打印事件签名
	fmt.Printf("事件签名: Initialized(address,uint32,uint32,address,address)\n")
	fmt.Printf("事件 Topic0: %s\n", ev.ID.Hex())
	fmt.Printf("查询区块范围: %d - %d\n", from.Uint64(), to.Uint64())
	if masterFilter != nil {
		fmt.Printf("过滤 Master 地址: %s\n", masterFilter.Hex())
	}
	if agentFilter != nil {
		fmt.Printf("过滤 Agent 地址: %s\n", agentFilter.Hex())
	}
	fmt.Printf("\n")

	// FilterLogs：按 topic0 过滤事件
	q := ethereum.FilterQuery{
		FromBlock: from,
		ToBlock:   to,
		Topics:    [][]common.Hash{{ev.ID}},
	}

	// 如果指定了 agent 地址，添加到地址过滤
	if agentFilter != nil {
		q.Addresses = []common.Address{*agentFilter}
	}

	logs, err := client.FilterLogs(ctx, q)
	if err != nil {
		log.Fatalf("FilterLogs 失败: %v", err)
	}

	// 如果没有找到事件，尝试查询所有日志（用于调试）
	if len(logs) == 0 {
		fmt.Println("没有找到 Initialized 事件")
		return
	}

	for _, lg := range logs {
		evt, err := decodeAgentInitialized(ev, lg)
		if err != nil {
			log.Printf("跳过无法解析的事件: err=%v", err)
			continue
		}

		// 应用过滤
		if masterFilter != nil && evt.Master != *masterFilter {
			continue
		}
		if agentFilter != nil && evt.ContractAddr != *agentFilter {
			continue
		}

		fmt.Printf("=== Initialized ===\n")
		fmt.Printf("block=%d tx=%s\n", evt.BlockNumber, evt.TxHash.Hex())
		fmt.Printf("agent=%s\n", evt.ContractAddr.Hex())
		fmt.Printf("master=%s\n", evt.Master.Hex())
		fmt.Printf("masterShardId=%d\n", evt.MasterShardID)
		fmt.Printf("agentShardId=%d\n", evt.AgentShardID)
		fmt.Printf("cacheAddr=%s\n", evt.CacheAddr.Hex())
		fmt.Printf("rpcOracleAddr=%s\n", evt.RpcOracleAddr.Hex())
		fmt.Println()
	}

	// 给用户一点时间戳信息，便于日志对齐
	fmt.Printf("done at %s\n", time.Now().Format(time.RFC3339))
}

func decodeAgentInitialized(ev abi.Event, lg ethtypes.Log) (*agentInitialized, error) {
	// topics: [sig, indexed master]
	if len(lg.Topics) < 2 {
		return nil, fmt.Errorf("topics 不足: %d", len(lg.Topics))
	}
	master := common.BytesToAddress(lg.Topics[1].Bytes()[12:])

	vals, err := ev.Inputs.NonIndexed().Unpack(lg.Data)
	if err != nil {
		return nil, err
	}
	if len(vals) != 4 {
		return nil, fmt.Errorf("non-indexed 字段数量不对: 期望 4 个，实际 %d", len(vals))
	}

	// masterShardId（uint32 / uint64 / *big.Int）
	var masterShardID uint32
	switch v := vals[0].(type) {
	case uint32:
		masterShardID = v
	case uint64:
		masterShardID = uint32(v)
	case *big.Int:
		masterShardID = uint32(v.Uint64())
	default:
		return nil, fmt.Errorf("masterShardId 类型未知: %T", vals[0])
	}

	// agentShardId（uint32 / uint64 / *big.Int）
	var agentShardID uint32
	switch v := vals[1].(type) {
	case uint32:
		agentShardID = v
	case uint64:
		agentShardID = uint32(v)
	case *big.Int:
		agentShardID = uint32(v.Uint64())
	default:
		return nil, fmt.Errorf("agentShardId 类型未知: %T", vals[1])
	}

	// cacheAddr（address）
	var cacheAddr common.Address
	switch v := vals[2].(type) {
	case common.Address:
		cacheAddr = v
	case []byte:
		if len(v) >= 20 {
			cacheAddr = common.BytesToAddress(v[:20])
		}
	default:
		// 如果解析失败，使用零地址（不是错误）
		cacheAddr = common.Address{}
	}

	// rpcOracleAddr（address）
	var rpcOracleAddr common.Address
	switch v := vals[3].(type) {
	case common.Address:
		rpcOracleAddr = v
	case []byte:
		if len(v) >= 20 {
			rpcOracleAddr = common.BytesToAddress(v[:20])
		}
	default:
		// 如果解析失败，使用零地址（不是错误）
		rpcOracleAddr = common.Address{}
	}

	return &agentInitialized{
		Master:        master,
		MasterShardID: masterShardID,
		AgentShardID:  agentShardID,
		CacheAddr:     cacheAddr,
		RpcOracleAddr: rpcOracleAddr,
		BlockNumber:   lg.BlockNumber,
		TxHash:        lg.TxHash,
		ContractAddr:  lg.Address, // Agent 合约地址
	}, nil
}
