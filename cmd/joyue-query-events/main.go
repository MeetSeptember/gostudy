package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE 通用事件查询工具

用途：
- 查询任意合约发出的任意事件
- 支持通过事件签名或 ABI 文件解析事件
- 支持按合约地址、事件签名、区块范围过滤

使用示例：
1. 通过事件签名查询：
   go run main.go \
     --rpc http://127.0.0.1:9500 \
     --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
     --event "MasterDeployed(address,bytes32,bytes,uint32)" \
     --from-block 0

2. 通过 ABI 文件查询：
   go run main.go \
     --rpc http://127.0.0.1:9500 \
     --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
     --abi-file contracts/joyue/JoyueMaster.abi \
     --event-name "MasterDeployed" \
     --from-block 0

3. 查询所有事件（不指定事件签名）：
   go run main.go \
     --rpc http://127.0.0.1:9500 \
     --contract 0x12a4113F44E5689C93df233008FD805BDc882ABb \
     --from-block 0
*/

func main() {
	var (
		rpcURL = flag.String("rpc", "http://127.0.0.1:9500", "RPC URL")

		// 合约地址
		contractAddrStr = flag.String("contract", "", "合约地址（0x...）")

		// 事件查询方式（三选一）
		eventSig  = flag.String("event", "", "事件签名（例如：MasterDeployed(address,bytes32,bytes,uint32)）")
		abiFile   = flag.String("abi-file", "", "ABI 文件路径（JSON 格式）")
		eventName = flag.String("event-name", "", "事件名称（需要配合 --abi-file 使用）")

		// 区块范围
		fromBlock = flag.Uint64("from-block", 0, "起始区块号（包含）")
		toBlock   = flag.Uint64("to-block", 0, "结束区块号（包含，0 表示最新）")

		// 输出选项
		outputJSON = flag.Bool("json", false, "以 JSON 格式输出")
		verbose    = flag.Bool("verbose", false, "显示详细信息（包括原始 log 数据）")
	)

	flag.Parse()

	if *contractAddrStr == "" {
		flag.Usage()
		log.Fatal("缺少参数：--contract")
	}

	contractAddr := common.HexToAddress(*contractAddrStr)

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}
	defer client.Close()

	// 确定区块范围
	var from, to *big.Int
	from = new(big.Int).SetUint64(*fromBlock)
	if *toBlock == 0 {
		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Fatalf("获取最新区块失败: %v", err)
		}
		to = head.Number
	} else {
		to = new(big.Int).SetUint64(*toBlock)
	}

	// 构建查询条件
	query := ethereum.FilterQuery{
		FromBlock: from,
		ToBlock:   to,
		Addresses: []common.Address{contractAddr},
	}

	// 如果指定了事件签名，添加到 topics
	var eventABI *abi.Event
	var eventABIStr string
	if *eventSig != "" {
		// 通过事件签名计算 topic0
		eventSigBytes := crypto.Keccak256([]byte(*eventSig))
		eventTopic := common.BytesToHash(eventSigBytes)
		query.Topics = [][]common.Hash{{eventTopic}}
		eventABIStr = *eventSig
	} else if *abiFile != "" && *eventName != "" {
		// 通过 ABI 文件解析事件
		abiData, err := os.ReadFile(*abiFile)
		if err != nil {
			log.Fatalf("读取 ABI 文件失败: %v", err)
		}

		var abiJSON abi.ABI
		if err := json.Unmarshal(abiData, &abiJSON); err != nil {
			log.Fatalf("解析 ABI JSON 失败: %v", err)
		}

		ev, ok := abiJSON.Events[*eventName]
		if !ok {
			log.Fatalf("ABI 中未找到事件: %s", *eventName)
		}

		eventABI = &ev
		query.Topics = [][]common.Hash{{ev.ID}}
		eventABIStr = ev.String()
	}

	// 查询日志
	logs, err := client.FilterLogs(ctx, query)
	if err != nil {
		log.Fatalf("FilterLogs 失败: %v", err)
	}

	if len(logs) == 0 {
		fmt.Printf("未找到事件（合约: %s, 区块范围: %d-%d）\n", contractAddr.Hex(), from.Uint64(), to.Uint64())
		return
	}

	fmt.Printf("找到 %d 个事件\n", len(logs))
	fmt.Printf("合约地址: %s\n", contractAddr.Hex())
	if eventABIStr != "" {
		fmt.Printf("事件签名: %s\n", eventABIStr)
	}
	fmt.Printf("区块范围: %d - %d\n", from.Uint64(), to.Uint64())
	fmt.Println()

	// 解析并输出事件
	for i, lg := range logs {
		if *outputJSON {
			outputEventJSON(lg, eventABI, i+1)
		} else {
			outputEventText(lg, eventABI, eventABIStr, *verbose, i+1)
		}
	}

	fmt.Printf("\n查询完成，共找到 %d 个事件\n", len(logs))
}

