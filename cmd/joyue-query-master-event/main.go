package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE 事件查询工具：扫描 Master 部署时 emit 的 MasterDeployed 事件

用途：
- 你部署 JoyueMaster 后，想确认 constructor 里 emit 的事件是否正确
- 想把事件里的 agentCreationCode 导出成文件（后续可以喂给 relayer 或做比对）

兼容性：
- 本仓库 go-ethereum replace 到 v1.11.2，所以事件 data 解码使用 Arguments.Unpack（不要用 UnpackIntoInterface）。

事件定义（需与 contracts/joyue/JoyueMaster.sol 保持一致）：
event MasterDeployed(
  address indexed master,
  bytes32 indexed salt,
  bytes agentCreationCode,
  uint32 masterShardId,
  address cacheAddr,
  address rpcOracleAddr
);
*/

const masterEventABI = `[
  {"anonymous":false,"inputs":[
    {"indexed":true,"internalType":"address","name":"master","type":"address"},
    {"indexed":true,"internalType":"bytes32","name":"salt","type":"bytes32"},
    {"indexed":false,"internalType":"bytes","name":"agentCreationCode","type":"bytes"},
    {"indexed":false,"internalType":"uint32","name":"masterShardId","type":"uint32"},
    {"indexed":false,"internalType":"address","name":"cacheAddr","type":"address"},
    {"indexed":false,"internalType":"address","name":"rpcOracleAddr","type":"address"}
  ],"name":"MasterDeployed","type":"event"}
]`

type masterDeployed struct {
	Master            common.Address
	Salt              common.Hash
	AgentCreationCode []byte
	MasterShardID     uint32
	CacheAddr         common.Address
	RpcOracleAddr     common.Address

	BlockNumber uint64
	TxHash      common.Hash
}

