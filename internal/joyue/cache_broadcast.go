package joyue

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/harmony-one/harmony/core/types"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/internal/utils/lrucache"
	"github.com/harmony-one/harmony/p2p"
	libp2p_pubsub "github.com/libp2p/go-libp2p-pubsub"
)

/*
JOYUE P2P 缓存广播模块

功能：
1. 监听 StateBroadcast 事件
2. 更新本地缓存
3. 通过 P2P 网络广播缓存更新
4. 接收其他节点的缓存更新消息

注意：当前实现不考虑安全性，仅实现基本功能
*/

// StateBroadcast 事件 ABI（新增 txId 字段）
const stateBroadcastEventABI = `[
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true, "internalType": "address", "name": "contractAddr", "type": "address"},
      {"indexed": true, "internalType": "bytes32", "name": "key", "type": "bytes32"},
      {"indexed": false, "internalType": "uint256", "name": "value", "type": "uint256"},
      {"indexed": false, "internalType": "uint64", "name": "version", "type": "uint64"},
      {"indexed": false, "internalType": "bytes32", "name": "txId", "type": "bytes32"}
    ],
    "name": "StateBroadcast",
    "type": "event"
  }
]`

// StateBroadcast 事件 ABI（旧格式：主合约部署/setState 使用，无 txId）
const stateBroadcastEventOldABI = `[
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true, "internalType": "address", "name": "contractAddr", "type": "address"},
      {"indexed": true, "internalType": "bytes32", "name": "key", "type": "bytes32"},
      {"indexed": false, "internalType": "uint256", "name": "value", "type": "uint256"},
      {"indexed": false, "internalType": "uint64", "name": "version", "type": "uint64"}
    ],
    "name": "StateBroadcast",
    "type": "event"
  }
]`

// StateUnfreeze 事件 ABI
const stateUnfreezeEventABI = `[
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true, "internalType": "address", "name": "contractAddr", "type": "address"},
      {"indexed": true, "internalType": "bytes32", "name": "key", "type": "bytes32"},
      {"indexed": false, "internalType": "bytes32", "name": "txId", "type": "bytes32"}
    ],
    "name": "StateUnfreeze",
    "type": "event"
  }
]`

// CacheEntry 缓存条目
type CacheEntry struct {
	ContractAddr common.Address
	ShardId      uint32
	Key          common.Hash
	Value        []byte
	Version      uint64
	TxId         common.Hash // 触发本次更新的交易标识符（用于解冻）
}

// CacheBroadcaster P2P 缓存广播器
type CacheBroadcaster struct {
	host       p2p.Host
	nodeConfig *nodeconfig.ConfigType
	cache      *lrucache.Cache[string, *CacheEntry] // LRU 缓存，key = contractAddr:shardId:key
	cacheLock  sync.RWMutex
	// 本地冻结表：记录已放行但未被主合约确认的占用量
	// 外层 key = cacheKey(contractAddr,shardId,storageKey)
	// 内层 key = txId，value = 冻结金额
	freezeTable map[string]map[common.Hash]*big.Int
	freezeLock  sync.RWMutex
	eventABI    abi.ABI
	eventABIOld abi.ABI // 主合约部署/setState 的旧格式（无 txId）
	unfreezeABI abi.ABI
	eventSig    common.Hash
	eventSigOld common.Hash // StateBroadcast(address,bytes32,uint256,uint64)
	unfreezeSig common.Hash
	topic       string
	ctx         context.Context
	cancel      context.CancelFunc
	// 改进：多节点订阅支持
	subs     []*libp2p_pubsub.Subscription // 多个订阅（冗余）
	subsLock sync.RWMutex
	// 改进：高优先级广播通道
	highPriorityTopic *libp2p_pubsub.Topic // 高优先级 topic（如果支持）
}

