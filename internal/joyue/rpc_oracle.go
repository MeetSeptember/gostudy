package joyue

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/eth/rpc"
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
	clientsLock   sync.RWMutex // 保护 rpcClients 和 shardRPCs 的并发访问
	clientTimeout time.Duration
	privateKey    *ecdsa.PrivateKey // 用于发送跨分片交易的私钥
	sendLock      sync.Mutex        // 保护 SendTransaction 的并发访问，避免同时进行多个 RPC 调用导致数据库并发访问问题
	relayer       *Relayer          // Relayer 服务（Event + Relayer 方案）
}

var (
	globalRpcOracle *RpcOracle
	rpcOracleLock   sync.RWMutex
)

// NewRpcOracle 创建 RPC Oracle（节点内部使用）
func NewRpcOracle(nodeConfig *nodeconfig.ConfigType) (*RpcOracle, error) {
	oracle := &RpcOracle{
		nodeConfig:    nodeConfig,
		shardRPCs:     make(map[uint32]string),
		rpcClients:    make(map[uint32]*ethclient.Client),
		clientTimeout: 30 * time.Second, // 增加超时时间，避免在 EVM 执行过程中 RPC 调用超时
	}

	// 加载私钥（用于发送跨分片交易）
	if nodeConfig.Joyue.DeployPrivateKey != "" {
		privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(nodeConfig.Joyue.DeployPrivateKey, "0x"))
		if err != nil {
			utils.Logger().Warn().
				Err(err).
				Msg("[JOYUE] failed to parse DeployPrivateKey, cross-shard write will be disabled")
		} else {
			oracle.privateKey = privateKey
			utils.Logger().Info().
				Str("address", crypto.PubkeyToAddress(privateKey.PublicKey).Hex()).
				Msg("[JOYUE] loaded private key for cross-shard transactions")
		}
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

	// 注意：Relayer 现在是独立运行的模块，不再在节点内部自动启动
	// 使用 cmd/joyue-relayer/main.go 独立运行 Relayer
	// 这样可以：
	// 1. 解耦：Relayer 独立运行，不依赖节点内部实现
	// 2. 灵活性：可以部署多个 Relayer，或者只部署一个
	// 3. 可维护性：Relayer 可以独立更新，不影响节点
	utils.Logger().Info().
		Msg("[JOYUE] RPC Oracle created (Relayer should be run separately via cmd/joyue-relayer)")

	return oracle, nil
}

// getClient 获取或创建指定分片的 RPC 客户端
// 注意：所有 map 访问都在锁保护下，避免并发 map 读写问题
func (ro *RpcOracle) getClient(shardID uint32) (*ethclient.Client, error) {
	// 先尝试读取（读锁保护 rpcClients 和 shardRPCs 的读取）
	ro.clientsLock.RLock()
	client, exists := ro.rpcClients[shardID]
	rpcURL, rpcExists := ro.shardRPCs[shardID]
	ro.clientsLock.RUnlock()

	if exists && client != nil {
		return client, nil
	}

	// 检查 RPC URL 是否存在（在锁保护下已读取）
	if !rpcExists {
		return nil, fmt.Errorf("RPC URL not configured for shard %d", shardID)
	}

	// 需要创建客户端，使用写锁保护整个创建过程
	ro.clientsLock.Lock()
	defer ro.clientsLock.Unlock()

	// 双重检查：可能在等待锁的过程中，其他 goroutine 已经创建了客户端
	if client, exists := ro.rpcClients[shardID]; exists && client != nil {
		return client, nil
	}

	// 再次检查 RPC URL（在写锁保护下）
	rpcURL, rpcExists = ro.shardRPCs[shardID]
	if !rpcExists {
		return nil, fmt.Errorf("RPC URL not configured for shard %d", shardID)
	}

	// 创建新的客户端
	ctx, cancel := context.WithTimeout(context.Background(), ro.clientTimeout)
	defer cancel()

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to shard %d RPC %s: %w", shardID, rpcURL, err)
	}

	// 缓存客户端（在写锁保护下）
	ro.rpcClients[shardID] = client

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

