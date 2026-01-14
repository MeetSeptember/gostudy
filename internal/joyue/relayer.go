package joyue

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/harmony-one/harmony/crypto/hash"
	"github.com/harmony-one/harmony/internal/utils"
)

/*
JOYUE Relayer 服务

功能：监听 CrossShardRequest 事件并发送跨分片交易

流程：
1. 监听链上 CrossShardRequest 事件
2. 解析事件数据
3. 发送交易到目标分片
4. 监听交易回执
5. 调用回调合约
*/

// CrossShardRequestEvent 跨分片请求事件
type CrossShardRequestEvent struct {
	RequestID        uint64         // 请求 ID
	TargetShardID    uint32         // 目标分片 ID
	Target           common.Address // 目标合约地址
	Calldata         []byte         // 调用数据
	Value            *big.Int       // 转账金额
	CallbackAddr     common.Address // 回调合约地址
	CallbackSelector [4]byte        // 回调函数 selector
	TxHash           common.Hash    // 触发事件的交易哈希
	BlockNumber      uint64         // 区块号
	LogIndex         uint           // 日志索引
}

// Relayer 跨分片交易 Relayer 服务
type Relayer struct {
	rpcOracle       *RpcOracle
	ethClient       *ethclient.Client
	rpcURL          string
	shardID         uint32
	eventSignature  common.Hash          // CrossShardRequest 事件签名
	processedEvents map[common.Hash]bool // 已处理的事件（避免重复处理）
	processedLock   sync.RWMutex
	stopChan        chan struct{}
	running         bool
	runningLock     sync.Mutex
}

// NewRelayer 创建新的 Relayer 服务（节点内部使用）
func NewRelayer(rpcOracle *RpcOracle, rpcURL string, shardID uint32) (*Relayer, error) {
	// 连接到 RPC 节点
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RPC: %w", err)
	}

	// 计算事件签名
	eventSignature := hash.Keccak256Hash([]byte("CrossShardRequest(uint256,uint32,address,bytes,uint256,address,bytes4)"))

	return &Relayer{
		rpcOracle:       rpcOracle,
		ethClient:       client,
		rpcURL:          rpcURL,
		shardID:         shardID,
		eventSignature:  eventSignature,
		processedEvents: make(map[common.Hash]bool),
		stopChan:        make(chan struct{}),
		running:         false,
	}, nil
}

// NewStandaloneRelayer 创建独立的 Relayer 服务（不依赖节点配置，使用已创建的 ethClient）
func NewStandaloneRelayer(rpcOracle *RpcOracle, ethClient *ethclient.Client, rpcURL string, shardID uint32) (*Relayer, error) {
	// 计算事件签名
	eventSignature := hash.Keccak256Hash([]byte("CrossShardRequest(uint256,uint32,address,bytes,uint256,address,bytes4)"))

	return &Relayer{
		rpcOracle:       rpcOracle,
		ethClient:       ethClient,
		rpcURL:          rpcURL,
		shardID:         shardID,
		eventSignature:  eventSignature,
		processedEvents: make(map[common.Hash]bool),
		stopChan:        make(chan struct{}),
		running:         false,
	}, nil
}

// Start 启动 Relayer 服务（从最新区块开始）
func (r *Relayer) Start(ctx context.Context) error {
	return r.StartFromBlock(ctx, 0, 2*time.Second)
}

// StartFromBlock 启动 Relayer 服务（从指定区块开始）
func (r *Relayer) StartFromBlock(ctx context.Context, fromBlock uint64, pollInterval time.Duration) error {
	r.runningLock.Lock()
	defer r.runningLock.Unlock()

	if r.running {
		return fmt.Errorf("relayer is already running")
	}

	r.running = true
	r.stopChan = make(chan struct{})

	// 启动事件监听 goroutine
	go r.eventLoopWithConfig(ctx, fromBlock, pollInterval)

	utils.Logger().Info().
		Str("rpcURL", r.rpcURL).
		Uint32("shardID", r.shardID).
		Uint64("fromBlock", fromBlock).
		Dur("pollInterval", pollInterval).
		Msg("[JOYUE] Relayer started")

	return nil
}

