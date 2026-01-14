package joyue

import (
	"encoding/binary"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/internal/utils"
)

// QueueItem 队列项：表示一个待发送的跨分片交易请求
type QueueItem struct {
	ID         uint64         // 队列项唯一 ID
	ShardID    uint32         // 目标分片 ID
	To         common.Address // 目标合约地址
	Calldata   []byte         // 调用数据
	Value      *big.Int       // 转账金额
	CreatedAt  time.Time      // 创建时间
	RetryCount uint32         // 重试次数
	Status     QueueStatus    // 状态
	TxHash     common.Hash    // 发送成功后的交易哈希（如果已发送）
	LastError  string         // 最后一次错误信息（如果失败）
}

// QueueStatus 队列项状态
type QueueStatus uint8

const (
	QueueStatusPending   QueueStatus = iota // 待发送
	QueueStatusSending                      // 正在发送
	QueueStatusSent                         // 已发送（等待确认）
	QueueStatusFailed                       // 发送失败（可重试）
	QueueStatusCompleted                    // 已完成（已确认）
)

// String 返回状态的字符串表示
func (s QueueStatus) String() string {
	switch s {
	case QueueStatusPending:
		return "pending"
	case QueueStatusSending:
		return "sending"
	case QueueStatusSent:
		return "sent"
	case QueueStatusFailed:
		return "failed"
	case QueueStatusCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

// CrossShardQueue 跨分片交易队列管理器
type CrossShardQueue struct {
	items      map[uint64]*QueueItem // ID -> QueueItem
	itemsLock  sync.RWMutex          // 保护 items 的并发访问
	nextID     uint64                // 下一个可用的 ID
	nextIDLock sync.Mutex            // 保护 nextID 的并发访问

	maxRetries uint32        // 最大重试次数
	retryDelay time.Duration // 重试延迟
}

// NewCrossShardQueue 创建新的跨分片队列管理器
func NewCrossShardQueue() *CrossShardQueue {
	return &CrossShardQueue{
		items:      make(map[uint64]*QueueItem),
		nextID:     1,
		maxRetries: 3,
		retryDelay: 5 * time.Second,
	}
}

// Enqueue 将新的跨分片交易请求加入队列
// 返回队列项 ID 和生成的临时交易哈希（用于合约返回）
func (q *CrossShardQueue) Enqueue(shardID uint32, to common.Address, calldata []byte, value *big.Int) (uint64, common.Hash, error) {
	// 生成唯一 ID
	q.nextIDLock.Lock()
	id := q.nextID
	q.nextID++
	q.nextIDLock.Unlock()

	// 创建队列项
	item := &QueueItem{
		ID:         id,
		ShardID:    shardID,
		To:         to,
		Calldata:   calldata,
		Value:      new(big.Int).Set(value), // 复制 value，避免外部修改
		CreatedAt:  time.Now(),
		RetryCount: 0,
		Status:     QueueStatusPending,
	}

	// 添加到队列（写锁保护）
	q.itemsLock.Lock()
	q.items[id] = item
	q.itemsLock.Unlock()

	// 生成临时交易哈希（基于队列 ID，用于合约返回）
	// 使用队列 ID 生成一个确定的哈希，这样合约可以追踪这个请求
	tempTxHash := q.generateTempTxHash(id)

	utils.Logger().Debug().
		Uint64("queueID", id).
		Uint32("shardID", shardID).
		Str("to", to.Hex()).
		Str("tempTxHash", tempTxHash.Hex()).
		Msg("[JOYUE] enqueued cross-shard transaction")

	return id, tempTxHash, nil
}

// generateTempTxHash 生成临时交易哈希（基于队列 ID）
// 这个哈希会在合约中返回，用于追踪请求
func (q *CrossShardQueue) generateTempTxHash(queueID uint64) common.Hash {
	// 使用队列 ID 生成一个确定的哈希
	// 格式：0x00000000... + queueID (8 bytes)
	var hash [32]byte
	binary.BigEndian.PutUint64(hash[24:], queueID)
	// 前 24 bytes 保持为 0，表示这是一个临时哈希
	return common.BytesToHash(hash[:])
}

// Dequeue 从队列中取出一个待发送的项（FIFO）
// 返回队列项，如果队列为空则返回 nil
func (q *CrossShardQueue) Dequeue() *QueueItem {
	q.itemsLock.Lock()
	defer q.itemsLock.Unlock()

	// 查找最早的待发送项（按 ID 排序，ID 越小越早）
	var oldestItem *QueueItem
	var oldestID uint64 = ^uint64(0) // 最大 uint64

	for id, item := range q.items {
		// 只处理待发送或可重试的项
		if item.Status == QueueStatusPending ||
			(item.Status == QueueStatusFailed && item.RetryCount < q.maxRetries) {
			// 检查重试延迟
			if item.Status == QueueStatusFailed {
				timeSinceLastTry := time.Since(item.CreatedAt)
				if timeSinceLastTry < q.retryDelay {
					continue // 还没到重试时间
				}
			}
			if id < oldestID {
				oldestID = id
				oldestItem = item
			}
		}
	}

	if oldestItem != nil {
		// 标记为正在发送
		oldestItem.Status = QueueStatusSending
		return oldestItem
	}

	return nil
}

// UpdateStatus 更新队列项的状态
func (q *CrossShardQueue) UpdateStatus(id uint64, status QueueStatus, txHash common.Hash, err error) error {
	q.itemsLock.Lock()
	defer q.itemsLock.Unlock()

	item, exists := q.items[id]
	if !exists {
		return errors.New("queue item not found")
	}

	item.Status = status
	if txHash != (common.Hash{}) {
		item.TxHash = txHash
	}
	if err != nil {
		item.LastError = err.Error()
		if status == QueueStatusFailed {
			item.RetryCount++
		}
	}

	utils.Logger().Debug().
		Uint64("queueID", id).
		Str("status", status.String()).
		Str("txHash", txHash.Hex()).
		Msg("[JOYUE] updated queue item status")

	return nil
}

// GetItem 获取队列项（只读）
func (q *CrossShardQueue) GetItem(id uint64) (*QueueItem, bool) {
	q.itemsLock.RLock()
	defer q.itemsLock.RUnlock()

	item, exists := q.items[id]
	if !exists {
		return nil, false
	}

	// 返回副本，避免外部修改
	itemCopy := *item
	itemCopy.Calldata = make([]byte, len(item.Calldata))
	copy(itemCopy.Calldata, item.Calldata)
	if item.Value != nil {
		itemCopy.Value = new(big.Int).Set(item.Value)
	}

	return &itemCopy, true
}

// GetPendingCount 获取待发送的队列项数量
func (q *CrossShardQueue) GetPendingCount() int {
	q.itemsLock.RLock()
	defer q.itemsLock.RUnlock()

	count := 0
	for _, item := range q.items {
		if item.Status == QueueStatusPending ||
			(item.Status == QueueStatusFailed && item.RetryCount < q.maxRetries) {
			count++
		}
	}
	return count
}

// CleanupCompleted 清理已完成的队列项（可选，避免内存泄漏）
// 只保留最近 N 个已完成的项
func (q *CrossShardQueue) CleanupCompleted(keepCount int) {
	q.itemsLock.Lock()
	defer q.itemsLock.Unlock()

	// 收集所有已完成的项
	completedItems := make([]*QueueItem, 0)
	for _, item := range q.items {
		if item.Status == QueueStatusCompleted {
			completedItems = append(completedItems, item)
		}
	}

	// 如果超过保留数量，删除最旧的
	if len(completedItems) > keepCount {
		// 按 ID 排序（ID 越小越旧）
		for i := 0; i < len(completedItems)-keepCount; i++ {
			delete(q.items, completedItems[i].ID)
		}
	}
}
