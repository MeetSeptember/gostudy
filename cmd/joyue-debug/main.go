package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE 调试工具

用于调试合约调用问题，检查合约状态。
*/

func main() {
	var (
		rpcURL       = flag.String("rpc", "http://127.0.0.1:9500", "RPC URL")
		contractAddr = flag.String("contract", "", "合约地址（0x...）")
		checkCode    = flag.Bool("check-code", false, "检查合约代码")
		checkBalance = flag.Bool("check-balance", false, "检查合约余额")
		checkNonce   = flag.Bool("check-nonce", false, "检查账户 nonce")
		address      = flag.String("address", "", "要检查的地址（用于 balance/nonce）")
	)
	flag.Parse()

	if *contractAddr == "" && !*checkNonce && *address == "" {
		flag.Usage()
		log.Fatal("请指定 --contract 或 --address")
	}

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}
	defer client.Close()

	fmt.Printf("=== JOYUE 调试工具 ===\n")
	fmt.Printf("RPC: %s\n", *rpcURL)
	fmt.Printf("\n")

	if *contractAddr != "" {
		addr := common.HexToAddress(*contractAddr)
		fmt.Printf("合约地址: %s\n", addr.Hex())
		fmt.Printf("\n")

		// 检查合约代码
		if *checkCode || true { // 默认检查
			code, err := client.CodeAt(ctx, addr, nil)
			if err != nil {
				log.Fatalf("查询合约代码失败: %v", err)
			}
			if len(code) == 0 {
				fmt.Printf("❌ 合约代码为空（合约可能未部署或地址错误）\n")
			} else {
				fmt.Printf("✅ 合约代码存在\n")
				fmt.Printf("代码长度: %d 字节\n", len(code))
				fmt.Printf("代码前 100 字节: 0x%s\n", hex.EncodeToString(code[:min(100, len(code))]))
			}
			fmt.Printf("\n")
		}

		// 检查合约余额
		if *checkBalance {
			balance, err := client.BalanceAt(ctx, addr, nil)
			if err != nil {
				log.Fatalf("查询余额失败: %v", err)
			}
			fmt.Printf("合约余额: %s wei\n", balance.String())
			fmt.Printf("\n")
		}
	}

	// 检查账户状态
	if *address != "" {
		addr := common.HexToAddress(*address)
		fmt.Printf("账户地址: %s\n", addr.Hex())
		fmt.Printf("\n")

		if *checkBalance {
			balance, err := client.BalanceAt(ctx, addr, nil)
			if err != nil {
				log.Fatalf("查询余额失败: %v", err)
			}
			fmt.Printf("账户余额: %s wei\n", balance.String())
			fmt.Printf("\n")
		}

		if *checkNonce {
			nonce, err := client.PendingNonceAt(ctx, addr)
			if err != nil {
				log.Fatalf("查询 nonce 失败: %v", err)
			}
			fmt.Printf("账户 nonce: %d\n", nonce)
			fmt.Printf("\n")
		}
	}

	// 检查链信息
	chainID, err := client.ChainID(ctx)
	if err == nil {
		fmt.Printf("Chain ID: %s\n", chainID.String())
	}

	blockNum, err := client.BlockNumber(ctx)
	if err == nil {
		fmt.Printf("当前区块号: %d\n", blockNum)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