// txParams 交易参数
type txParams struct {
	chainID  *big.Int
	nonce    uint64
	gasPrice *big.Int
	gasLimit uint64
}

// prepareTransactionParams 准备交易参数（获取链ID、nonce、gas price等）
func (ro *RpcOracle) prepareTransactionParams(ctx context.Context, client *ethclient.Client, from common.Address) (*txParams, error) {
	// 获取链 ID
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get chain ID: %w", err)
	}

	// 获取 nonce
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("failed to get nonce: %w", err)
	}

	// 获取 gas price
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get gas price: %w", err)
	}

	// 跳过 EstimateGas，直接使用默认值
	// 原因：
	// 1. EstimateGas 会模拟执行交易，触发 Executor.executeAndCallback()
	// 2. 这会发送回调，但 requestId 还未注册，导致 "unknown requestId" 错误
	// 3. EstimateGas 使用二分查找，会多次模拟执行，产生大量重复日志
	// 4. 跨分片场景下 EstimateGas 不准确，因为会触发真实的跨分片调用
	const defaultGasLimit = uint64(500000) // 使用默认值，确保复杂调用有足够 gas

	return &txParams{
		chainID:  chainID,
		nonce:    nonce,
		gasPrice: gasPrice,
		gasLimit: defaultGasLimit,
	}, nil
}

// buildAndSignTransaction 构建并签名交易
func (ro *RpcOracle) buildAndSignTransaction(params *txParams, shardID uint32, to common.Address, calldata []byte, value *big.Int) (*types.Transaction, []byte, error) {
	// 通过 RPC 发送到目标分片时，需要使用同分片交易（shardID == toShardID == targetShardID）
	// 因为 AddPendingTransaction 会检查 tx.ShardID() == node.ShardID
	// 虽然这是通过 RPC 发送的"跨分片"调用，但交易本身在目标分片上是同分片的
	tx := types.NewTransaction(params.nonce, to, shardID, value, params.gasLimit, params.gasPrice, calldata)

	// 签名交易
	signer := types.NewEIP155Signer(params.chainID)
	signedTx, err := types.SignTx(tx, signer, ro.privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to sign transaction: %w", err)
	}

	// 将 Harmony 交易编码为 RLP
	encodedTx, err := rlp.EncodeToBytes(signedTx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode transaction: %w", err)
	}

	return signedTx, encodedTx, nil
}

// sendRawTransactionViaRPC 通过 RPC 发送原始交易
func (ro *RpcOracle) sendRawTransactionViaRPC(ctx context.Context, shardID uint32, encodedTx []byte) (common.Hash, error) {
	// 获取 RPC URL
	ro.clientsLock.RLock()
	rpcURL, exists := ro.shardRPCs[shardID]
	ro.clientsLock.RUnlock()
	if !exists {
		return common.Hash{}, fmt.Errorf("RPC URL not configured for shard %d", shardID)
	}

	// 创建 RPC 客户端
	rpcClient, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to dial RPC: %w", err)
	}
	defer rpcClient.Close()

	// 发送交易
	var txHash common.Hash
	err = rpcClient.CallContext(ctx, &txHash, "hmy_sendRawTransaction", hexutil.Bytes(encodedTx))
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to send transaction via RPC: %w", err)
	}

	return txHash, nil
}

