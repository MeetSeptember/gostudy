package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
通用合约部署查询工具

用于检查合约是否部署成功，并查询合约的基本信息。

支持的查询方式：
1. 通过部署者地址和 nonce 计算合约地址
2. 直接查询指定地址
3. 通过交易 hash 查询 receipt 获取合约地址

支持的功能：
- 检查合约是否已部署（是否有代码）
- 查询合约代码长度
- 查询交易 receipt 信息
- 可选：调用标准函数（如 name(), symbol(), decimals() 等）
- 可选：自定义函数调用
*/

func main() {
	var (
		rpcURL = flag.String("rpc", "", "目标分片的 RPC URL（例如：http://127.0.0.1:9501）")

		// 方式1：通过部署者地址和 nonce 计算合约地址
		deployerAddrStr = flag.String("deployer", "", "部署者地址（0x...）")
		nonce           = flag.Uint64("nonce", 0, "部署交易的 nonce")

		// 方式2：直接查询指定地址
		contractAddrStr = flag.String("contract", "", "合约地址（0x...），如果指定则直接查询此地址")

		// 方式3：通过交易 hash 查询
		txHashStr = flag.String("tx", "", "部署交易的 hash（0x...），查询 receipt 获取合约地址")

		// 方式4：通过私钥计算部署者地址
		privateKeyHex = flag.String("private-key", "", "部署私钥（hex，不带 0x），用于计算部署者地址")

		// 可选：查询标准函数
		queryStandard = flag.Bool("query-standard", false, "是否查询标准函数（name, symbol, decimals 等）")

		// 可选：自定义函数调用（格式：functionName,param1,param2...）
		customCalls = flag.String("call", "", "自定义函数调用，多个调用用逗号分隔（例如：name(),symbol(),totalSupply()）")

		// 可选：显示交易详情
		showTxDetails = flag.Bool("show-tx", false, "是否显示交易详情（当使用 --tx 时）")
	)
	flag.Parse()

	if *rpcURL == "" {
		flag.Usage()
		log.Fatal("缺少参数：--rpc")
	}

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}
	defer client.Close()

	var contractAddr common.Address
	var method string
	var _ common.Hash

	// 确定查询方式
	if *contractAddrStr != "" {
		// 方式2：直接查询指定地址
		contractAddr = common.HexToAddress(*contractAddrStr)
		method = "直接指定地址"
	} else if *txHashStr != "" {
		// 方式3：通过交易 hash 查询
		txHash := common.HexToHash(*txHashStr)
		receipt, err := client.TransactionReceipt(ctx, txHash)
		if err != nil {
			log.Fatalf("查询交易 receipt 失败: %v", err)
		}
		if receipt.ContractAddress == (common.Address{}) {
			log.Fatal("该交易不是合约创建交易，或合约地址为空")
		}
		contractAddr = receipt.ContractAddress
		_ = txHash
		method = fmt.Sprintf("交易 hash: %s", *txHashStr)

		if *showTxDetails {
			tx, _, err := client.TransactionByHash(ctx, txHash)
			if err == nil && tx != nil {
				fmt.Printf("\n=== 交易详情 ===\n")
				fmt.Printf("交易 Hash: %s\n", txHash.Hex())

				// 获取发送者地址（使用 go-ethereum 的 types.Sender）
				var fromAddr common.Address
				chainID, err := client.ChainID(ctx)
				if err == nil {
					signer := types.NewEIP155Signer(chainID)
					if addr, err := types.Sender(signer, tx); err == nil {
						fromAddr = addr
					}
				}
				if fromAddr != (common.Address{}) {
					fmt.Printf("From: %s\n", fromAddr.Hex())
				}

				fmt.Printf("Gas Used: %d\n", receipt.GasUsed)
				fmt.Printf("Status: %s\n", getStatusString(receipt.Status))
				fmt.Printf("Block Number: %d\n", receipt.BlockNumber.Uint64())
				fmt.Printf("Block Hash: %s\n", receipt.BlockHash.Hex())
				fmt.Printf("Gas Limit: %d\n", tx.Gas())
				fmt.Printf("Gas Price: %s\n", tx.GasPrice().String())
				fmt.Printf("Value: %s ETH\n", formatWei(tx.Value()))
				if tx.To() != nil {
					fmt.Printf("To: %s\n", tx.To().Hex())
				} else {
					fmt.Printf("To: <合约创建>\n")
				}
				fmt.Printf("Nonce: %d\n", tx.Nonce())
				fmt.Printf("\n")
			}
		}
	} else if *deployerAddrStr != "" || *privateKeyHex != "" {
		// 方式1：通过部署者地址和 nonce 计算
		var deployerAddr common.Address
		if *privateKeyHex != "" {
			// 从私钥计算地址
			keyBytes, err := hex.DecodeString(strings.TrimPrefix(*privateKeyHex, "0x"))
			if err != nil {
				log.Fatalf("解析私钥失败: %v", err)
			}
			privKey, err := crypto.ToECDSA(keyBytes)
			if err != nil {
				log.Fatalf("转换私钥失败: %v", err)
			}
			deployerAddr = crypto.PubkeyToAddress(privKey.PublicKey)
			fmt.Printf("从私钥计算的部署者地址: %s\n", deployerAddr.Hex())
		} else {
			deployerAddr = common.HexToAddress(*deployerAddrStr)
		}

		if *nonce == 0 {
			// 如果没有指定 nonce，尝试获取当前 nonce
			currentNonce, err := client.PendingNonceAt(ctx, deployerAddr)
			if err != nil {
				log.Fatalf("获取 nonce 失败: %v", err)
			}
			if currentNonce == 0 {
				log.Printf("警告：未指定 nonce，使用 0。如果这不是第一次部署，请使用 --nonce 指定正确的 nonce")
				*nonce = 0
			} else {
				log.Printf("警告：未指定 nonce，当前 nonce 为 %d。如果合约已部署，请使用 --nonce 指定部署时的 nonce", currentNonce)
			}
		}

		contractAddr = crypto.CreateAddress(deployerAddr, *nonce)
		method = fmt.Sprintf("部署者地址: %s, nonce: %d", deployerAddr.Hex(), *nonce)
	} else {
		flag.Usage()
		log.Fatal("请指定以下参数之一：--contract, --tx, 或 --deployer+--nonce")
	}

	fmt.Printf("\n=== 合约部署检查 ===\n")
	fmt.Printf("RPC: %s\n", *rpcURL)
	fmt.Printf("查询方式: %s\n", method)
	fmt.Printf("合约地址: %s\n", contractAddr.Hex())
	fmt.Printf("\n")

	// 查询合约代码
	code, err := client.CodeAt(ctx, contractAddr, nil)
	if err != nil {
		log.Fatalf("查询合约代码失败: %v", err)
	}

	if len(code) == 0 {
		fmt.Printf("❌ 合约未部署\n")
		fmt.Printf("地址 %s 没有代码，合约可能尚未部署或部署失败\n", contractAddr.Hex())
		return
	}

	fmt.Printf("✅ 合约已部署\n")
	fmt.Printf("代码长度: %d 字节\n", len(code))
	fmt.Printf("代码 Hash: %s\n", crypto.Keccak256Hash(code).Hex())

	// 查询合约余额
	balance, err := client.BalanceAt(ctx, contractAddr, nil)
	if err == nil {
		fmt.Printf("合约余额: %s ETH\n", formatWei(balance))
	}

	// 查询标准函数
	if *queryStandard {
		queryStandardFunctions(ctx, client, contractAddr)
	}

	// 自定义函数调用
	if *customCalls != "" {
		calls := strings.Split(*customCalls, ",")
		for _, call := range calls {
			call = strings.TrimSpace(call)
			if call != "" {
				callFunction(ctx, client, contractAddr, call)
			}
		}
	}

	fmt.Printf("\n✅ 检查完成：合约已成功部署并包含代码\n")
}