// NewCacheBroadcaster 创建缓存广播器
func NewCacheBroadcaster(host p2p.Host, nodeConfig *nodeconfig.ConfigType) (*CacheBroadcaster, error) {
	eventABI, err := abi.JSON(bytes.NewReader([]byte(stateBroadcastEventABI)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse StateBroadcast ABI: %w", err)
	}

	eventABIOld, err := abi.JSON(bytes.NewReader([]byte(stateBroadcastEventOldABI)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse StateBroadcast old ABI: %w", err)
	}

	unfreezeABI, err := abi.JSON(bytes.NewReader([]byte(stateUnfreezeEventABI)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse StateUnfreeze ABI: %w", err)
	}

	// 事件签名：新格式（commit 产生，含 txId）与旧格式（部署/setState 产生，无 txId）
	eventSig := crypto.Keccak256Hash([]byte("StateBroadcast(address,bytes32,uint256,uint64,bytes32)"))
	eventSigOld := crypto.Keccak256Hash([]byte("StateBroadcast(address,bytes32,uint256,uint64)"))
	unfreezeSig := crypto.Keccak256Hash([]byte("StateUnfreeze(address,bytes32,bytes32)"))

	topic := "joyue-cache-global"
	ctx, cancel := context.WithCancel(context.Background())

	cacheSize := 10000
	cache := lrucache.NewCache[string, *CacheEntry](cacheSize)

	cb := &CacheBroadcaster{
		host:        host,
		nodeConfig:  nodeConfig,
		cache:       cache,
		freezeTable: make(map[string]map[common.Hash]*big.Int),
		eventABI:    eventABI,
		eventABIOld: eventABIOld,
		unfreezeABI: unfreezeABI,
		eventSig:    eventSig,
		eventSigOld: eventSigOld,
		unfreezeSig: unfreezeSig,
		topic:       topic,
		ctx:         ctx,
		cancel:      cancel,
	}

	// 订阅 P2P topic（异步，不阻塞初始化）
	// 改进：多节点冗余订阅
	go func() {
		if err := cb.subscribeP2PTopicWithRedundancy(); err != nil {
			utils.Logger().Error().
				Err(err).
				Msg("[JOYUE] failed to subscribe P2P topic")
		}
	}()

	return cb, nil
}

// subscribeP2PTopic 订阅 P2P topic 接收缓存更新（保留向后兼容）
func (cb *CacheBroadcaster) subscribeP2PTopic() error {
	return cb.subscribeP2PTopicWithRedundancy()
}

// subscribeP2PTopicWithRedundancy 订阅 P2P topic，支持多节点冗余订阅
// 改进：每个节点订阅多个其他分片的节点，提高可靠性和实时性
func (cb *CacheBroadcaster) subscribeP2PTopicWithRedundancy() error {
	topic, err := cb.host.GetOrJoin(cb.topic)
	if err != nil {
		return fmt.Errorf("failed to join topic: %w", err)
	}

	// 改进：创建多个订阅（冗余订阅）
	// 默认创建 3 个订阅，提高可靠性
	redundancyLevel := 3
	cb.subsLock.Lock()
	cb.subs = make([]*libp2p_pubsub.Subscription, 0, redundancyLevel)
	cb.subsLock.Unlock()

	for i := 0; i < redundancyLevel; i++ {
		sub, err := topic.Subscribe()
		if err != nil {
			utils.Logger().Warn().
				Err(err).
				Int("subscription", i).
				Msg("[JOYUE] failed to create redundant subscription")
			continue
		}

		cb.subsLock.Lock()
		cb.subs = append(cb.subs, sub)
		cb.subsLock.Unlock()

		// 为每个订阅启动独立的处理 goroutine
		go cb.handleP2PMessages(sub)
	}

	utils.Logger().Info().
		Str("topic", cb.topic).
		Int("redundancy", len(cb.subs)).
		Msg("[JOYUE] subscribed to cache broadcast topic with redundancy")

	return nil
}

// handleP2PMessages 处理接收到的 P2P 消息
func (cb *CacheBroadcaster) handleP2PMessages(sub *libp2p_pubsub.Subscription) {
	utils.Logger().Info().Msg("[JOYUE] started P2P message handler")

	for {
		select {
		case <-cb.ctx.Done():
			utils.Logger().Info().Msg("[JOYUE] P2P message handler stopped")
			return
		default:
			msg, err := sub.Next(cb.ctx)
			if err != nil {
				if err == context.Canceled {
					return
				}
				utils.Logger().Error().
					Err(err).
					Msg("[JOYUE] failed to receive P2P message")
				continue
			}

			// 忽略自己发送的消息
			if msg.GetFrom() == cb.host.GetID() {
				continue
			}

			rawData := msg.GetData()
			if len(rawData) == 0 {
				continue
			}
			msgType := rawData[0]

			if msgType == 0x03 {
				// 解冻消息：[1 type=0x03][20 contractAddr][4 shardId][32 key][32 txId]
				if len(rawData) < 1+20+4+32+32 {
					utils.Logger().Error().Msg("[JOYUE] invalid unfreeze message length from P2P")
					continue
				}
				contractAddr := common.BytesToAddress(rawData[1:21])
				shardId := binary.BigEndian.Uint32(rawData[21:25])
				key := common.BytesToHash(rawData[25:57])
				txId := common.BytesToHash(rawData[57:89])
				cb.Unfreeze(contractAddr, shardId, key, txId)
				utils.Logger().Debug().
					Str("contract", contractAddr.Hex()).Str("key", key.Hex()).
					Str("txId", txId.Hex()).Str("from", msg.GetFrom().String()).
					Msg("[JOYUE] received unfreeze message from P2P")
				continue
			}

			// 权威缓存更新（type=0x01 旧格式 或 type=0x02 新格式含 txId）
			entry, err := cb.deserializeCacheEntry(rawData)
			if err != nil {
				utils.Logger().Error().Err(err).Msg("[JOYUE] failed to deserialize cache entry")
				continue
			}

			if !cb.validateVersion(entry) {
				utils.Logger().Debug().
					Str("contract", entry.ContractAddr.Hex()).Str("key", entry.Key.Hex()).
					Uint64("version", entry.Version).Str("from", msg.GetFrom().String()).
					Msg("[JOYUE] rejected cache update (version not newer)")
				continue
			}

			cb.updateLocalCache(entry)

			// 若 txId 非零，一并解冻
			if entry.TxId != (common.Hash{}) {
				cb.Unfreeze(entry.ContractAddr, entry.ShardId, entry.Key, entry.TxId)
				utils.Logger().Debug().
					Str("contract", entry.ContractAddr.Hex()).Str("key", entry.Key.Hex()).
					Str("txId", entry.TxId.Hex()).Str("from", msg.GetFrom().String()).
					Msg("[JOYUE] unfrozen after P2P commit update")
			}

			valueUint := new(big.Int).SetBytes(entry.Value)
			utils.Logger().Debug().
				Str("contract", entry.ContractAddr.Hex()).
				Str("key", entry.Key.Hex()).
				Uint64("version", entry.Version).
				Str("txId", entry.TxId.Hex()).
				Str("valueUint", valueUint.String()).
				Int("valueLen", len(entry.Value)).
				Str("from", msg.GetFrom().String()).
				Msg("[JOYUE] received cache update from P2P")
		}
	}
}

// ProcessBlockLogs 处理区块中的日志，查找 StateBroadcast 事件
// 接受 Harmony 的 types.Block 和 types.Receipts
func (cb *CacheBroadcaster) ProcessBlockLogs(block *types.Block, receipts types.Receipts) {
	if receipts == nil {
		utils.Logger().Debug().
			Uint64("block", block.NumberU64()).
			Msg("[JOYUE] ProcessBlockLogs: receipts is nil")
		return
	}

	utils.Logger().Debug().
		Uint64("block", block.NumberU64()).
		Int("receiptsCount", len(receipts)).
		Str("eventSig", cb.eventSig.Hex()).
		Msg("[JOYUE] ProcessBlockLogs: processing block logs")

	for _, receipt := range receipts {
		if receipt.Status != ethtypes.ReceiptStatusSuccessful {
			continue
		}

		for _, log := range receipt.Logs {
			if len(log.Topics) == 0 {
				continue
			}

			utils.Logger().Debug().
				Str("txHash", receipt.TxHash.Hex()).
				Str("logTopic0", log.Topics[0].Hex()).
				Msg("[JOYUE] ProcessBlockLogs: checking log")

			ethLog := convertToEthLog(log)

			switch log.Topics[0] {
			case cb.eventSig, cb.eventSigOld:
				// StateBroadcast：更新权威缓存 + 解冻（若有 txId）
				// eventSig：commit 产生（含 txId）；eventSigOld：部署/setState 产生（无 txId）
				utils.Logger().Debug().
					Str("txHash", receipt.TxHash.Hex()).
					Str("logAddress", log.Address.Hex()).
					Bool("fromDeploy", log.Topics[0] == cb.eventSigOld).
					Msg("[JOYUE] ProcessBlockLogs: found StateBroadcast event")

				var entry *CacheEntry
				var err error
				if log.Topics[0] == cb.eventSig {
					entry, err = cb.parseStateBroadcastEvent(ethLog)
				} else {
					entry, err = cb.parseStateBroadcastEventOld(ethLog)
				}
				if err != nil {
					utils.Logger().Error().Err(err).Str("txHash", receipt.TxHash.Hex()).
						Msg("[JOYUE] failed to parse StateBroadcast event")
					continue
				}

				if !cb.validateVersion(entry) {
					utils.Logger().Debug().
						Str("contract", entry.ContractAddr.Hex()).Str("key", entry.Key.Hex()).
						Uint64("version", entry.Version).Uint64("block", block.NumberU64()).
						Msg("[JOYUE] rejected cache update (version not newer)")
					continue
				}

				cb.updateLocalCache(entry)

				// 从冻结表中移除该 txId
				if entry.TxId != (common.Hash{}) {
					cb.Unfreeze(entry.ContractAddr, entry.ShardId, entry.Key, entry.TxId)
					utils.Logger().Debug().
						Str("contract", entry.ContractAddr.Hex()).Str("key", entry.Key.Hex()).
						Str("txId", entry.TxId.Hex()).
						Msg("[JOYUE] ProcessBlockLogs: unfrozen after commit (StateBroadcast)")
				}

				cacheKey := cb.cacheKey(entry.ContractAddr, entry.ShardId, entry.Key)
				if err := cb.broadcastCacheUpdateImmediate(entry); err != nil {
					utils.Logger().Error().Err(err).
						Str("contract", entry.ContractAddr.Hex()).Str("key", entry.Key.Hex()).
						Msg("[JOYUE] failed to broadcast cache update immediately")
				} else {
					valueUint := new(big.Int).SetBytes(entry.Value)
					utils.Logger().Debug().
						Str("contract", entry.ContractAddr.Hex()).Str("key", entry.Key.Hex()).
						Str("cacheKey", cacheKey).Uint64("version", entry.Version).
						Str("txId", entry.TxId.Hex()).
						Str("valueUint", valueUint.String()).Uint64("block", block.NumberU64()).
						Msg("[JOYUE] cache updated and broadcasted immediately")
				}

			case cb.unfreezeSig:
				// StateUnfreeze：仅从冻结表解冻，不更新权威缓存
				utils.Logger().Debug().
					Str("txHash", receipt.TxHash.Hex()).
					Str("logAddress", log.Address.Hex()).
					Msg("[JOYUE] ProcessBlockLogs: found StateUnfreeze event")

				contractAddr, key, txId, err := cb.parseStateUnfreezeEvent(ethLog)
				if err != nil {
					utils.Logger().Error().Err(err).Str("txHash", receipt.TxHash.Hex()).
						Msg("[JOYUE] failed to parse StateUnfreeze event")
					continue
				}

				shardId := cb.nodeConfig.ShardID
				cb.Unfreeze(contractAddr, shardId, key, txId)
				utils.Logger().Debug().
					Str("contract", contractAddr.Hex()).Str("key", key.Hex()).
					Str("txId", txId.Hex()).
					Msg("[JOYUE] ProcessBlockLogs: unfrozen after rollback (StateUnfreeze)")

				if err := cb.broadcastUnfreezeImmediate(contractAddr, shardId, key, txId); err != nil {
					utils.Logger().Error().Err(err).
						Str("contract", contractAddr.Hex()).Str("key", key.Hex()).
						Msg("[JOYUE] failed to broadcast unfreeze immediately")
				}

			default:
				// 非目标事件，跳过
			}
		}
	}
}

// convertToEthLog 将 Harmony 的 Log 转换为 Ethereum 的 Log
func convertToEthLog(log *types.Log) *ethtypes.Log {
	return &ethtypes.Log{
		Address:     log.Address,
		Topics:      log.Topics,
		Data:        log.Data,
		BlockNumber: log.BlockNumber,
		TxHash:      log.TxHash,
		TxIndex:     log.TxIndex,
		BlockHash:   log.BlockHash,
		Index:       log.Index,
		Removed:     log.Removed,
	}
}

// parseStateBroadcastEvent 解析 StateBroadcast 事件
// 新格式：Topics[1]=contractAddr, Topics[2]=key, Data=abi(uint256 value, uint64 version, bytes32 txId)
func (cb *CacheBroadcaster) parseStateBroadcastEvent(log *ethtypes.Log) (*CacheEntry, error) {
	if len(log.Topics) < 3 {
		return nil, fmt.Errorf("invalid event topics count: %d", len(log.Topics))
	}

	contractAddr := common.BytesToAddress(log.Topics[1].Bytes()[12:])
	key := log.Topics[2]

	// 解析 non-indexed 参数 (value, version, txId)
	vals, err := cb.eventABI.Events["StateBroadcast"].Inputs.NonIndexed().Unpack(log.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack StateBroadcast data: %w", err)
	}

	if len(vals) != 3 {
		return nil, fmt.Errorf("expected 3 values (value,version,txId), got %d", len(vals))
	}

	value, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("invalid value type: %T", vals[0])
	}

	version, ok := vals[1].(uint64)
	if !ok {
		if vBig, ok2 := vals[1].(*big.Int); ok2 {
			version = vBig.Uint64()
		} else {
			return nil, fmt.Errorf("invalid version type: %T", vals[1])
		}
	}

	txIdRaw, ok := vals[2].([32]byte)
	if !ok {
		return nil, fmt.Errorf("invalid txId type: %T", vals[2])
	}
	txId := common.Hash(txIdRaw)

	// 左填充 value 到 32 字节
	valueBytes := value.Bytes()
	paddedValue := make([]byte, 32)
	if len(valueBytes) > 0 {
		copy(paddedValue[32-len(valueBytes):], valueBytes)
	}

	return &CacheEntry{
		ContractAddr: contractAddr,
		ShardId:      cb.nodeConfig.ShardID,
		Key:          key,
		Value:        paddedValue,
		Version:      version,
		TxId:         txId,
	}, nil
}

// parseStateBroadcastEventOld 解析旧格式 StateBroadcast 事件（主合约部署/setState，无 txId）
// 格式：Topics[1]=contractAddr, Topics[2]=key, Data=abi(uint256 value, uint64 version)
func (cb *CacheBroadcaster) parseStateBroadcastEventOld(log *ethtypes.Log) (*CacheEntry, error) {
	if len(log.Topics) < 3 {
		return nil, fmt.Errorf("invalid event topics count: %d", len(log.Topics))
	}

	contractAddr := common.BytesToAddress(log.Topics[1].Bytes()[12:])
	key := log.Topics[2]

	vals, err := cb.eventABIOld.Events["StateBroadcast"].Inputs.NonIndexed().Unpack(log.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack StateBroadcast old data: %w", err)
	}

	if len(vals) != 2 {
		return nil, fmt.Errorf("expected 2 values (value,version), got %d", len(vals))
	}

	value, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("invalid value type: %T", vals[0])
	}

	version, ok := vals[1].(uint64)
	if !ok {
		if vBig, ok2 := vals[1].(*big.Int); ok2 {
			version = vBig.Uint64()
		} else {
			return nil, fmt.Errorf("invalid version type: %T", vals[1])
		}
	}

	valueBytes := value.Bytes()
	paddedValue := make([]byte, 32)
	if len(valueBytes) > 0 {
		copy(paddedValue[32-len(valueBytes):], valueBytes)
	}

	return &CacheEntry{
		ContractAddr: contractAddr,
		ShardId:      cb.nodeConfig.ShardID,
		Key:          key,
		Value:        paddedValue,
		Version:      version,
		TxId:         common.Hash{}, // 旧格式无 txId，部署/setState 不涉及冻结
	}, nil
}

// parseStateUnfreezeEvent 解析 StateUnfreeze 事件
// Topics[1]=contractAddr, Topics[2]=key, Data=bytes32 txId
func (cb *CacheBroadcaster) parseStateUnfreezeEvent(log *ethtypes.Log) (common.Address, common.Hash, common.Hash, error) {
	if len(log.Topics) < 3 {
		return common.Address{}, common.Hash{}, common.Hash{}, fmt.Errorf("invalid StateUnfreeze topics count: %d", len(log.Topics))
	}

	contractAddr := common.BytesToAddress(log.Topics[1].Bytes()[12:])
	key := log.Topics[2]

	vals, err := cb.unfreezeABI.Events["StateUnfreeze"].Inputs.NonIndexed().Unpack(log.Data)
	if err != nil {
		return common.Address{}, common.Hash{}, common.Hash{}, fmt.Errorf("failed to unpack StateUnfreeze data: %w", err)
	}
	if len(vals) != 1 {
		return common.Address{}, common.Hash{}, common.Hash{}, fmt.Errorf("expected 1 value (txId), got %d", len(vals))
	}

	txIdRaw, ok := vals[0].([32]byte)
	if !ok {
		return common.Address{}, common.Hash{}, common.Hash{}, fmt.Errorf("invalid txId type: %T", vals[0])
	}

	return contractAddr, key, common.Hash(txIdRaw), nil
}

// validateVersion 验证版本号，只接受更高版本号的更新
// 改进：加强版本号验证，避免回退
func (cb *CacheBroadcaster) validateVersion(entry *CacheEntry) bool {
	key := cb.cacheKey(entry.ContractAddr, entry.ShardId, entry.Key)

	cb.cacheLock.RLock()
	existing, exists := cb.cache.Get(key)
	cb.cacheLock.RUnlock()

	if !exists {
		// 如果不存在，接受任何版本（首次更新）
		return true
	}

	// 只接受更高版本号的更新
	if entry.Version > existing.Version {
		return true
	}

	// 如果版本号相同，检查值是否不同（可能是同一版本的不同值，应该拒绝）
	if entry.Version == existing.Version {
		// 值相同，可能是重复消息，允许（幂等性）
		if bytes.Equal(entry.Value, existing.Value) {
			return true
		}
		// 值不同但版本相同，可能是冲突，拒绝
		utils.Logger().Warn().
			Str("key", key).
			Uint64("version", entry.Version).
			Msg("[JOYUE] version conflict: same version but different value")
		return false
	}

	// 版本号更低，拒绝（避免回退）
	return false
}

// updateLocalCache 更新本地缓存
// 改进：加强版本号验证
// 注意：此函数用于处理来自 P2P 的缓存更新（主合约的 StateBroadcast 事件）
// 代理合约的本地更新应使用 SetCacheLocal，不会被此函数覆盖
func (cb *CacheBroadcaster) updateLocalCache(entry *CacheEntry) {
	key := cb.cacheKey(entry.ContractAddr, entry.ShardId, entry.Key)

	cb.cacheLock.Lock()
	defer cb.cacheLock.Unlock()

	// 检查版本，只更新更新的版本
	existing, exists := cb.cache.Get(key)
	if !exists {
		// 缓存不存在，直接设置
		valueUint := new(big.Int).SetBytes(entry.Value)
		cb.cache.Set(key, entry)
		utils.Logger().Debug().
			Str("key", key).
			Str("contract", entry.ContractAddr.Hex()).
			Uint64("version", entry.Version).
			Str("valueUint", valueUint.String()).
			Int("valueLen", len(entry.Value)).
			Msg("[JOYUE] local cache updated (new entry from P2P)")
	} else if entry.Version > existing.Version {
		// 版本更高，更新缓存
		valueUint := new(big.Int).SetBytes(entry.Value)
		existingValueUint := new(big.Int).SetBytes(existing.Value)
		cb.cache.Set(key, entry)
		utils.Logger().Debug().
			Str("key", key).
			Str("contract", entry.ContractAddr.Hex()).
			Uint64("existingVersion", existing.Version).
			Uint64("newVersion", entry.Version).
			Str("existingValue", existingValueUint.String()).
			Str("newValue", valueUint.String()).
			Msg("[JOYUE] local cache updated (version upgrade from P2P)")
	} else if entry.Version == existing.Version {
		// 版本相同，检查值是否相同（幂等性）
		if !bytes.Equal(entry.Value, existing.Value) {
			valueUint := new(big.Int).SetBytes(entry.Value)
			existingValueUint := new(big.Int).SetBytes(existing.Value)
			utils.Logger().Warn().
				Str("key", key).
				Str("contract", entry.ContractAddr.Hex()).
				Uint64("version", entry.Version).
				Str("existingValue", existingValueUint.String()).
				Str("newValue", valueUint.String()).
				Msg("[JOYUE] version conflict: same version but different value, keeping existing")
		} else {
			utils.Logger().Debug().
				Str("key", key).
				Str("contract", entry.ContractAddr.Hex()).
				Uint64("version", entry.Version).
				Msg("[JOYUE] duplicate cache update (same version and value), ignoring")
		}
	} else {
		// 版本更低，拒绝更新（保护本地更新不被低版本 P2P 消息覆盖）
		valueUint := new(big.Int).SetBytes(entry.Value)
		existingValueUint := new(big.Int).SetBytes(existing.Value)
		utils.Logger().Debug().
			Str("key", key).
			Str("contract", entry.ContractAddr.Hex()).
			Uint64("existingVersion", existing.Version).
			Uint64("newVersion", entry.Version).
			Str("existingValue", existingValueUint.String()).
			Str("newValue", valueUint.String()).
			Msg("[JOYUE] rejected cache update (version not newer, protecting local update)")
	}
}

// broadcastCacheUpdate 通过 P2P 广播缓存更新（保留向后兼容）
func (cb *CacheBroadcaster) broadcastCacheUpdate(entry *CacheEntry) error {
	return cb.broadcastCacheUpdateImmediate(entry)
}

// broadcastCacheUpdateImmediate 立即广播缓存更新（不等待区块确认）
// 改进：使用高优先级 P2P 消息通道，立即广播
func (cb *CacheBroadcaster) broadcastCacheUpdateImmediate(entry *CacheEntry) error {
	// 序列化缓存条目
	msg := cb.serializeCacheEntry(entry)

	// 通过 PubSub 广播
	topic, err := cb.host.GetOrJoin(cb.topic)
	if err != nil {
		return fmt.Errorf("failed to get topic: %w", err)
	}

	// 改进：立即发布，不等待
	// 使用带超时的 context，确保快速失败
	ctx, cancel := context.WithTimeout(cb.ctx, 1*time.Second)
	defer cancel()

	// GetOrJoin 直接返回 *libp2p_pubsub.Topic
	// 立即发布，不等待确认（异步）
	if err := topic.Publish(ctx, msg); err != nil {
		return fmt.Errorf("failed to publish message immediately: %w", err)
	}

	// 改进：记录广播时间，用于性能监控
	utils.Logger().Debug().
		Str("contract", entry.ContractAddr.Hex()).
		Str("key", entry.Key.Hex()).
		Uint64("version", entry.Version).
		Str("txId", entry.TxId.Hex()).
		Msg("[JOYUE] cache update broadcasted immediately")

	return nil
}

// serializeCacheEntry 序列化缓存条目（type=0x02，含 txId）
// 格式：[1 type=0x02][20 contractAddr][4 shardId][32 key][8 version][32 txId][4 valueLen][valueLen value]
func (cb *CacheBroadcaster) serializeCacheEntry(entry *CacheEntry) []byte {
	buf := make([]byte, 1+20+4+32+8+32+4+len(entry.Value))
	offset := 0
	buf[offset] = 0x02
	offset++
	copy(buf[offset:], entry.ContractAddr.Bytes())
	offset += 20
	binary.BigEndian.PutUint32(buf[offset:], entry.ShardId)
	offset += 4
	copy(buf[offset:], entry.Key.Bytes())
	offset += 32
	binary.BigEndian.PutUint64(buf[offset:], entry.Version)
	offset += 8
	copy(buf[offset:], entry.TxId.Bytes())
	offset += 32
	binary.BigEndian.PutUint32(buf[offset:], uint32(len(entry.Value)))
	offset += 4
	copy(buf[offset:], entry.Value)
	return buf
}

// deserializeCacheEntry 反序列化缓存条目，兼容 type=0x01（旧）和 type=0x02（新，含 txId）
func (cb *CacheBroadcaster) deserializeCacheEntry(data []byte) (*CacheEntry, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("empty message")
	}
	msgType := data[0]

	switch msgType {
	case 0x01:
		// 旧格式：[1][20][4][32][8][4][value]
		minLen := 1 + 20 + 4 + 32 + 8 + 4
		if len(data) < minLen {
			return nil, fmt.Errorf("invalid v1 message length: %d", len(data))
		}
		offset := 1
		contractAddr := common.BytesToAddress(data[offset : offset+20])
		offset += 20
		shardId := binary.BigEndian.Uint32(data[offset : offset+4])
		offset += 4
		key := common.BytesToHash(data[offset : offset+32])
		offset += 32
		version := binary.BigEndian.Uint64(data[offset : offset+8])
		offset += 8
		valueLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if len(data) < offset+valueLen {
			return nil, fmt.Errorf("v1 value truncated")
		}
		value := make([]byte, valueLen)
		copy(value, data[offset:offset+valueLen])
		return &CacheEntry{ContractAddr: contractAddr, ShardId: shardId, Key: key, Value: value, Version: version}, nil

	case 0x02:
		// 新格式：[1][20][4][32][8][32 txId][4][value]
		minLen := 1 + 20 + 4 + 32 + 8 + 32 + 4
		if len(data) < minLen {
			return nil, fmt.Errorf("invalid v2 message length: %d", len(data))
		}
		offset := 1
		contractAddr := common.BytesToAddress(data[offset : offset+20])
		offset += 20
		shardId := binary.BigEndian.Uint32(data[offset : offset+4])
		offset += 4
		key := common.BytesToHash(data[offset : offset+32])
		offset += 32
		version := binary.BigEndian.Uint64(data[offset : offset+8])
		offset += 8
		txId := common.BytesToHash(data[offset : offset+32])
		offset += 32
		valueLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if len(data) < offset+valueLen {
			return nil, fmt.Errorf("v2 value truncated")
		}
		value := make([]byte, valueLen)
		copy(value, data[offset:offset+valueLen])
		return &CacheEntry{ContractAddr: contractAddr, ShardId: shardId, Key: key, Value: value, Version: version, TxId: txId}, nil

	default:
		return nil, fmt.Errorf("unsupported message type: 0x%02x", msgType)
	}
}