// Stop 停止 Relayer 服务
func (r *Relayer) Stop() {
	r.runningLock.Lock()
	defer r.runningLock.Unlock()

	if !r.running {
		return
	}

	r.running = false
	close(r.stopChan)

	utils.Logger().Info().
		Msg("[JOYUE] Relayer stopped")
}

// eventLoop 事件监听循环（使用默认配置）
func (r *Relayer) eventLoop(ctx context.Context) {
	r.eventLoopWithConfig(ctx, 0, 2*time.Second)
}

// eventLoopWithConfig 事件监听循环（可配置）
func (r *Relayer) eventLoopWithConfig(ctx context.Context, fromBlock uint64, pollInterval time.Duration) {
	// Precompile 地址（0x6D = 109）
	precompileAddr := common.BytesToAddress([]byte{109})

	// 查询过滤器配置
	query := ethereum.FilterQuery{
		Addresses: []common.Address{precompileAddr},
		Topics: [][]common.Hash{
			{r.eventSignature}, // 事件签名
		},
	}

	// 获取最新区块号
	latestBlock, err := r.ethClient.BlockNumber(ctx)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Msg("[JOYUE] failed to get latest block number")
		return
	}

	// 如果 fromBlock 为 0，从最新区块开始
	if fromBlock == 0 {
		fromBlock = latestBlock
		if fromBlock > 100 {
			fromBlock -= 100 // 回退 100 个区块，避免遗漏
		}
		utils.Logger().Info().
			Uint64("fromBlock", fromBlock).
			Uint64("latestBlock", latestBlock).
			Msg("[JOYUE] Relayer starting event loop (auto-detect fromBlock)")
	} else {
		utils.Logger().Info().
			Uint64("fromBlock", fromBlock).
			Uint64("latestBlock", latestBlock).
			Msg("[JOYUE] Relayer starting event loop (manual fromBlock)")
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopChan:
			return
		case <-ticker.C:
			// 获取最新区块号
			currentBlock, err := r.ethClient.BlockNumber(ctx)
			if err != nil {
				utils.Logger().Error().
					Err(err).
					Msg("[JOYUE] failed to get current block number")
				continue
			}

			if currentBlock <= fromBlock {
				continue // 没有新区块
			}

			// 查询事件
			query.FromBlock = big.NewInt(int64(fromBlock))
			query.ToBlock = big.NewInt(int64(currentBlock))

			logs, err := r.ethClient.FilterLogs(ctx, query)
			if err != nil {
				utils.Logger().Error().
					Err(err).
					Msg("[JOYUE] failed to filter logs")
				continue
			}

			// 处理事件
			for _, log := range logs {
				r.processEvent(ctx, log)
			}

			// 更新 fromBlock
			fromBlock = currentBlock + 1
		}
	}
}

// processEvent 处理单个事件
func (r *Relayer) processEvent(ctx context.Context, log types.Log) {
	// 检查是否已处理
	indexHash := common.BigToHash(big.NewInt(int64(log.Index)))
	eventKey := hash.Keccak256Hash(log.TxHash.Bytes(), indexHash.Bytes())
	r.processedLock.RLock()
	if r.processedEvents[eventKey] {
		r.processedLock.RUnlock()
		return
	}
	r.processedLock.RUnlock()

	// 解析事件
	event, err := r.parseEvent(log)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("txHash", log.TxHash.Hex()).
			Msg("[JOYUE] failed to parse CrossShardRequest event")
		return
	}

	// 标记为已处理
	r.processedLock.Lock()
	r.processedEvents[eventKey] = true
	r.processedLock.Unlock()

	utils.Logger().Info().
		Uint64("requestId", event.RequestID).
		Uint32("targetShardID", event.TargetShardID).
		Str("target", event.Target.Hex()).
		Str("txHash", log.TxHash.Hex()).
		Msg("[JOYUE] Relayer processing CrossShardRequest event")

	// 发送交易到目标分片
	go r.sendTransaction(ctx, event)
}