// queryStandardFunctions 查询标准函数（ERC20/ERC721 等）
func queryStandardFunctions(ctx context.Context, client *ethclient.Client, contractAddr common.Address) {
	fmt.Printf("\n=== 标准函数查询 ===\n")

	// name() string
	if result := callFunctionSimple(ctx, client, contractAddr, "name()"); result != nil {
		if name, ok := decodeString(result); ok {
			fmt.Printf("name(): %s\n", name)
		}
	}

	// symbol() string
	if result := callFunctionSimple(ctx, client, contractAddr, "symbol()"); result != nil {
		if symbol, ok := decodeString(result); ok {
			fmt.Printf("symbol(): %s\n", symbol)
		}
	}

	// decimals() uint8
	if result := callFunctionSimple(ctx, client, contractAddr, "decimals()"); result != nil {
		if decimals, ok := decodeUint8(result); ok {
			fmt.Printf("decimals(): %d\n", decimals)
		}
	}

	// totalSupply() uint256
	if result := callFunctionSimple(ctx, client, contractAddr, "totalSupply()"); result != nil {
		if supply, ok := decodeUint256(result); ok {
			fmt.Printf("totalSupply(): %s\n", supply.String())
		}
	}
}

// callFunction 调用自定义函数
func callFunction(ctx context.Context, client *ethclient.Client, contractAddr common.Address, functionSig string) {
	fmt.Printf("\n=== 调用函数: %s ===\n", functionSig)

	result := callFunctionSimple(ctx, client, contractAddr, functionSig)
	if result == nil {
		fmt.Printf("❌ 调用失败或函数不存在\n")
		return
	}

	fmt.Printf("返回数据 (hex): %s\n", hex.EncodeToString(result))
	fmt.Printf("返回数据长度: %d 字节\n", len(result))

	// 尝试解析常见类型
	if len(result) >= 32 {
		// 尝试解析为 address
		if addr := common.BytesToAddress(result[12:32]); addr != (common.Address{}) {
			fmt.Printf("解析为 address: %s\n", addr.Hex())
		}

		// 尝试解析为 uint256
		if val := new(big.Int).SetBytes(result); val.Cmp(big.NewInt(0)) != 0 {
			fmt.Printf("解析为 uint256: %s\n", val.String())
		}

		// 尝试解析为 bool
		if result[31] == 0 || result[31] == 1 {
			fmt.Printf("解析为 bool: %v\n", result[31] != 0)
		}
	}
}

