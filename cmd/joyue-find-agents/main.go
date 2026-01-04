package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE 工具：查找指定 Master 合约在其他分片上的代理合约（Agent 合约）

使用方法：
  go run cmd/joyue-find-agents/main.go \
    --master-addr="0x36d9eaa5eCF358e2653B489046b0d3dF385B13A6" \
    --deployer-key="3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6" \
    --shard-rpcs="1=http://127.0.0.1:9501,2=http://127.0.0.1:9502"

工作原理：
1. 从部署私钥计算部署者地址
2. 查询每个分片的部署者 nonce
3. 根据 nonce 计算可能的 Agent 合约地址（crypto.CreateAddress(deployer, nonce)）
4. 验证合约是否存在，并读取 master 地址进行匹配
*/

func main() {
	var (
		masterAddrHex  = flag.String("master-addr", "", "Master 合约地址（必需）")
		deployerKeyHex = flag.String("deployer-key", "", "部署私钥 hex（不带 0x，必需）")
		shardRPCsStr   = flag.String("shard-rpcs", "", "分片 RPC 地址，格式：shardID=rpcURL,shardID=rpcURL（必需）")
		timeout        = flag.Duration("timeout", 10*time.Second, "RPC 查询超时")
		maxNonceCheck  = flag.Uint64("max-nonce", 100, "最大检查 nonce 范围（从 0 到 max-nonce）")
	)
	flag.Parse()

	if *masterAddrHex == "" {
		flag.Usage()
		fmt.Println("\n错误：缺少 --master-addr 参数")
		return
	}
	if *deployerKeyHex == "" {
		flag.Usage()
		fmt.Println("\n错误：缺少 --deployer-key 参数")
		return
	}
	if *shardRPCsStr == "" {
		flag.Usage()
		fmt.Println("\n错误：缺少 --shard-rpcs 参数")
		return
	}

	masterAddr := common.HexToAddress(*masterAddrHex)
	if masterAddr == (common.Address{}) {
		fmt.Printf("错误：无效的 Master 地址: %s\n", *masterAddrHex)
		return
	}

	// 解析部署私钥
	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*deployerKeyHex, "0x"))
	if err != nil {
		fmt.Printf("错误：私钥格式错误: %v\n", err)
		return
	}
	deployerAddr := crypto.PubkeyToAddress(privKey.PublicKey)
	fmt.Printf("部署者地址: %s\n", deployerAddr.Hex())
	fmt.Printf("Master 合约地址: %s\n", masterAddr.Hex())
	fmt.Println()

	// 解析分片 RPC 配置
	shardRPCs, err := parseShardRPCs(*shardRPCsStr)
	if err != nil {
		fmt.Printf("错误：解析分片 RPC 配置失败: %v\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Agent 合约 ABI（用于读取 master 地址）
	agentABI := `[{"constant":true,"inputs":[],"name":"master","outputs":[{"name":"","type":"address"}],"type":"function"}]`
	parsedABI, err := abi.JSON(strings.NewReader(agentABI))
	if err != nil {
		fmt.Printf("错误：解析 ABI 失败: %v\n", err)
		return
	}
	masterMethod := parsedABI.Methods["master"]

	fmt.Println("=== 查找 Agent 合约 ===")
	fmt.Println()

	foundCount := 0
	for _, shardRPC := range shardRPCs {
		fmt.Printf("检查分片 %d (%s)...\n", shardRPC.ShardID, shardRPC.RPCURL)

		client, err := ethclient.DialContext(ctx, shardRPC.RPCURL)
		if err != nil {
			fmt.Printf("  ❌ 连接失败: %v\n", err)
			continue
		}

		// 获取部署者的当前 nonce
		currentNonce, err := client.PendingNonceAt(ctx, deployerAddr)
		if err != nil {
			fmt.Printf("  ❌ 获取 nonce 失败: %v\n", err)
			client.Close()
			continue
		}

		fmt.Printf("  部署者当前 nonce: %d\n", currentNonce)

		// 检查从 0 到 maxNonceCheck 的所有可能 nonce
		checkEnd := currentNonce
		if checkEnd > *maxNonceCheck {
			checkEnd = *maxNonceCheck
		}

		foundInShard := false
		for nonce := uint64(0); nonce < checkEnd; nonce++ {
			// 计算可能的 Agent 合约地址
			possibleAddr := crypto.CreateAddress(deployerAddr, nonce)

			// 检查合约是否存在（有代码）
			code, err := client.CodeAt(ctx, possibleAddr, nil)
			if err != nil {
				continue
			}
			if len(code) == 0 {
				continue
			}

			// 尝试读取 master 地址
			callData := masterMethod.ID

			result, err := client.CallContract(ctx, ethereum.CallMsg{
				To:   &possibleAddr,
				Data: callData,
			}, nil)
			if err != nil {
				continue
			}

			// 解析返回的地址
			var agentMasterAddr common.Address
			if err := masterMethod.Outputs.Unpack(&agentMasterAddr, result); err != nil {
				continue
			}

			// 检查是否匹配
			if agentMasterAddr == masterAddr {
				fmt.Printf("  ✅ 找到 Agent 合约！\n")
				fmt.Printf("     地址: %s\n", possibleAddr.Hex())
				fmt.Printf("     Nonce: %d\n", nonce)
				fmt.Printf("     代码长度: %d 字节\n", len(code))
				fmt.Printf("     Master 地址: %s\n", agentMasterAddr.Hex())
				foundInShard = true
				foundCount++
			}
		}

		if !foundInShard {
			fmt.Printf("  ⚠️  未找到匹配的 Agent 合约\n")
		}

		client.Close()
		fmt.Println()
	}

	fmt.Printf("=== 查找完成 ===\n")
	fmt.Printf("共找到 %d 个匹配的 Agent 合约\n", foundCount)
}

// shardRPC 表示一个分片的 RPC 地址
type shardRPC struct {
	ShardID uint32
	RPCURL  string
}

// parseShardRPCs 解析配置中的分片 RPC 地址
func parseShardRPCs(s string) ([]shardRPC, error) {
	if s == "" {
		return nil, fmt.Errorf("empty shard RPCs config")
	}

	parts := strings.Split(s, ",")
	result := make([]shardRPC, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid shard RPC entry: %q", p)
		}

		shardIDStr := strings.TrimSpace(kv[0])
		rpcURL := strings.TrimSpace(kv[1])

		shardID, err := parseUint32(shardIDStr)
		if err != nil {
			return nil, fmt.Errorf("invalid shard ID %q: %w", shardIDStr, err)
		}

		if rpcURL == "" {
			return nil, fmt.Errorf("empty RPC URL for shard %d", shardID)
		}

		result = append(result, shardRPC{
			ShardID: shardID,
			RPCURL:  rpcURL,
		})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no valid shard RPCs found")
	}

	return result, nil
}

func parseUint32(s string) (uint32, error) {
	var result uint64
	_, err := fmt.Sscanf(s, "%d", &result)
	if err != nil {
		return 0, err
	}
	if result > 0xFFFFFFFF {
		return 0, fmt.Errorf("value too large for uint32: %d", result)
	}
	return uint32(result), nil
}