// parseEvent 解析 CrossShardRequest 事件
func (r *Relayer) parseEvent(log types.Log) (*CrossShardRequestEvent, error) {
	// 验证事件签名
	if len(log.Topics) < 2 {
		return nil, fmt.Errorf("invalid event: insufficient topics")
	}
	if log.Topics[0] != r.eventSignature {
		return nil, fmt.Errorf("invalid event signature")
	}

	// Topics[1] = requestId (indexed)
	requestID := new(big.Int).SetBytes(log.Topics[1][:]).Uint64()

	// 解析 Data
	// 格式：shardID (4 bytes) + target (20 bytes) + calldataLen (4 bytes) + calldata + value (32 bytes) + callbackAddr (20 bytes) + callbackSelector (4 bytes)
	data := log.Data
	if len(data) < 4+20+4+32+20+4 {
		return nil, fmt.Errorf("invalid event data length")
	}

	offset := 0
	targetShardID := binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	target := common.BytesToAddress(data[offset : offset+20])
	offset += 20

	calldataLen := binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	if len(data) < offset+int(calldataLen)+32+20+4 {
		return nil, fmt.Errorf("invalid event data: calldata length mismatch")
	}

	calldata := make([]byte, calldataLen)
	copy(calldata, data[offset:offset+int(calldataLen)])
	offset += int(calldataLen)

	value := new(big.Int).SetBytes(data[offset : offset+32])
	offset += 32

	callbackAddr := common.BytesToAddress(data[offset : offset+20])
	offset += 20

	var callbackSelector [4]byte
	copy(callbackSelector[:], data[offset:offset+4])

	return &CrossShardRequestEvent{
		RequestID:        requestID,
		TargetShardID:    targetShardID,
		Target:           target,
		Calldata:         calldata,
		Value:            value,
		CallbackAddr:     callbackAddr,
		CallbackSelector: callbackSelector,
		TxHash:           log.TxHash,
		BlockNumber:      log.BlockNumber,
		LogIndex:         log.Index,
	}, nil
}

// sendTransaction 发送交易到目标分片
// 注意：Relayer 只负责发送交易，不负责回调
// 回调由目标分片的 executor 通过 Precompile 发出事件，由目标分片的 Relayer 处理
func (r *Relayer) sendTransaction(ctx context.Context, event *CrossShardRequestEvent) {
	// 使用 RpcOracle 发送交易
	txHash, err := r.rpcOracle.SendTransaction(
		event.TargetShardID,
		event.Target,
		event.Calldata,
		event.Value,
	)

	if err != nil {
		utils.Logger().Error().
			Err(err).
			Uint64("requestId", event.RequestID).
			Uint32("targetShardID", event.TargetShardID).
			Str("target", event.Target.Hex()).
			Msg("[JOYUE] Relayer failed to send transaction")

		// 发送失败，无法回调（因为回调需要通过目标分片的 executor）
		// 这里可以记录错误，或者通过其他机制通知
		// 注意：如果发送失败，executor 不会执行，也就不会发出回调事件
		// 源分片的合约需要自己处理超时或失败的情况
		return
	}

	utils.Logger().Info().
		Str("txHash", txHash.Hex()).
		Uint64("requestId", event.RequestID).
		Uint32("targetShardID", event.TargetShardID).
		Str("target", event.Target.Hex()).
		Msg("[JOYUE] Relayer sent transaction successfully")

	// 不再等待回执和直接回调
	// executor 执行完成后会通过 Precompile 发出回调事件
	// 回调事件会由目标分片的 Relayer 监听并处理
}

// 注意：waitForReceipt 和 sendCallback 已移除
// 回调流程由 executor 通过 Precompile 发出事件，由目标分片的 Relayer 处理
// 这样更符合去中心化的设计：每个分片的 Relayer 只处理自己分片的事件