// callFunctionSimple 简单调用函数（不解析结果）
func callFunctionSimple(ctx context.Context, client *ethclient.Client, contractAddr common.Address, functionSig string) []byte {
	sig := crypto.Keccak256([]byte(functionSig))[:4]
	msg := ethereum.CallMsg{
		To:   &contractAddr,
		Data: sig,
	}
	result, err := client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil
	}
	return result
}

// decodeString 解码 Solidity string 类型
func decodeString(data []byte) (string, bool) {
	if len(data) < 32 {
		return "", false
	}
	// Solidity string 编码：offset (32 bytes) + length (32 bytes) + data
	offset := new(big.Int).SetBytes(data[0:32]).Uint64()
	if offset != 32 || len(data) < 64 {
		return "", false
	}
	length := new(big.Int).SetBytes(data[32:64]).Uint64()
	if len(data) < int(64+length) {
		return "", false
	}
	return string(data[64 : 64+length]), true
}

// decodeUint8 解码 uint8
func decodeUint8(data []byte) (uint8, bool) {
	if len(data) < 32 {
		return 0, false
	}
	return data[31], true
}

// decodeUint256 解码 uint256
func decodeUint256(data []byte) (*big.Int, bool) {
	if len(data) < 32 {
		return nil, false
	}
	return new(big.Int).SetBytes(data), true
}

// formatWei 格式化 Wei 为 ETH
func formatWei(wei *big.Int) string {
	eth := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18))
	return eth.Text('f', 18)
}

// getStatusString 获取交易状态字符串
func getStatusString(status uint64) string {
	if status == 1 {
		return "✅ 成功"
	}
	return "❌ 失败"
}
