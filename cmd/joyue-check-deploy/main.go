package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE 小工具：检查“合约部署交易”是否成功

输入：
- --rpc: 分片 RPC（HTTP）
- --tx:  部署交易 hash（0x...）

输出：
- receipt.status（成功=1，失败=0）
- receipt.contractAddress（部署出来的合约地址）
- blockNumber / gasUsed
- eth_getCode(contractAddress) 的长度（是否真的有代码）

用途：
- relayer 发起代理合约部署后，你想快速确认“是否部署成功”
*/

func main() {
	var (
		rpcURL  = flag.String("rpc", "http://127.0.0.1:9500", "RPC URL（例如 shard1: http://127.0.0.1:9501）")
		txHashS = flag.String("tx", "", "部署交易 hash（0x...）")
		wait    = flag.Bool("wait", false, "如果 receipt 还没出来，是否等待一段时间")
		timeout = flag.Duration("timeout", 60*time.Second, "等待 receipt 超时时间（仅 wait=true 时生效）")
	)
	flag.Parse()

	if strings.TrimSpace(*txHashS) == "" {
		flag.Usage()
		log.Fatal("缺少参数：--tx")
	}
	txHash := common.HexToHash(*txHashS)

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}

	var receipt *ethtypes.Receipt
	if *wait {
		ctx2, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		receipt, err = waitReceipt(ctx2, client, txHash)
	} else {
		receipt, err = client.TransactionReceipt(ctx, txHash)
	}
	if err != nil {
		fmt.Printf("tx=%s\n", txHash.Hex())
		fmt.Printf("receipt=NOT_FOUND err=%v\n", err)
		fmt.Println("结论：未确认（交易可能还没上链/节点暂时查不到）。可以加 --wait 再试。")
		return
	}

	fmt.Printf("tx=%s\n", txHash.Hex())
	fmt.Printf("blockNumber=%d\n", receipt.BlockNumber.Uint64())
	fmt.Printf("status=%d\n", receipt.Status)
	fmt.Printf("gasUsed=%d\n", receipt.GasUsed)
	fmt.Printf("contractAddress=%s\n", receipt.ContractAddress.Hex())

	if receipt.ContractAddress == (common.Address{}) {
		fmt.Println("结论：这笔交易不是合约创建（contractAddress 为空）。")
		return
	}

	code, err := client.CodeAt(ctx, receipt.ContractAddress, nil)
	if err != nil {
		fmt.Printf("getCodeErr=%v\n", err)
		fmt.Println("结论：receipt 有了，但读取代码失败（RPC/节点问题）。")
		return
	}
	fmt.Printf("codeBytes=%d\n", len(code))

	// 最终结论
	if receipt.Status == 1 && len(code) > 0 {
		fmt.Println("结论：部署成功（status=1 且合约代码存在）。")
		return
	}
	if receipt.Status == 0 {
		fmt.Println("结论：部署失败（status=0，EVM 执行回滚）。")
		return
	}
	if len(code) == 0 {
		fmt.Println("结论：异常：status=1 但 code 为空（可能是自毁/节点数据问题/查询高度问题）。")
		return
	}
	fmt.Println("结论：未知状态。")
}

func waitReceipt(ctx context.Context, client *ethclient.Client, tx common.Hash) (*ethtypes.Receipt, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		receipt, err := client.TransactionReceipt(ctx, tx)
		if err == nil && receipt != nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
