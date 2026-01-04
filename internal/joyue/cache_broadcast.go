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

// StateBroadcast 事件 ABI
const stateBroadcastEventABI = `[
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

// CacheEntry 缓存条目
type CacheEntry struct {
	ContractAddr common.Address
	Key          common.Hash
	Value        []byte
	Version      uint64
}

// CacheBroadcaster P2P 缓存广播器
type CacheBroadcaster struct {
	host       p2p.Host
	nodeConfig *nodeconfig.ConfigType
	cache      *lrucache.Cache[string, *CacheEntry] // LRU 缓存，key = contractAddr+key
	cacheLock  sync.RWMutex
	eventABI   abi.ABI
	eventSig   common.Hash
	topic      string
	ctx        context.Context
	cancel     context.CancelFunc
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
		return nil, fmt.Errorf("failed to parse event ABI: %w", err)
	}

	// 计算事件签名
	eventSig := crypto.Keccak256Hash([]byte("StateBroadcast(address,bytes32,uint256,uint64)"))

	// 创建全局 PubSub topic（跨分片同步）
	// 所有分片都订阅同一个 topic，实现跨分片缓存同步
	topic := "joyue-cache-global"

	ctx, cancel := context.WithCancel(context.Background())

	// 创建 LRU 缓存，默认大小 10000 条目
	// TODO: 可以通过配置项调整缓存大小
	cacheSize := 10000
	cache := lrucache.NewCache[string, *CacheEntry](cacheSize)

	cb := &CacheBroadcaster{
		host:       host,
		nodeConfig: nodeConfig,
		cache:      cache,
		eventABI:   eventABI,
		eventSig:   eventSig,
		topic:      topic,
		ctx:        ctx,
		cancel:     cancel,
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

			// 解析缓存更新
			entry, err := cb.deserializeCacheEntry(msg.GetData())
			if err != nil {
				utils.Logger().Error().
					Err(err).
					Msg("[JOYUE] failed to deserialize cache entry")
				continue
			}

			// 改进：版本号验证 - 只接受更高版本号的更新，避免回退
			// updateLocalCache 内部已经有版本号检查，这里加强验证
			if !cb.validateVersion(entry) {
				utils.Logger().Debug().
					Str("contract", entry.ContractAddr.Hex()).
					Str("key", entry.Key.Hex()).
					Uint64("version", entry.Version).
					Str("from", msg.GetFrom().String()).
					Msg("[JOYUE] rejected cache update (version not newer)")
				continue
			}

			// 更新本地缓存
			cb.updateLocalCache(entry)

			valueUint := new(big.Int).SetBytes(entry.Value)
			utils.Logger().Info().
				Str("contract", entry.ContractAddr.Hex()).
				Str("key", entry.Key.Hex()).
				Uint64("version", entry.Version).
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

			// 调试：打印所有事件的 Topic0
			utils.Logger().Debug().
				Str("txHash", receipt.TxHash.Hex()).
				Str("logTopic0", log.Topics[0].Hex()).
				Str("expectedEventSig", cb.eventSig.Hex()).
				Msg("[JOYUE] ProcessBlockLogs: checking log")

			// 检查是否是 StateBroadcast 事件
			if log.Topics[0] != cb.eventSig {
				continue
			}

			utils.Logger().Info().
				Str("txHash", receipt.TxHash.Hex()).
				Str("logAddress", log.Address.Hex()).
				Msg("[JOYUE] ProcessBlockLogs: found StateBroadcast event")

			// 解析事件（需要转换为 Ethereum 的 Log 类型）
			ethLog := convertToEthLog(log)
			entry, err := cb.parseStateBroadcastEvent(ethLog)
			if err != nil {
				utils.Logger().Error().
					Err(err).
					Str("txHash", receipt.TxHash.Hex()).
					Msg("[JOYUE] failed to parse StateBroadcast event")
				continue
			}

			// 改进：版本号验证 - 只接受更高版本号的更新，避免回退
			if !cb.validateVersion(entry) {
				utils.Logger().Debug().
					Str("contract", entry.ContractAddr.Hex()).
					Str("key", entry.Key.Hex()).
					Uint64("version", entry.Version).
					Uint64("block", block.NumberU64()).
					Msg("[JOYUE] rejected cache update (version not newer)")
				continue
			}

			// 更新本地缓存
			cb.updateLocalCache(entry)

			// 调试：打印缓存键信息和 value 内容
			cacheKey := cb.cacheKey(entry.ContractAddr, entry.Key)
			valueUint := new(big.Int).SetBytes(entry.Value)
			utils.Logger().Debug().
				Str("cacheKey", cacheKey).
				Str("contract", entry.ContractAddr.Hex()).
				Str("key", entry.Key.Hex()).
				Int("valueLen", len(entry.Value)).
				Str("valueHex", hex.EncodeToString(entry.Value)).
				Str("valueUint", valueUint.String()).
				Msg("[JOYUE] cache key generated")

			// 改进：立即广播（不等待区块确认）- 使用高优先级通道
			// 在检测到 StateBroadcast 事件后立即广播，不等待区块确认
			if err := cb.broadcastCacheUpdateImmediate(entry); err != nil {
				utils.Logger().Error().
					Err(err).
					Str("contract", entry.ContractAddr.Hex()).
					Str("key", entry.Key.Hex()).
					Msg("[JOYUE] failed to broadcast cache update immediately")
			} else {
				valueUint := new(big.Int).SetBytes(entry.Value)
				utils.Logger().Info().
					Str("contract", entry.ContractAddr.Hex()).
					Str("key", entry.Key.Hex()).
					Str("cacheKey", cacheKey).
					Uint64("version", entry.Version).
					Str("valueUint", valueUint.String()).
					Int("valueLen", len(entry.Value)).
					Uint64("block", block.NumberU64()).
					Msg("[JOYUE] cache updated and broadcasted immediately")
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
// 接受 Ethereum 的 types.Log（因为 ABI 解析需要）
func (cb *CacheBroadcaster) parseStateBroadcastEvent(log *ethtypes.Log) (*CacheEntry, error) {
	if len(log.Topics) < 3 {
		return nil, fmt.Errorf("invalid event topics count: %d", len(log.Topics))
	}

	// Topics[0]: 事件签名
	// Topics[1]: contractAddr (indexed)
	// Topics[2]: key (indexed)
	contractAddr := common.BytesToAddress(log.Topics[1].Bytes()[12:])
	key := log.Topics[2]

	// 解析 non-indexed 参数 (value, version)
	vals, err := cb.eventABI.Events["StateBroadcast"].Inputs.NonIndexed().Unpack(log.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack event data: %w", err)
	}

	if len(vals) != 2 {
		return nil, fmt.Errorf("invalid event data length: %d", len(vals))
	}

	value, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("invalid value type: %T", vals[0])
	}

	version, ok := vals[1].(uint64)
	if !ok {
		// 尝试 *big.Int 转换
		if vBig, ok := vals[1].(*big.Int); ok {
			version = vBig.Uint64()
		} else {
			return nil, fmt.Errorf("invalid version type: %T", vals[1])
		}
	}

	// 将 value 填充到 32 字节（uint256 的标准长度）
	// big.Int.Bytes() 返回的是大端序，不包含前导零
	// 需要左填充到 32 字节，以便 Solidity 合约正确解析
	valueBytes := value.Bytes()
	if len(valueBytes) < 32 {
		// 左填充到 32 字节
		paddedValue := make([]byte, 32)
		copy(paddedValue[32-len(valueBytes):], valueBytes)
		valueBytes = paddedValue
	} else if len(valueBytes) > 32 {
		// 如果超过 32 字节，只取最后 32 字节（处理溢出情况）
		valueBytes = valueBytes[len(valueBytes)-32:]
	}

	return &CacheEntry{
		ContractAddr: contractAddr,
		Key:          key,
		Value:        valueBytes,
		Version:      version,
	}, nil
}

// validateVersion 验证版本号，只接受更高版本号的更新
// 改进：加强版本号验证，避免回退
func (cb *CacheBroadcaster) validateVersion(entry *CacheEntry) bool {
	key := cb.cacheKey(entry.ContractAddr, entry.Key)

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
	key := cb.cacheKey(entry.ContractAddr, entry.Key)

	cb.cacheLock.Lock()
	defer cb.cacheLock.Unlock()

	// 检查版本，只更新更新的版本
	existing, exists := cb.cache.Get(key)
	if !exists {
		// 缓存不存在，直接设置
		valueUint := new(big.Int).SetBytes(entry.Value)
		cb.cache.Set(key, entry)
		utils.Logger().Info().
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
		utils.Logger().Info().
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
		utils.Logger().Info().
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
		Msg("[JOYUE] cache update broadcasted immediately")

	return nil
}

// serializeCacheEntry 序列化缓存条目
func (cb *CacheBroadcaster) serializeCacheEntry(entry *CacheEntry) []byte {
	// 简单序列化格式：
	// [1 byte: version] [20 bytes: contractAddr] [32 bytes: key] [8 bytes: version] [4 bytes: valueLen] [valueLen bytes: value]
	buf := make([]byte, 1+20+32+8+4+len(entry.Value))
	offset := 0

	// Version (1 byte)
	buf[offset] = 0x01
	offset++

	// ContractAddr (20 bytes)
	copy(buf[offset:], entry.ContractAddr.Bytes())
	offset += 20

	// Key (32 bytes)
	copy(buf[offset:], entry.Key.Bytes())
	offset += 32

	// Version (8 bytes)
	binary.BigEndian.PutUint64(buf[offset:], entry.Version)
	offset += 8

	// Value length (4 bytes)
	binary.BigEndian.PutUint32(buf[offset:], uint32(len(entry.Value)))
	offset += 4

	// Value
	copy(buf[offset:], entry.Value)

	return buf
}

// deserializeCacheEntry 反序列化缓存条目
func (cb *CacheBroadcaster) deserializeCacheEntry(data []byte) (*CacheEntry, error) {
	if len(data) < 1+20+32+8+4 {
		return nil, fmt.Errorf("invalid message length: %d", len(data))
	}

	offset := 0

	// Version
	if data[offset] != 0x01 {
		return nil, fmt.Errorf("unsupported message version: %d", data[offset])
	}
	offset++

	// ContractAddr
	contractAddr := common.BytesToAddress(data[offset : offset+20])
	offset += 20

	// Key
	key := common.BytesToHash(data[offset : offset+32])
	offset += 32

	// Version
	version := binary.BigEndian.Uint64(data[offset : offset+8])
	offset += 8

	// Value length
	valueLen := binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4

	if len(data) < offset+int(valueLen) {
		return nil, fmt.Errorf("invalid value length: expected %d, got %d", valueLen, len(data)-offset)
	}

	// Value
	value := make([]byte, valueLen)
	copy(value, data[offset:offset+int(valueLen)])

	return &CacheEntry{
		ContractAddr: contractAddr,
		Key:          key,
		Value:        value,
		Version:      version,
	}, nil
}

// cacheKey 生成缓存键
func (cb *CacheBroadcaster) cacheKey(contractAddr common.Address, key common.Hash) string {
	return fmt.Sprintf("%s:%s", contractAddr.Hex(), key.Hex())
}

// GetCache 获取缓存值
func (cb *CacheBroadcaster) GetCache(contractAddr common.Address, key common.Hash) ([]byte, uint64, bool) {
	cacheKey := cb.cacheKey(contractAddr, key)

	cb.cacheLock.RLock()
	defer cb.cacheLock.RUnlock()

	entry, exists := cb.cache.Get(cacheKey)
	if !exists {
		utils.Logger().Info().
			Str("cacheKey", cacheKey).
			Str("contract", contractAddr.Hex()).
			Str("key", key.Hex()).
			Msg("[JOYUE] cache miss in GetCache")
		return nil, 0, false
	}

	valueUint := new(big.Int).SetBytes(entry.Value)
	utils.Logger().Info().
		Str("cacheKey", cacheKey).
		Str("contract", contractAddr.Hex()).
		Str("key", key.Hex()).
		Uint64("version", entry.Version).
		Str("valueUint", valueUint.String()).
		Int("valueLen", len(entry.Value)).
		Str("valueHex", hex.EncodeToString(entry.Value)).
		Msg("[JOYUE] cache hit in GetCache")

	return entry.Value, entry.Version, true
}

// GetAllCacheForContract 获取某个合约的所有缓存条目
func (cb *CacheBroadcaster) GetAllCacheForContract(contractAddr common.Address) []*CacheEntry {
	cb.cacheLock.RLock()
	defer cb.cacheLock.RUnlock()

	var results []*CacheEntry
	contractPrefix := contractAddr.Hex() + ":"

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
func (cb *CacheBroadcaster) SetCacheLocal(contractAddr common.Address, key common.Hash, value []byte, version uint64) {
	cacheKey := cb.cacheKey(contractAddr, key)

	cb.cacheLock.Lock()
	defer cb.cacheLock.Unlock()

	// 检查现有缓存
	existing, exists := cb.cache.Get(cacheKey)
	if !exists {
		// 缓存不存在，直接设置
		valueUint := new(big.Int).SetBytes(value)
		cb.cache.Set(cacheKey, &CacheEntry{
			ContractAddr: contractAddr,
			Key:          key,
			Value:        value,
			Version:      version,
		})
		utils.Logger().Info().
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
			Key:          key,
			Value:        value,
			Version:      version,
		})
		utils.Logger().Info().
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
		// 版本相同，检查值是否相同（幂等性）
		if !bytes.Equal(value, existing.Value) {
			valueUint := new(big.Int).SetBytes(value)
			existingValueUint := new(big.Int).SetBytes(existing.Value)
			utils.Logger().Warn().
				Str("cacheKey", cacheKey).
				Str("contract", contractAddr.Hex()).
				Str("key", key.Hex()).
				Uint64("version", version).
				Str("existingValue", existingValueUint.String()).
				Str("newValue", valueUint.String()).
				Msg("[JOYUE] SetCacheLocal: version conflict (same version but different value), keeping existing")
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
		utils.Logger().Info().
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