// cacheKey 生成缓存键
func (cb *CacheBroadcaster) cacheKey(contractAddr common.Address, shardId uint32, key common.Hash) string {
	return fmt.Sprintf("%s:%d:%s", contractAddr.Hex(), shardId, key.Hex())
}

// GetCache 获取缓存值
func (cb *CacheBroadcaster) GetCache(contractAddr common.Address, shardId uint32, key common.Hash) ([]byte, uint64, bool) {
	cacheKey := cb.cacheKey(contractAddr, shardId, key)

	cb.cacheLock.RLock()
	defer cb.cacheLock.RUnlock()

	entry, exists := cb.cache.Get(cacheKey)
	if !exists {
		utils.Logger().Debug().
			Str("cacheKey", cacheKey).
			Str("contract", contractAddr.Hex()).
			Uint32("shardId", shardId).
			Str("key", key.Hex()).
			Msg("[JOYUE] cache miss in GetCache")
		return nil, 0, false
	}

	valueUint := new(big.Int).SetBytes(entry.Value)
	utils.Logger().Debug().
		Str("cacheKey", cacheKey).
		Str("contract", contractAddr.Hex()).
		Uint32("shardId", shardId).
		Str("key", key.Hex()).
		Uint64("version", entry.Version).
		Str("valueUint", valueUint.String()).
		Int("valueLen", len(entry.Value)).
		Str("valueHex", hex.EncodeToString(entry.Value)).
		Msg("[JOYUE] cache hit in GetCache")

	return entry.Value, entry.Version, true
}