func main() {
	var (
		rpcURL = flag.String("rpc", "http://127.0.0.1:9500", "RPC URL（一般是 shard0）")

		// 扫描范围；不填 to-block 则默认扫到最新
		fromBlock = flag.Uint64("from-block", 0, "从哪个区块开始扫（包含）")
		toBlock   = flag.Uint64("to-block", 0, "扫到哪个区块（包含，0 表示最新）")

		// 可选：只看某一个 master 合约地址（如果你已经知道）
		masterAddrStr = flag.String("master", "", "只输出指定 master 地址的事件（可选，0x...）")

		// 输出 agentCreationCode 到文件（可选）
		outFile = flag.String("out-agent-code", "", "把 agentCreationCode 导出到文件（内容是 hex，不带 0x）（可选）")

		// 打印时避免把超长 hex 一股脑输出
		printCodeHex = flag.Bool("print-code-hex", false, "是否把 agentCreationCode 的 hex 打印到 stdout（不建议，可能很长）")
	)
	flag.Parse()

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}

	abiObj, err := abi.JSON(strings.NewReader(masterEventABI))
	if err != nil {
		log.Fatalf("解析 ABI 失败: %v", err)
	}
	ev := abiObj.Events["MasterDeployed"]

	var masterFilter *common.Address
	if strings.TrimSpace(*masterAddrStr) != "" {
		addr := common.HexToAddress(*masterAddrStr)
		masterFilter = &addr
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
	fmt.Printf("事件签名: MasterDeployed(address,bytes32,bytes,uint32,address,address)\n")
	fmt.Printf("事件 Topic0: %s\n", ev.ID.Hex())
	fmt.Printf("查询区块范围: %d - %d\n", from.Uint64(), to.Uint64())
	fmt.Printf("\n")

	// FilterLogs：按 topic0 过滤事件
	q := ethereum.FilterQuery{
		FromBlock: from,
		ToBlock:   to,
		Topics:    [][]common.Hash{{ev.ID}},
	}

	logs, err := client.FilterLogs(ctx, q)
	if err != nil {
		log.Fatalf("FilterLogs 失败: %v", err)
	}

	// 如果没有找到事件，尝试查询所有日志（用于调试）
	if len(logs) == 0 {
		fmt.Println("没有找到 MasterDeployed 事件")
		fmt.Println("\n尝试查询所有日志（用于调试）...")

		// 查询该合约地址的所有日志
		if masterFilter != nil {
			allLogsQuery := ethereum.FilterQuery{
				FromBlock: from,
				ToBlock:   to,
				Addresses: []common.Address{*masterFilter},
			}
			allLogs, err := client.FilterLogs(ctx, allLogsQuery)
			if err == nil {
				fmt.Printf("找到 %d 条日志（来自合约 %s）\n", len(allLogs), masterFilter.Hex())
				for i, lg := range allLogs {
					if i < 5 { // 只显示前 5 条
						fmt.Printf("  日志 #%d: Topic0=%s, Topics数量=%d\n", i+1, lg.Topics[0].Hex(), len(lg.Topics))
					}
				}
			}
		}
		return
	}

	for _, lg := range logs {
		evt, err := decodeMasterDeployed(ev, lg)
		if err != nil {
			log.Printf("跳过无法解析的事件: err=%v", err)
			continue
		}
		if masterFilter != nil && evt.Master != *masterFilter {
			continue
		}

		codeHash := crypto.Keccak256Hash(evt.AgentCreationCode)

		fmt.Printf("=== MasterDeployed ===\n")
		fmt.Printf("block=%d tx=%s\n", evt.BlockNumber, evt.TxHash.Hex())
		fmt.Printf("master=%s\n", evt.Master.Hex())
		fmt.Printf("salt=%s\n", evt.Salt.Hex())
		fmt.Printf("masterShardId=%d\n", evt.MasterShardID)
		fmt.Printf("cacheAddr=%s\n", evt.CacheAddr.Hex())
		fmt.Printf("rpcOracleAddr=%s\n", evt.RpcOracleAddr.Hex())
		fmt.Printf("agentCreationCodeLen=%d\n", len(evt.AgentCreationCode))
		fmt.Printf("agentCreationCodeHash=%s\n", codeHash.Hex())

		if *printCodeHex {
			fmt.Printf("agentCreationCodeHex=%s\n", hex.EncodeToString(evt.AgentCreationCode))
		}

		if strings.TrimSpace(*outFile) != "" {
			// 以 hex（不带 0x）写文件，方便直接粘贴/读取
			content := hex.EncodeToString(evt.AgentCreationCode)
			// 简单防止误覆盖：如果文件已存在，给出提示（不自动覆盖）
			if _, err := os.Stat(*outFile); err == nil {
				log.Fatalf("输出文件已存在，避免覆盖：%s", *outFile)
			}
			if err := os.WriteFile(*outFile, []byte(content), 0o644); err != nil {
				log.Fatalf("写输出文件失败: %v", err)
			}
			fmt.Printf("已导出 agentCreationCode 到文件：%s\n", *outFile)
		}

		fmt.Println()
	}

	// 给用户一点时间戳信息，便于日志对齐
	fmt.Printf("done at %s\n", time.Now().Format(time.RFC3339))
}

func decodeMasterDeployed(ev abi.Event, lg ethtypes.Log) (*masterDeployed, error) {
	// topics: [sig, indexed master, indexed salt]
	if len(lg.Topics) < 3 {
		return nil, fmt.Errorf("topics 不足: %d", len(lg.Topics))
	}
	master := common.BytesToAddress(lg.Topics[1].Bytes()[12:])
	salt := common.BytesToHash(lg.Topics[2].Bytes())

	vals, err := ev.Inputs.NonIndexed().Unpack(lg.Data)
	if err != nil {
		return nil, err
	}
	if len(vals) != 4 {
		return nil, fmt.Errorf("non-indexed 字段数量不对: 期望 4 个，实际 %d", len(vals))
	}

	// agentCreationCode（bytes）
	var agentCode []byte
	switch v := vals[0].(type) {
	case []byte:
		agentCode = append(agentCode[:0:0], v...)
	default:
		return nil, fmt.Errorf("agentCreationCode 类型未知: %T", vals[0])
	}

	// masterShardId（uint32 / uint64 / *big.Int）
	var shardID uint32
	switch v := vals[1].(type) {
	case uint32:
		shardID = v
	case uint64:
		shardID = uint32(v)
	case *big.Int:
		shardID = uint32(v.Uint64())
	default:
		return nil, fmt.Errorf("masterShardId 类型未知: %T", vals[1])
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

	return &masterDeployed{
		Master:            master,
		Salt:              salt,
		AgentCreationCode: agentCode,
		MasterShardID:     shardID,
		CacheAddr:         cacheAddr,
		RpcOracleAddr:     rpcOracleAddr,
		BlockNumber:       lg.BlockNumber,
		TxHash:            lg.TxHash,
	}, nil
}