// outputEventText 以文本格式输出事件
func outputEventText(log ethtypes.Log, eventABI *abi.Event, eventSig string, verbose bool, index int) {
	fmt.Printf("=== 事件 #%d ===\n", index)
	fmt.Printf("区块号: %d\n", log.BlockNumber)
	fmt.Printf("交易哈希: %s\n", log.TxHash.Hex())
	fmt.Printf("日志索引: %d\n", log.Index)
	fmt.Printf("合约地址: %s\n", log.Address.Hex())

	// 显示 topic0（事件签名）
	if len(log.Topics) > 0 {
		fmt.Printf("事件签名 (topic0): %s\n", log.Topics[0].Hex())
	}

	// 如果有 ABI，尝试解析事件
	if eventABI != nil {
		fmt.Printf("事件名称: %s\n", eventABI.Name)
		fmt.Println("\n--- 事件参数 ---")

		// 解析 indexed 参数（从 topics 中）
		if len(eventABI.Inputs) > 0 {
			indexedArgs := make([]abi.Argument, 0)
			for _, input := range eventABI.Inputs {
				if input.Indexed {
					indexedArgs = append(indexedArgs, input)
				}
			}

			if len(indexedArgs) > 0 && len(log.Topics) > 1 {
				fmt.Println("Indexed 参数:")
				for i, arg := range indexedArgs {
					if i+1 < len(log.Topics) {
						topic := log.Topics[i+1]
						value := formatTopicValue(topic, arg.Type)
						fmt.Printf("  %s (%s): %s\n", arg.Name, arg.Type, value)
					}
				}
			}

			// 解析 non-indexed 参数（从 data 中）
			nonIndexedArgs := make([]abi.Argument, 0)
			for _, input := range eventABI.Inputs {
				if !input.Indexed {
					nonIndexedArgs = append(nonIndexedArgs, input)
				}
			}

			if len(nonIndexedArgs) > 0 && len(log.Data) > 0 {
				fmt.Println("Non-indexed 参数:")
				vals, err := eventABI.Inputs.NonIndexed().Unpack(log.Data)
				if err != nil {
					fmt.Printf("  解析失败: %v\n", err)
				} else {
					for i, arg := range nonIndexedArgs {
						if i < len(vals) {
							value := formatValue(vals[i], arg.Type)
							fmt.Printf("  %s (%s): %s\n", arg.Name, arg.Type, value)
						}
					}
				}
			}
		}
	} else if eventSig != "" {
		// 只有事件签名，显示 topics 和 data
		fmt.Println("\n--- Topics ---")
		for i, topic := range log.Topics {
			if i == 0 {
				fmt.Printf("  Topic[0] (事件签名): %s\n", topic.Hex())
			} else {
				fmt.Printf("  Topic[%d] (indexed 参数): %s\n", i, topic.Hex())
			}
		}

		if len(log.Data) > 0 {
			fmt.Printf("\n--- Data (non-indexed 参数) ---\n")
			fmt.Printf("  长度: %d 字节\n", len(log.Data))
			if verbose {
				fmt.Printf("  内容: 0x%s\n", hex.EncodeToString(log.Data))
			} else {
				// 只显示前 100 字节
				preview := log.Data
				if len(preview) > 100 {
					preview = preview[:100]
					fmt.Printf("  内容 (前 100 字节): 0x%s...\n", hex.EncodeToString(preview))
				} else {
					fmt.Printf("  内容: 0x%s\n", hex.EncodeToString(preview))
				}
			}
		}
	} else {
		// 没有事件签名，显示所有 topics 和 data
		fmt.Println("\n--- Topics ---")
		for i, topic := range log.Topics {
			fmt.Printf("  Topic[%d]: %s\n", i, topic.Hex())
		}

		if len(log.Data) > 0 {
			fmt.Printf("\n--- Data ---\n")
			fmt.Printf("  长度: %d 字节\n", len(log.Data))
			if verbose {
				fmt.Printf("  内容: 0x%s\n", hex.EncodeToString(log.Data))
			} else {
				preview := log.Data
				if len(preview) > 100 {
					preview = preview[:100]
					fmt.Printf("  内容 (前 100 字节): 0x%s...\n", hex.EncodeToString(preview))
				} else {
					fmt.Printf("  内容: 0x%s\n", hex.EncodeToString(preview))
				}
			}
		}
	}

	if verbose {
		fmt.Printf("\n--- 原始日志信息 ---\n")
		fmt.Printf("Removed: %v\n", log.Removed)
		fmt.Printf("BlockHash: %s\n", log.BlockHash.Hex())
		fmt.Printf("TxIndex: %d\n", log.TxIndex)
	}

	fmt.Println()
}

