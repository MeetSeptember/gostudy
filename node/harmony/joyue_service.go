package node

import (
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

// JoyueService 实现核心的影子缓存逻辑
// 它必须满足 core/vm/joyue_bridge.go 中定义的 ShadowReader 接口
type JoyueService struct {
	// 缓存结构: Key = "ShardID-Address-StorageKey", Value = RLP bytes
	// 使用 string 作为 map key 是因为 slice 不能作为 key
	cache map[string][]byte
	lock  sync.RWMutex
}

// NewJoyueService 创建服务实例
func NewJoyueService() *JoyueService {
	return &JoyueService{
		cache: make(map[string][]byte),
	}
}

// GetShadowState 是供 EVM 调用的核心接口
// 实现 vm.ShadowReader 接口
func (s *JoyueService) GetShadowState(shardID uint32, addr common.Address, key []byte) []byte {
	s.lock.RLock()
	defer s.lock.RUnlock()

	// 构造唯一的缓存键
	// 格式: {ShardID}-{HexAddress}-{HexKey}
	cacheKey := s.makeCacheKey(shardID, addr, key)

	if val, ok := s.cache[cacheKey]; ok {
		// 返回数据的副本，防止外部修改内部缓存
		ret := make([]byte, len(val))
		copy(ret, val)
		return ret
	}

	// 缓存未命中返回 nil，预编译合约会将其处理为空字节
	return nil
}

// SetShadowState 用于更新缓存 (未来由 P2P 消息触发)
func (s *JoyueService) SetShadowState(shardID uint32, addr common.Address, key []byte, val []byte) {
	s.lock.Lock()
	defer s.lock.Unlock()

	cacheKey := s.makeCacheKey(shardID, addr, key)

	// 存储数据的副本
	valCopy := make([]byte, len(val))
	copy(valCopy, val)
	s.cache[cacheKey] = valCopy
}

// makeCacheKey 生成内部存储键
func (s *JoyueService) makeCacheKey(shardID uint32, addr common.Address, key []byte) string {
	return fmt.Sprintf("%d-%s-%s", shardID, addr.Hex(), hex.EncodeToString(key))
}