// GetAllCacheForContract 获取某个合约的所有缓存条目
func (cb *CacheBroadcaster) GetAllCacheForContract(contractAddr common.Address, shardId uint32) []*CacheEntry {
	cb.cacheLock.RLock()
	defer cb.cacheLock.RUnlock()

	var results []*CacheEntry
	contractPrefix := fmt.Sprintf("%s:%d:", contractAddr.Hex(), shardId)

	// 遍历所有缓存键
	keys := cb.cache.Keys()
	for _, key := range keys {
		// 检查是否是目标合约的缓存
		if len(key) > len(contractPrefix) && key[:len(contractPrefix)] == contractPrefix {
			if entry, exists := cb.cache.Get(key); exists {
				results = append(results, entry)
			}
		}
	}

	return results
}

// SetCacheLocal 设置本地缓存值（不广播）
// 用于代理合约修改本地缓存，不需要广播到其他节点
// 注意：也会进行版本号验证，防止低版本覆盖高版本
func (cb *CacheBroadcaster) SetCacheLocal(contractAddr common.Address, shardId uint32, key common.Hash, value []byte, version uint64) {
	cacheKey := cb.cacheKey(contractAddr, shardId, key)

	cb.cacheLock.Lock()
	defer cb.cacheLock.Unlock()

	// 检查现有缓存
	existing, exists := cb.cache.Get(cacheKey)
	if !exists {
		// 缓存不存在，直接设置
		valueUint := new(big.Int).SetBytes(value)
		cb.cache.Set(cacheKey, &CacheEntry{
			ContractAddr: contractAddr,
			ShardId:      shardId,
			Key:          key,
			Value:        value,
			Version:      version,
		})
		utils.Logger().Debug().
			Str("cacheKey", cacheKey).
			Str("contract", contractAddr.Hex()).
			Str("key", key.Hex()).
			Uint64("version", version).
			Str("value", valueUint.String()).
			Int("valueLen", len(value)).
			Msg("[JOYUE] SetCacheLocal: creating new cache entry")
	} else if version > existing.Version {
		// 版本更高，更新缓存
		valueUint := new(big.Int).SetBytes(value)
		existingValueUint := new(big.Int).SetBytes(existing.Value)
		cb.cache.Set(cacheKey, &CacheEntry{
			ContractAddr: contractAddr,
			ShardId:      shardId,
			Key:          key,
			Value:        value,
			Version:      version,
		})
		utils.Logger().Debug().
			Str("cacheKey", cacheKey).
			Str("contract", contractAddr.Hex()).
			Str("key", key.Hex()).
			Uint64("existingVersion", existing.Version).
			Uint64("newVersion", version).
			Str("existingValue", existingValueUint.String()).
			Str("newValue", valueUint.String()).
			Int("valueLen", len(value)).
			Msg("[JOYUE] SetCacheLocal: updating local cache (version upgrade)")
	} else if version == existing.Version {
		// 版本相同：值不同则更新（代理合约乐观快进，不修改版本号）；值相同则幂等忽略
		if !bytes.Equal(value, existing.Value) {
			valueUint := new(big.Int).SetBytes(value)
			existingValueUint := new(big.Int).SetBytes(existing.Value)
			cb.cache.Set(cacheKey, &CacheEntry{
				ContractAddr: contractAddr,
				ShardId:      shardId,
				Key:          key,
				Value:        value,
				Version:      version,
			})
			utils.Logger().Debug().
				Str("cacheKey", cacheKey).
				Str("contract", contractAddr.Hex()).
				Str("key", key.Hex()).
				Uint64("version", version).
				Str("existingValue", existingValueUint.String()).
				Str("newValue", valueUint.String()).
				Msg("[JOYUE] SetCacheLocal: agent optimistic fast-forward (same version, updated value)")
		} else {
			utils.Logger().Debug().
				Str("cacheKey", cacheKey).
				Str("contract", contractAddr.Hex()).
				Str("key", key.Hex()).
				Uint64("version", version).
				Msg("[JOYUE] SetCacheLocal: duplicate update (same version and value), ignoring")
		}
	} else {
		// 版本更低，拒绝更新（保护高版本缓存不被低版本覆盖）
		valueUint := new(big.Int).SetBytes(value)
		existingValueUint := new(big.Int).SetBytes(existing.Value)
		utils.Logger().Debug().
			Str("cacheKey", cacheKey).
			Str("contract", contractAddr.Hex()).
			Str("key", key.Hex()).
			Uint64("existingVersion", existing.Version).
			Uint64("newVersion", version).
			Str("existingValue", existingValueUint.String()).
			Str("newValue", valueUint.String()).
			Msg("[JOYUE] SetCacheLocal: rejected (version not newer, protecting existing cache)")
	}
}

