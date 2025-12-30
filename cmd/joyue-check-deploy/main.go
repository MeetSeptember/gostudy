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
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE 部署检查工具

用于检查代理合约是否在其他分片部署成功。

使用方法：
1. 通过部署者地址和 nonce 计算合约地址并查询代码
2. 直接查询指定地址的合约代码
3. 查询交易 receipt 获取合约地址
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
		method = fmt.Sprintf("交易 hash: %s", *txHashStr)
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
			_, err := client.PendingNonceAt(ctx, deployerAddr)
			if err != nil {
				log.Fatalf("获取 nonce 失败: %v", err)
			}
			// 假设是第一次部署，nonce 应该是 0
			// 但为了安全，我们提示用户
			log.Printf("警告：未指定 nonce，使用 0。如果这不是第一次部署，请使用 --nonce 指定正确的 nonce")
			*nonce = 0
		}

		contractAddr = crypto.CreateAddress(deployerAddr, *nonce)
		method = fmt.Sprintf("部署者地址: %s, nonce: %d", deployerAddr.Hex(), *nonce)
	} else {
		flag.Usage()
		log.Fatal("请指定以下参数之一：--contract, --tx, 或 --deployer+--nonce")
	}

	fmt.Printf("\n=== JOYUE 代理合约部署检查 ===\n")
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

	// 尝试调用 initialized() 函数检查是否已初始化
	// JoyueAgent 的 initialized() 函数签名：initialized()(bool)
	initializedSig := crypto.Keccak256([]byte("initialized()"))[:4]
	callData := initializedSig

	msg := ethereum.CallMsg{
		To:   &contractAddr,
		Data: callData,
	}
	result, err := client.CallContract(ctx, msg, nil)
	if err != nil {
		fmt.Printf("⚠️ 调用 initialized() 失败: %v\n", err)
	} else if len(result) == 0 {
		fmt.Printf("⚠️ initialized() 返回空（可能 revert）\n")
	} else if len(result) >= 32 {
		// Solidity 返回的 bool 是 32 字节，最后 1 字节是实际值
		isInitialized := result[31] != 0
		fmt.Printf("初始化状态: %v\n", isInitialized)
		if !isInitialized {
			fmt.Printf("⚠️ 合约未初始化，需要先调用 initialize()\n")
		}
	}

	// 尝试调用 master() 函数获取 master 地址
	// JoyueAgent 的 master() 函数签名：master()(address)
	masterSig := crypto.Keccak256([]byte("master()"))[:4]
	callData = masterSig

	msg = ethereum.CallMsg{
		To:   &contractAddr,
		Data: callData,
	}
	result, err = client.CallContract(ctx, msg, nil)
	if err == nil && len(result) >= 32 {
		// address 类型返回 32 字节，最后 20 字节是地址
		masterAddr := common.BytesToAddress(result[12:32])
		if masterAddr != (common.Address{}) {
			fmt.Printf("Master 地址: %s\n", masterAddr.Hex())
		}
	}

	// 尝试调用 agentShardId() 函数
	agentShardIdSig := crypto.Keccak256([]byte("agentShardId()"))[:4]
	callData = agentShardIdSig

	msg = ethereum.CallMsg{
		To:   &contractAddr,
		Data: callData,
	}
	result, err = client.CallContract(ctx, msg, nil)
	if err == nil && len(result) >= 32 {
		// uint32 类型返回 32 字节，最后 4 字节是值
		shardID := new(big.Int).SetBytes(result[28:32]).Uint64()
		fmt.Printf("Agent Shard ID: %d\n", shardID)
	}

	fmt.Printf("\n✅ 检查完成：合约已成功部署并包含代码\n")
}
