package joyue

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
)

/*
JOYUE RPC Oracle 模块

功能：通过 RPC 查询其他分片的主合约权威状态（非缓存）

注意：当前实现不考虑安全性，仅实现基本功能
*/

// RpcOracle RPC Oracle 管理器
type RpcOracle struct {
	nodeConfig    *nodeconfig.ConfigType
	shardRPCs     map[uint32]string // shardID -> RPC URL
	rpcClients    map[uint32]*ethclient.Client
	clientsLock   sync.RWMutex
	clientTimeout time.Duration
}

var (
	globalRpcOracle *RpcOracle
	rpcOracleLock   sync.RWMutex
)

// NewRpcOracle 创建 RPC Oracle
func NewRpcOracle(nodeConfig *nodeconfig.ConfigType) (*RpcOracle, error) {
	oracle := &RpcOracle{
		nodeConfig:    nodeConfig,
		shardRPCs:     make(map[uint32]string),
		rpcClients:    make(map[uint32]*ethclient.Client),
		clientTimeout: 5 * time.Second,
	}

	// 解析 OtherShardRPCs 配置
	// 格式：shardID=rpcURL,shardID=rpcURL
	// 例如：0=http://127.0.0.1:9500,1=http://127.0.0.1:9502
	if nodeConfig.Joyue.OtherShardRPCs != "" {
		pairs := strings.Split(nodeConfig.Joyue.OtherShardRPCs, ",")
		for _, pair := range pairs {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) != 2 {
				utils.Logger().Warn().
					Str("pair", pair).
					Msg("[JOYUE] invalid RPC pair format, skipping")
				continue
			}
			var shardID uint32
			if _, err := fmt.Sscanf(parts[0], "%d", &shardID); err != nil {
				utils.Logger().Warn().
					Str("pair", pair).
					Err(err).
					Msg("[JOYUE] failed to parse shard ID, skipping")
				continue
			}
			rpcURL := strings.TrimSpace(parts[1])
			oracle.shardRPCs[shardID] = rpcURL
		}
	}

	// 添加当前分片的 RPC（如果配置了）
	currentShardID := nodeConfig.ShardID
	if nodeConfig.RPCServer.HTTPEnabled {
		rpcURL := fmt.Sprintf("http://%s:%d", nodeConfig.RPCServer.HTTPIp, nodeConfig.RPCServer.HTTPPort)
		oracle.shardRPCs[currentShardID] = rpcURL
	}

	return oracle, nil
}

// getClient 获取或创建指定分片的 RPC 客户端
func (ro *RpcOracle) getClient(shardID uint32) (*ethclient.Client, error) {
	ro.clientsLock.RLock()
	client, exists := ro.rpcClients[shardID]
	ro.clientsLock.RUnlock()

	if exists && client != nil {
		return client, nil
	}

	// 获取 RPC URL
	rpcURL, exists := ro.shardRPCs[shardID]
	if !exists {
		return nil, fmt.Errorf("RPC URL not configured for shard %d", shardID)
	}

	// 创建新的客户端
	ctx, cancel := context.WithTimeout(context.Background(), ro.clientTimeout)
	defer cancel()

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to shard %d RPC %s: %w", shardID, rpcURL, err)
	}

	// 缓存客户端
	ro.clientsLock.Lock()
	ro.rpcClients[shardID] = client
	ro.clientsLock.Unlock()

	return client, nil
}

// QueryContractState 通过 RPC 查询其他分片的主合约状态
// 返回：value (bytes), version (uint64), ok (bool)
func (ro *RpcOracle) QueryContractState(shardID uint32, contractAddr common.Address, key common.Hash) ([]byte, uint64, bool) {
	// 获取 RPC 客户端
	client, err := ro.getClient(shardID)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Uint32("shard", shardID).
			Str("contract", contractAddr.Hex()).
			Msg("[JOYUE] failed to get RPC client")
		return nil, 0, false
	}

	// 通过 eth_getStorageAt 查询状态
	// 注意：这里假设 key 对应的是 storage slot
	// 如果需要查询合约的 getter 函数，需要使用 eth_call
	ctx, cancel := context.WithTimeout(context.Background(), ro.clientTimeout)
	defer cancel()

	state, err := client.StorageAt(ctx, contractAddr, key, nil)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Uint32("shard", shardID).
			Str("contract", contractAddr.Hex()).
			Str("key", key.Hex()).
			Msg("[JOYUE] failed to query contract state via RPC")
		return nil, 0, false
	}

	// 注意：eth_getStorageAt 只返回 value，不返回 version
	// 如果需要 version，可能需要调用合约的 getter 函数
	// 这里先返回 value，version 设为 0（表示未知）
	return state, 0, true
}

// QueryContractGetter 通过 RPC 调用合约的 getter 函数
// 用于查询合约的公开状态变量或函数返回值
func (ro *RpcOracle) QueryContractGetter(shardID uint32, contractAddr common.Address, calldata []byte) ([]byte, error) {
	// 获取 RPC 客户端
	client, err := ro.getClient(shardID)
	if err != nil {
		return nil, fmt.Errorf("failed to get RPC client for shard %d: %w", shardID, err)
	}

	// 通过 eth_call 调用合约函数
	ctx, cancel := context.WithTimeout(context.Background(), ro.clientTimeout)
	defer cancel()

	msg := ethereum.CallMsg{
		To:   &contractAddr,
		Data: calldata,
	}

	result, err := client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to call contract on shard %d: %w", shardID, err)
	}

	return result, nil
}

// SetGlobalRpcOracle 设置全局 RPC Oracle（供 precompile 使用）
func SetGlobalRpcOracle(oracle *RpcOracle) {
	rpcOracleLock.Lock()
	defer rpcOracleLock.Unlock()
	globalRpcOracle = oracle
}

// GetGlobalRpcOracle 获取全局 RPC Oracle
func GetGlobalRpcOracle() *RpcOracle {
	rpcOracleLock.RLock()
	defer rpcOracleLock.RUnlock()
	return globalRpcOracle
}