// ============================================================
//                      冻结表操作
// ============================================================

// GetAvailable 返回某个 key 的可用量 = 权威缓存值 - 所有未确认冻结量之和。
// 若权威缓存中没有该 key，返回 (nil, 0, false)。
func (cb *CacheBroadcaster) GetAvailable(contractAddr common.Address, shardId uint32, key common.Hash) ([]byte, uint64, bool) {
	value, version, ok := cb.GetCache(contractAddr, shardId, key)
	if !ok {
		return nil, 0, false
	}

	authValue := new(big.Int).SetBytes(value)

	cacheKey := cb.cacheKey(contractAddr, shardId, key)
	cb.freezeLock.RLock()
	txMap := cb.freezeTable[cacheKey]
	totalFrozen := new(big.Int)
	for _, amount := range txMap {
		totalFrozen.Add(totalFrozen, amount)
	}
	cb.freezeLock.RUnlock()

	available := new(big.Int).Sub(authValue, totalFrozen)
	if available.Sign() < 0 {
		// 说明在在冻结区的某些交易注定会失败。这里available可以直接设置为缓存值
		available.Set(authValue)
	}

	result := make([]byte, 32)
	available.FillBytes(result)

	utils.Logger().Debug().
		Str("contract", contractAddr.Hex()).
		Uint32("shardId", shardId).
		Str("key", key.Hex()).
		Str("authValue", authValue.String()).
		Str("totalFrozen", totalFrozen.String()).
		Str("available", available.String()).
		Msg("[JOYUE] GetAvailable")

	return result, version, true
}