// SendTransaction 通过 RPC 发送交易到其他分片（跨分片写操作）
// 注意：使用互斥锁保护，避免在 EVM 执行过程中同时进行多个 RPC 调用导致数据库并发访问问题
func (ro *RpcOracle) SendTransaction(shardID uint32, to common.Address, calldata []byte, value *big.Int) (common.Hash, error) {
	// 检查私钥配置
	if ro.privateKey == nil {
		return common.Hash{}, fmt.Errorf("private key not configured, cannot send cross-shard transaction")
	}

	// 使用互斥锁保护整个发送过程，避免并发 RPC 调用导致数据库并发访问问题
	// 这会导致跨分片调用串行化，但可以避免 LevelDB 并发 map 读写错误
	ro.sendLock.Lock()
	defer ro.sendLock.Unlock()

	// 创建统一的 Context
	ctx, cancel := context.WithTimeout(context.Background(), ro.clientTimeout)
	defer cancel()

	// 获取 RPC 客户端
	client, err := ro.getClient(shardID)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to get RPC client for shard %d: %w", shardID, err)
	}

	// 获取发送者地址
	from := crypto.PubkeyToAddress(ro.privateKey.PublicKey)

	// 准备交易参数
	params, err := ro.prepareTransactionParams(ctx, client, from)
	if err != nil {
		return common.Hash{}, err
	}

	utils.Logger().Debug().
		Uint32("shardID", shardID).
		Str("to", to.Hex()).
		Uint64("gasLimit", params.gasLimit).
		Msg("[JOYUE] using default gas limit (EstimateGas skipped for cross-shard calls)")

	// 构建并签名交易
	signedTx, encodedTx, err := ro.buildAndSignTransaction(params, shardID, to, calldata, value)
	if err != nil {
		return common.Hash{}, err
	}

	// 通过 RPC 发送交易（忽略返回的 txHash，使用 signedTx.Hash() 更可靠）
	_, err = ro.sendRawTransactionViaRPC(ctx, shardID, encodedTx)
	if err != nil {
		return common.Hash{}, err
	}

	// 记录成功日志
	utils.Logger().Info().
		Str("txHash", signedTx.Hash().Hex()).
		Uint32("shardID", shardID).
		Str("to", to.Hex()).
		Uint64("nonce", params.nonce).
		Msg("[JOYUE] sent cross-shard transaction via RPC")

	return signedTx.Hash(), nil
}

// SendTransactionRequest 发送交易请求结构
type SendTransactionRequest struct {
	ShardID  uint32
	To       common.Address
	Calldata []byte
	Value    *big.Int
}

// SendTransactionBatch 批量串行发送多个跨分片交易
// 注意：串行发送以避免并发 map 读写问题
// 返回：所有交易哈希数组（失败的位置为 zero hash），以及成功发送的数量
func (ro *RpcOracle) SendTransactionBatch(requests []SendTransactionRequest) ([]common.Hash, uint32, error) {
	if ro.privateKey == nil {
		return nil, 0, fmt.Errorf("private key not configured, cannot send cross-shard transaction")
	}

	if len(requests) == 0 {
		return nil, 0, nil
	}

	// 串行发送所有请求（避免并发 map 读写问题）
	txHashes := make([]common.Hash, len(requests))
	successCount := uint32(0)

	for i, req := range requests {
		txHash, err := ro.SendTransaction(req.ShardID, req.To, req.Calldata, req.Value)
		if err != nil {
			utils.Logger().Warn().
				Err(err).
				Int("index", i).
				Uint32("shardID", req.ShardID).
				Str("to", req.To.Hex()).
				Msg("[JOYUE] failed to send transaction in batch")
			// txHashes[i] 保持为零值（zero hash）
		} else {
			txHashes[i] = txHash
			successCount++
		}
	}

	utils.Logger().Info().
		Int("total", len(requests)).
		Uint32("success", successCount).
		Msg("[JOYUE] batch sent cross-shard transactions (serial)")

	return txHashes, successCount, nil
}

// SetGlobalRpcOracle 设置全局 RPC Oracle（供 precompile 使用）
func SetGlobalRpcOracle(oracle *RpcOracle) {
	rpcOracleLock.Lock()
	defer rpcOracleLock.Unlock()
	globalRpcOracle = oracle

	// 注意：Relayer 现在是独立运行的模块，不再在节点内部自动启动
	// 使用 cmd/joyue-relayer/main.go 独立运行 Relayer
	if oracle == nil {
		utils.Logger().Warn().
			Msg("[JOYUE] SetGlobalRpcOracle called with nil oracle")
		return
	}

	utils.Logger().Info().
		Msg("[JOYUE] RPC Oracle set (Relayer should be run separately via cmd/joyue-relayer)")
}

// GetGlobalRpcOracle 获取全局 RPC Oracle
func GetGlobalRpcOracle() *RpcOracle {
	rpcOracleLock.RLock()
	defer rpcOracleLock.RUnlock()
	return globalRpcOracle
}