// outputEventJSON 以 JSON 格式输出事件
func outputEventJSON(log ethtypes.Log, eventABI *abi.Event, index int) {
	result := map[string]interface{}{
		"index":       index,
		"blockNumber": log.BlockNumber,
		"txHash":      log.TxHash.Hex(),
		"logIndex":    log.Index,
		"address":     log.Address.Hex(),
		"topics":      make([]string, len(log.Topics)),
		"data":        "0x" + hex.EncodeToString(log.Data),
	}

	for i, topic := range log.Topics {
		result["topics"].([]string)[i] = topic.Hex()
	}

	// 如果有 ABI，解析事件参数
	if eventABI != nil {
		result["eventName"] = eventABI.Name

		params := make(map[string]interface{})
		result["parameters"] = params

		// 解析 indexed 参数
		if len(eventABI.Inputs) > 0 {
			indexedArgs := make([]abi.Argument, 0)
			for _, input := range eventABI.Inputs {
				if input.Indexed {
					indexedArgs = append(indexedArgs, input)
				}
			}

			if len(indexedArgs) > 0 && len(log.Topics) > 1 {
				indexedParams := make(map[string]interface{})
				for i, arg := range indexedArgs {
					if i+1 < len(log.Topics) {
						topic := log.Topics[i+1]
						value := formatTopicValue(topic, arg.Type)
						indexedParams[arg.Name] = value
					}
				}
				params["indexed"] = indexedParams
			}

			// 解析 non-indexed 参数
			nonIndexedArgs := make([]abi.Argument, 0)
			for _, input := range eventABI.Inputs {
				if !input.Indexed {
					nonIndexedArgs = append(nonIndexedArgs, input)
				}
			}

			if len(nonIndexedArgs) > 0 && len(log.Data) > 0 {
				vals, err := eventABI.Inputs.NonIndexed().Unpack(log.Data)
				if err == nil {
					nonIndexedParams := make(map[string]interface{})
					for i, arg := range nonIndexedArgs {
						if i < len(vals) {
							value := formatValue(vals[i], arg.Type)
							nonIndexedParams[arg.Name] = value
						}
					}
					params["nonIndexed"] = nonIndexedParams
				}
			}
		}
	}

	jsonData, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "JSON 序列化失败: %v\n", err)
		return
	}

	fmt.Println(string(jsonData))
	fmt.Println()
}

// formatTopicValue 格式化 topic 值
func formatTopicValue(topic common.Hash, typ abi.Type) string {
	switch typ.T {
	case abi.AddressTy:
		// address 在 topic 中是 32 字节，最后 20 字节是地址
		addr := common.BytesToAddress(topic[12:])
		return addr.Hex()
	case abi.BoolTy:
		// bool 在 topic 中，非零表示 true
		return fmt.Sprintf("%v", topic.Big().Sign() != 0)
	case abi.IntTy, abi.UintTy:
		// 整数类型
		return topic.Big().String()
	case abi.BytesTy, abi.FixedBytesTy:
		// bytes 类型
		return "0x" + hex.EncodeToString(topic.Bytes())
	case abi.StringTy:
		// string 在 topic 中是 hash
		return topic.Hex()
	default:
		return topic.Hex()
	}
}

// formatValue 格式化值
func formatValue(val interface{}, typ abi.Type) string {
	switch v := val.(type) {
	case common.Address:
		return v.Hex()
	case []byte:
		if len(v) > 100 {
			return fmt.Sprintf("0x%s... (%d bytes)", hex.EncodeToString(v[:100]), len(v))
		}
		return "0x" + hex.EncodeToString(v)
	case *big.Int:
		return v.String()
	case string:
		return v
	case bool:
		return fmt.Sprintf("%v", v)
	case uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%v", v)
	case int8, int16, int32, int64:
		return fmt.Sprintf("%v", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