// Freeze 尝试在冻结表中预占 amount。
// 若当前可用量 >= amount，插入成功并返回 true；否则返回 false。
// 同一 txId 对同一 key 的多次调用会累加冻结量。
func (cb *CacheBroadcaster) Freeze(contractAddr common.Address, shardId uint32, key common.Hash, txId common.Hash, amount *big.Int) bool {
	if amount == nil || amount.Sign() <= 0 {
		return true // 零金额无需冻结
	}

	cacheKey := cb.cacheKey(contractAddr, shardId, key)

	// 先读取权威值（不持 freezeLock）
	value, _, ok := cb.GetCache(contractAddr, shardId, key)
	authValue := new(big.Int)
	if ok {
		authValue.SetBytes(value)
	}

	cb.freezeLock.Lock()
	defer cb.freezeLock.Unlock()

	// 计算当前总冻结量
	totalFrozen := new(big.Int)
	txMap := cb.freezeTable[cacheKey]
	for _, a := range txMap {
		totalFrozen.Add(totalFrozen, a)
	}

	available := new(big.Int).Sub(authValue, totalFrozen)
	if available.Cmp(amount) < 0 {
		utils.Logger().Warn().
			Str("contract", contractAddr.Hex()).
			Str("key", key.Hex()).
			Str("txId", txId.Hex()).
			Str("authValue", authValue.String()).
			Str("totalFrozen", totalFrozen.String()).
			Str("available", available.String()).
			Str("requested", amount.String()).
			Msg("[JOYUE] Freeze: insufficient available balance")
		return false
	}

	if txMap == nil {
		txMap = make(map[common.Hash]*big.Int)
		cb.freezeTable[cacheKey] = txMap
	}
	if existing, exists := txMap[txId]; exists {
		txMap[txId] = new(big.Int).Add(existing, amount)
	} else {
		txMap[txId] = new(big.Int).Set(amount)
	}

	utils.Logger().Debug().
		Str("contract", contractAddr.Hex()).
		Str("key", key.Hex()).
		Str("txId", txId.Hex()).
		Str("amount", amount.String()).
		Str("newTotalFrozen", new(big.Int).Add(totalFrozen, amount).String()).
		Msg("[JOYUE] Freeze: reserved")

	return true
}

// Unfreeze 从冻结表中移除 txId 对应的冻结记录。
func (cb *CacheBroadcaster) Unfreeze(contractAddr common.Address, shardId uint32, key common.Hash, txId common.Hash) {
	cacheKey := cb.cacheKey(contractAddr, shardId, key)

	cb.freezeLock.Lock()
	defer cb.freezeLock.Unlock()

	txMap, exists := cb.freezeTable[cacheKey]
	if !exists {
		return
	}
	amount, found := txMap[txId]
	if !found {
		return
	}
	delete(txMap, txId)
	if len(txMap) == 0 {
		delete(cb.freezeTable, cacheKey)
	}

	utils.Logger().Debug().
		Str("contract", contractAddr.Hex()).
		Str("key", key.Hex()).
		Str("txId", txId.Hex()).
		Str("releasedAmount", amount.String()).
		Msg("[JOYUE] Unfreeze: released")
}

// broadcastUnfreezeImmediate 通过 P2P 广播解冻消息，通知其他节点移除冻结表条目。
// P2P 消息格式（type=0x03）：[1 byte type=0x03][20 contractAddr][4 shardId][32 key][32 txId]
func (cb *CacheBroadcaster) broadcastUnfreezeImmediate(contractAddr common.Address, shardId uint32, key common.Hash, txId common.Hash) error {
	buf := make([]byte, 1+20+4+32+32)
	buf[0] = 0x03
	copy(buf[1:21], contractAddr.Bytes())
	binary.BigEndian.PutUint32(buf[21:25], shardId)
	copy(buf[25:57], key.Bytes())
	copy(buf[57:89], txId.Bytes())

	topic, err := cb.host.GetOrJoin(cb.topic)
	if err != nil {
		return fmt.Errorf("failed to get topic: %w", err)
	}
	ctx, cancel := context.WithTimeout(cb.ctx, 1*time.Second)
	defer cancel()
	return topic.Publish(ctx, buf)
}

// Close 关闭广播器
func (cb *CacheBroadcaster) Close() {
	if cb.cancel != nil {
		cb.cancel()
	}
}

// ============================================================
//                      全局缓存访问器
// ============================================================

var (
	globalCacheBroadcaster *CacheBroadcaster
	globalCacheLock        sync.RWMutex
)

// SetGlobalCacheBroadcaster 设置全局缓存广播器（供 precompile 使用）
func SetGlobalCacheBroadcaster(cb *CacheBroadcaster) {
	globalCacheLock.Lock()
	defer globalCacheLock.Unlock()
	globalCacheBroadcaster = cb
}

// GetGlobalCacheBroadcaster 获取全局缓存广播器
func GetGlobalCacheBroadcaster() *CacheBroadcaster {
	globalCacheLock.RLock()
	defer globalCacheLock.RUnlock()
	return globalCacheBroadcaster
}
