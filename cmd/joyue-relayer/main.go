package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	harmonytypes "github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/crypto/hash"
	"github.com/harmony-one/harmony/eth/rpc"
)

/*
JOYUE Relayer - 完全独立的跨分片交易转发服务

功能：
- 监听链上的 CrossShardRequest 事件
- 发送交易到目标分片
- Agent→Master 仅对 Master.processIntent(bytes) 聚批为 processIntentBatch；Sparrow Wallet 等其它 targetCalldata 一律单笔转发

使用方式：
  go run cmd/joyue-relayer/main.go \
    --rpc http://127.0.0.1:9500 \
    --shard-id 0 \
    --private-key 0x... \
    --target-shard-rpcs 1=http://127.0.0.1:9502,2=http://127.0.0.1:9504 \
    --gas-limit 3000000

转发子交易默认 gas 较大（与 chainspace / 多腿 prepare 同量级）；高压仍 OOG 时可再调高 --gas-limit。
*/

// CrossShardRequestEvent 跨分片请求事件
type CrossShardRequestEvent struct {
	RequestID        uint64
	TargetShardID    uint32
	Target           common.Address
	Calldata         []byte
	Value            *big.Int
	CallbackAddr     common.Address
	CallbackSelector [4]byte
	TxHash           common.Hash
	BlockNumber      uint64
	LogIndex         uint
}

// joyueProcessIntentSelector = bytes4(keccak256("processIntent(bytes)"))；仅此类目标调用可参与 Agent→Master 批处理。
var joyueProcessIntentSelector = crypto.Keccak256([]byte("processIntent(bytes)"))[:4]

// parsedExecutorCall 从 executeAndCallback calldata 解析出的信息（仅 Agent→Master processIntent 格式）
type parsedExecutorCall struct {
	Event         *CrossShardRequestEvent
	MasterAddr    common.Address
	Payload       []byte
	SourceShardID uint32
	CallbackAddr  common.Address
	CallbackSel   [4]byte
	RequestID     uint64
}

// onCrossShardCallback(uint256,bool,bytes) 选择器，用于 Master→Agent 回调
// 此类事件不做解析，直接单跳转发
var onCrossShardCallbackSelector = [4]byte{0x8f, 0x4f, 0xfc, 0xb1}

// isMasterToAgentCallback 判断是否为 Master→Agent 回调
func isMasterToAgentCallback(event *CrossShardRequestEvent) bool {
	return event.CallbackSelector == onCrossShardCallbackSelector
}

// Relayer 跨分片交易 Relayer 服务（完全独立，不依赖链代码）
// 支持双队列架构：Queue 1 (logsCh) Poller→Worker，Queue 2 (sendCh) Worker→Sender（仅 concurrent=1）
type Relayer struct {
	privateKey    *ecdsa.PrivateKey
	shardRPCs     map[uint32]string // shardID -> RPC URL
	rpcClients    map[uint32]*ethclient.Client
	clientsLock   sync.RWMutex
	clientTimeout time.Duration
	sendLockMap   sync.Map // targetShardID -> *sync.Mutex，按分片加锁，多 Sender 可并行发往不同分片

	sendNonceCache   map[uint32]uint64 // targetShardID -> next nonce
	sendNonceCacheMu sync.Mutex        // 保护 sendNonceCache（多 Sender / go sendTransaction 并发写 map 会 fatal）

	ethClient       *ethclient.Client
	rpcURL          string
	shardID         uint32
	eventSignature  common.Hash
	processedEvents map[common.Hash]bool
	processedLock   sync.RWMutex
	stopChan        chan struct{}
	running         bool
	runningLock     sync.Mutex

	// 双队列：concurrent=0 仅用 logsCh，concurrent=1 用 logsCh + sendCh
	concurrent   bool
	numSenders   int                          // concurrent=1 时 Sender 协程数
	maxBatchSize int                          // Agent→Master 聚合时每批最多多少个（0=不限制）
	gasLimit     uint64                       // 发往目标分片的子交易 gas limit（固定值，与 calldata 复杂度相关）
	logsCh       chan []ethtypes.Log          // Queue 1: Poller → Worker
	sendCh       chan *CrossShardRequestEvent // Queue 2: Worker → Sender（concurrent=1 时）
	pollerWg     sync.WaitGroup
	workerWg     sync.WaitGroup
	senderWg     sync.WaitGroup
}

// NewRelayer 创建新的 Relayer
// concurrent: 0=顺序模式（2PC 安全），1=并发模式（JOYUE 高吞吐，Poller 不阻塞）
// numSenders: concurrent=1 时 Sender 协程数，多 Sender 可并行发往不同分片
// maxBatchSize: Agent→Master 聚合时每批最多多少个，0=不限制
func NewRelayer(privateKeyHex string, sourceRPCURL string, sourceShardID uint32, targetShardRPCs string, concurrent bool, numSenders int, maxBatchSize int, gasLimit uint64) (*Relayer, error) {
	// 解析私钥
	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(privateKeyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	// 连接到源分片 RPC（用于监听事件）
	ethClient, err := ethclient.Dial(sourceRPCURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to source RPC: %w", err)
	}

	// 解析目标分片 RPC 配置
	shardRPCs := make(map[uint32]string)
	if targetShardRPCs != "" {
		pairs := strings.Split(targetShardRPCs, ",")
		for _, pair := range pairs {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) != 2 {
				log.Printf("[WARN] invalid RPC pair format: %s, skipping", pair)
				continue
			}
			var shardID uint32
			if _, err := fmt.Sscanf(parts[0], "%d", &shardID); err != nil {
				log.Printf("[WARN] failed to parse shard ID from %s: %v, skipping", pair, err)
				continue
			}
			rpcURL := strings.TrimSpace(parts[1])
			shardRPCs[shardID] = rpcURL
		}
	}

	// 计算事件签名
	eventSignature := hash.Keccak256Hash([]byte("CrossShardRequest(uint256,uint32,address,bytes,uint256,address,bytes4)"))

	// Queue 1: 有界缓冲，避免 Poller 积压过多
	logsCh := make(chan []ethtypes.Log, 50)
	var sendCh chan *CrossShardRequestEvent
	if concurrent {
		sendCh = make(chan *CrossShardRequestEvent, 500) // Queue 2，多 Sender 时需更大缓冲
	}
	if numSenders < 1 {
		numSenders = 1
	}

	if gasLimit == 0 {
		gasLimit = 3_000_000
	}
	log.Printf("[INFO] Relayer created (address: %s, source shard: %d, target shards: %d, concurrent=%v, numSenders=%d, maxBatchSize=%d, gasLimit=%d)",
		crypto.PubkeyToAddress(privateKey.PublicKey).Hex(),
		sourceShardID,
		len(shardRPCs),
		concurrent,
		numSenders,
		maxBatchSize,
		gasLimit)

	return &Relayer{
		privateKey:      privateKey,
		shardRPCs:       shardRPCs,
		rpcClients:      make(map[uint32]*ethclient.Client),
		clientTimeout:   30 * time.Second,
		sendNonceCache:  make(map[uint32]uint64),
		ethClient:       ethClient,
		rpcURL:          sourceRPCURL,
		shardID:         sourceShardID,
		eventSignature:  eventSignature,
		processedEvents: make(map[common.Hash]bool),
		stopChan:        make(chan struct{}),
		running:         false,
		concurrent:      concurrent,
		numSenders:      numSenders,
		maxBatchSize:    maxBatchSize,
		gasLimit:        gasLimit,
		logsCh:          logsCh,
		sendCh:          sendCh,
	}, nil
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

	// 启动 Worker：消费 Queue 1
	r.workerWg.Add(1)
	go func() {
		defer r.workerWg.Done()
		r.workerLoop(ctx)
	}()

	// 启动多个 Sender：消费 Queue 2（仅 concurrent=1）
	if r.concurrent {
		for i := 0; i < r.numSenders; i++ {
			r.senderWg.Add(1)
			go func(id int) {
				defer r.senderWg.Done()
				r.senderLoop(ctx)
			}(i)
		}
	}

	// 启动 Poller：生产 Queue 1
	r.pollerWg.Add(1)
	go func() {
		defer r.pollerWg.Done()
		r.pollerLoop(ctx, fromBlock, pollInterval)
	}()

	log.Printf("[INFO] Relayer started (RPC: %s, Shard: %d, FromBlock: %d, PollInterval: %v, Concurrent: %v, NumSenders: %d)",
		r.rpcURL, r.shardID, fromBlock, pollInterval, r.concurrent, r.numSenders)

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

	// 等待 Poller 退出（避免向已关闭的 logsCh 发送）
	r.pollerWg.Wait()

	// 关闭 Queue 1，让 Worker 从阻塞中退出
	close(r.logsCh)
	r.workerWg.Wait()

	// 关闭 Queue 2，让 Sender 退出（仅 concurrent=1）
	if r.concurrent && r.sendCh != nil {
		close(r.sendCh)
		r.senderWg.Wait()
	}

	if r.ethClient != nil {
		r.ethClient.Close()
	}

	log.Println("[INFO] Relayer stopped")
}

// pollerLoop Poller：只负责轮询链、拉事件、入队，不阻塞在发送上（方案 C）
func (r *Relayer) pollerLoop(ctx context.Context, fromBlock uint64, pollInterval time.Duration) {
	precompileAddr := common.BytesToAddress([]byte{109})
	query := ethereum.FilterQuery{
		Addresses: []common.Address{precompileAddr},
		Topics:    [][]common.Hash{{r.eventSignature}},
	}

	latestBlock, err := r.ethClient.BlockNumber(ctx)
	if err != nil {
		log.Printf("[ERROR] failed to get latest block number: %v", err)
		return
	}

	if fromBlock == 0 {
		fromBlock = latestBlock
		if fromBlock > 100 {
			fromBlock -= 100
		}
		log.Printf("[INFO] Auto-detected fromBlock: %d (latest: %d)", fromBlock, latestBlock)
	} else {
		log.Printf("[INFO] Using manual fromBlock: %d (latest: %d)", fromBlock, latestBlock)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastNoBlockLog time.Time
	var lastFromBlockAhead time.Time // 记录 fromBlock > currentBlock 首次出现时间
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopChan:
			return
		case <-ticker.C:
			currentBlock, err := r.ethClient.BlockNumber(ctx)
			if err != nil {
				log.Printf("[ERROR] failed to get current block number: %v", err)
				continue
			}

			if currentBlock <= fromBlock {
				if fromBlock > currentBlock {
					if lastFromBlockAhead.IsZero() {
						lastFromBlockAhead = time.Now()
					}
					// 若 fromBlock 长期领先（如 reorg 或链停滞），2 分钟后回退以恢复轮询
					if time.Since(lastFromBlockAhead) > 2*time.Minute {
						log.Printf("[WARN] fromBlock(%d) > latest(%d) 已超过 2 分钟，回退 fromBlock 以恢复",
							fromBlock, currentBlock)
						fromBlock = currentBlock + 1
						lastFromBlockAhead = time.Time{}
					}
				} else {
					lastFromBlockAhead = time.Time{}
				}
				if time.Since(lastNoBlockLog) > 30*time.Second {
					if fromBlock > currentBlock {
						// 超前 1 块多为正常（刚处理完 last block，等待下一块），>1 才可能是停滞/reorg
						if fromBlock-currentBlock > 1 {
							log.Printf("[WARN] fromBlock(%d) > latest(%d): 链可能已停滞或发生 reorg",
								fromBlock, currentBlock)
						} else {
							log.Printf("[DEBUG] fromBlock(%d) > latest(%d)，等待链产出新区块",
								fromBlock, currentBlock)
						}
					} else {
						log.Printf("[DEBUG] No new blocks (fromBlock=%d, latest=%d), waiting...",
							fromBlock, currentBlock)
					}
					lastNoBlockLog = time.Now()
				}
				continue
			}
			lastFromBlockAhead = time.Time{}

			query.FromBlock = big.NewInt(int64(fromBlock))
			query.ToBlock = big.NewInt(int64(currentBlock))

			logs, err := r.ethClient.FilterLogs(ctx, query)
			if err != nil {
				log.Printf("[ERROR] failed to filter logs: %v", err)
				continue
			}

			log.Printf("[DEBUG] Polled blocks %d-%d (fromBlock=%d, latest=%d), got %d logs from precompile 0x6D",
				fromBlock, currentBlock, fromBlock, currentBlock, len(logs))

			// 入队 Queue 1（阻塞时表示 Worker 处理慢，自然背压）
			select {
			case r.logsCh <- logs:
				fromBlock = currentBlock + 1
			case <-ctx.Done():
				return
			case <-r.stopChan:
				return
			}
		}
	}
}

// workerLoop Worker：消费 Queue 1，解析、聚合、发送（方案 C 的消费端）
func (r *Relayer) workerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopChan:
			return
		case logs, ok := <-r.logsCh:
			if !ok {
				return // channel 已关闭，退出
			}
			if len(logs) == 0 {
				continue
			}
			r.processBlockEvents(ctx, logs)
		}
	}
}

// senderLoop Sender：消费 Queue 2，执行实际发送（方案 A，仅 concurrent=1）
func (r *Relayer) senderLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopChan:
			return
		case event, ok := <-r.sendCh:
			if !ok {
				return // channel 已关闭，退出
			}
			r.sendTransaction(ctx, event)
		}
	}
}

// doSend 根据 concurrent 模式选择同步发送或入队 Queue 2
func (r *Relayer) doSend(ctx context.Context, event *CrossShardRequestEvent) {
	if r.concurrent {
		select {
		case r.sendCh <- event:
		case <-ctx.Done():
		case <-r.stopChan:
		}
	} else {
		r.sendTransaction(ctx, event)
	}
}

// processEvent 处理单个事件
func (r *Relayer) processEvent(ctx context.Context, eventLog ethtypes.Log) {
	// 检查是否已处理
	indexHash := common.BigToHash(big.NewInt(int64(eventLog.Index)))
	eventKey := hash.Keccak256Hash(eventLog.TxHash.Bytes(), indexHash.Bytes())
	r.processedLock.RLock()
	if r.processedEvents[eventKey] {
		r.processedLock.RUnlock()
		return
	}
	r.processedLock.RUnlock()

	// 解析事件
	event, err := r.parseEvent(eventLog)
	if err != nil {
		log.Printf("[ERROR] failed to parse CrossShardRequest event (txHash: %s): %v", eventLog.TxHash.Hex(), err)
		return
	}

	// 标记为已处理
	r.processedLock.Lock()
	r.processedEvents[eventKey] = true
	r.processedLock.Unlock()

	log.Printf("[INFO] Processing CrossShardRequest event (requestId: %d, targetShard: %d, target: %s, txHash: %s)",
		event.RequestID, event.TargetShardID, event.Target.Hex(), eventLog.TxHash.Hex())

	// 发送交易到目标分片
	go r.sendTransaction(ctx, event)
}

// processBlockEvents 按区块分组处理事件
func (r *Relayer) processBlockEvents(ctx context.Context, logs []ethtypes.Log) {
	blockGroups := make(map[uint64][]ethtypes.Log)
	for _, evtLog := range logs {
		blockGroups[evtLog.BlockNumber] = append(blockGroups[evtLog.BlockNumber], evtLog)
	}
	for blockNum, blockLogs := range blockGroups {
		r.processBlockLogs(ctx, blockNum, blockLogs)
	}
}

// processBlockLogs 处理单个区块内的事件
// 转发类型隔离：Master→Agent（onCrossShardCallback）直接单跳，不解析；Agent→Master 才尝试解析聚合
func (r *Relayer) processBlockLogs(ctx context.Context, blockNum uint64, logs []ethtypes.Log) {
	type groupKey struct {
		shardID uint32
		master  common.Address
	}
	groups := make(map[groupKey][]*parsedExecutorCall)

	for _, evtLog := range logs {
		indexHash := common.BigToHash(big.NewInt(int64(evtLog.Index)))
		eventKey := hash.Keccak256Hash(evtLog.TxHash.Bytes(), indexHash.Bytes())
		r.processedLock.RLock()
		if r.processedEvents[eventKey] {
			r.processedLock.RUnlock()
			continue
		}
		r.processedLock.RUnlock()

		event, err := r.parseEvent(evtLog)
		if err != nil {
			log.Printf("[ERROR] failed to parse event (txHash: %s): %v", evtLog.TxHash.Hex(), err)
			continue
		}

		// 隔离：Master→Agent 回调直接单跳转发，不解析（避免 calldata 格式不符时报错）
		if isMasterToAgentCallback(event) {
			log.Printf("[INFO] Master→Agent callback (requestId=%d), single-hop forward", event.RequestID)
			r.doSend(ctx, event)
			r.processedLock.Lock()
			r.processedEvents[eventKey] = true
			r.processedLock.Unlock()
			continue
		}

		// Agent→Master：尝试解析 executeAndCallback，成功则参与聚合
		parsed, err := r.parseExecuteAndCallbackCalldata(event)
		if err != nil {
			log.Printf("[WARN] parseExecuteAndCallbackCalldata failed (txHash=%s), fallback to single send: %v", evtLog.TxHash.Hex(), err)
			r.doSend(ctx, event)
			r.processedLock.Lock()
			r.processedEvents[eventKey] = true
			r.processedLock.Unlock()
			continue
		}

		key := groupKey{shardID: event.TargetShardID, master: parsed.MasterAddr}
		groups[key] = append(groups[key], parsed)
		r.processedLock.Lock()
		r.processedEvents[eventKey] = true
		r.processedLock.Unlock()
	}

	// 对 Agent→Master 聚合组发送（按 maxBatchSize 分批）
	for key, calls := range groups {
		if len(calls) == 1 {
			r.doSend(ctx, calls[0].Event)
		} else {
			batchSize := r.maxBatchSize
			if batchSize <= 0 {
				r.sendBatchTransaction(ctx, key.shardID, key.master, calls)
			} else {
				for i := 0; i < len(calls); i += batchSize {
					end := i + batchSize
					if end > len(calls) {
						end = len(calls)
					}
					batch := calls[i:end]
					if len(batch) == 1 {
						r.doSend(ctx, batch[0].Event)
					} else {
						r.sendBatchTransaction(ctx, key.shardID, key.master, batch)
					}
				}
			}
		}
	}
}

// parseExecuteAndCallbackCalldata 解析 executeAndCallback 格式。
// 仅当 target 为 Master.processIntent(bytes) 时成功；Sparrow / 其它任意 calldata 返回错误以便走单笔 doSend，避免误聚批为 processIntentBatch。
func (r *Relayer) parseExecuteAndCallbackCalldata(event *CrossShardRequestEvent) (*parsedExecutorCall, error) {
	cd := event.Calldata
	if len(cd) < 228 {
		return nil, fmt.Errorf("calldata too short (need 228, got %d)", len(cd))
	}
	masterAddr := common.BytesToAddress(cd[144:164])
	calldataOffset64 := binary.BigEndian.Uint64(cd[220:228])
	if calldataOffset64 > 0x7fffffffffffffff {
		return nil, fmt.Errorf("invalid targetCalldata offset: overflow")
	}
	calldataOffset := int(calldataOffset64)
	dataStart := 4 + calldataOffset
	if dataStart < 0 || dataStart+32 > len(cd) {
		return nil, fmt.Errorf("invalid targetCalldata offset")
	}
	calldataLen64 := binary.BigEndian.Uint64(cd[dataStart+24 : dataStart+32])
	if calldataLen64 > uint64(len(cd)) || calldataLen64 > 0x7fffffffffffffff || dataStart+32+int(calldataLen64) > len(cd) {
		return nil, fmt.Errorf("invalid targetCalldata length")
	}
	calldataLen := int(calldataLen64)
	targetCalldata := cd[dataStart+32 : dataStart+32+calldataLen]
	if len(targetCalldata) < 4 {
		return nil, fmt.Errorf("targetCalldata too short for selector")
	}
	if !bytes.Equal(targetCalldata[:4], joyueProcessIntentSelector) {
		return nil, fmt.Errorf("not processIntent(bytes), skip batch (selector=%#x)", targetCalldata[:4])
	}
	if len(targetCalldata) < 68 {
		return nil, fmt.Errorf("targetCalldata too short for processIntent")
	}
	payloadOffset64 := binary.BigEndian.Uint64(targetCalldata[28:36])
	if payloadOffset64 > 0x7fffffffffffffff {
		return nil, fmt.Errorf("invalid payload offset: overflow")
	}
	payloadStart := 4 + int(payloadOffset64)
	if payloadStart < 0 || payloadStart+32 > len(targetCalldata) {
		return nil, fmt.Errorf("invalid payload offset")
	}
	payloadLen64 := binary.BigEndian.Uint64(targetCalldata[payloadStart+24 : payloadStart+32])
	if payloadLen64 > uint64(len(targetCalldata)) || payloadLen64 > 0x7fffffffffffffff || payloadStart+32+int(payloadLen64) > len(targetCalldata) {
		return nil, fmt.Errorf("invalid payload length")
	}
	payloadLen := int(payloadLen64)
	payload := make([]byte, payloadLen)
	copy(payload, targetCalldata[payloadStart+32:payloadStart+32+payloadLen])
	sourceShardID := binary.BigEndian.Uint32(cd[32:36])
	callbackAddr := common.BytesToAddress(cd[48:68])
	var callbackSel [4]byte
	copy(callbackSel[:], cd[68:72])
	requestID := new(big.Int).SetBytes(cd[100:132]).Uint64()
	return &parsedExecutorCall{
		Event:         event,
		MasterAddr:    masterAddr,
		Payload:       payload,
		SourceShardID: sourceShardID,
		CallbackAddr:  callbackAddr,
		CallbackSel:   callbackSel,
		RequestID:     requestID,
	}, nil
}

var processIntentBatchSelector = []byte{0x7d, 0x5e, 0x81, 0x2e}

// abiUint256 将 uint64 编码为 32 字节 ABI uint256（大端序，右对齐）
func abiUint256(v uint64) []byte {
	b := make([]byte, 32)
	binary.BigEndian.PutUint64(b[24:32], v)
	return b
}

func (r *Relayer) buildProcessIntentBatchCalldata(payloads [][]byte) []byte {
	// ABI bytes[]: offset(32) + [at offset: length(32) + elem_offsets(N*32)] + [len+data, ...]
	// 偏移量相对于 params 起始（即 calldata[4:]），不是相对于 calldata
	paramsHeadSize := 32 + 32 + len(payloads)*32 // offset + length + elem_offsets
	dataStartInParams := paramsHeadSize          // 第一个元素在 params 中的起始位置
	offsets := make([]byte, len(payloads)*32)
	var dataBytes []byte
	for i, p := range payloads {
		binary.BigEndian.PutUint64(offsets[i*32+24:(i+1)*32], uint64(dataStartInParams))
		lenBytes := make([]byte, 32)
		binary.BigEndian.PutUint64(lenBytes[24:32], uint64(len(p)))
		dataBytes = append(dataBytes, lenBytes...)
		dataBytes = append(dataBytes, p...)
		dataStartInParams += 32 + len(p)
	}
	headSize := 4 + paramsHeadSize // selector + params head
	calldata := make([]byte, 0, headSize+len(dataBytes))
	calldata = append(calldata, processIntentBatchSelector...)
	// 标准 ABI：每个 uint256 为 32 字节，值右对齐（最后 8 字节）
	calldata = append(calldata, abiUint256(32)...)                    // array offset
	calldata = append(calldata, abiUint256(uint64(len(payloads)))...) // array length
	calldata = append(calldata, offsets...)
	calldata = append(calldata, dataBytes...)
	return calldata
}

func (r *Relayer) buildExecuteAndCallbackCalldata(sourceShardID uint32, callbackAddr common.Address, callbackSel [4]byte, requestID uint64, target common.Address, targetCalldata []byte) []byte {
	executorSelector := []byte{0x2b, 0x5d, 0x76, 0xa4}
	params := make([]byte, 0, 224+32+32+len(targetCalldata))
	b := make([]byte, 32)
	binary.BigEndian.PutUint32(b[28:32], sourceShardID)
	params = append(params, b...)
	b = make([]byte, 32)
	copy(b[12:32], callbackAddr.Bytes())
	params = append(params, b...)
	b = make([]byte, 32)
	copy(b[0:4], callbackSel[:])
	params = append(params, b...)
	b = make([]byte, 32)
	new(big.Int).SetUint64(requestID).FillBytes(b)
	params = append(params, b...)
	b = make([]byte, 32)
	copy(b[12:32], target.Bytes())
	params = append(params, b...)
	params = append(params, make([]byte, 32)...)
	b = make([]byte, 32)
	binary.BigEndian.PutUint64(b[24:32], 224)
	params = append(params, b...)
	b = make([]byte, 32)
	binary.BigEndian.PutUint64(b[24:32], uint64(len(targetCalldata)))
	params = append(params, b...)
	params = append(params, targetCalldata...)
	return append(executorSelector, params...)
}

func (r *Relayer) sendBatchTransaction(ctx context.Context, targetShardID uint32, masterAddr common.Address, calls []*parsedExecutorCall) {
	log.Printf("[INFO] sendBatchTransaction: aggregating %d Agent→Master call(s) to shard %d, master=%s",
		len(calls), targetShardID, masterAddr.Hex())
	payloads := make([][]byte, len(calls))
	for i, c := range calls {
		payloads[i] = c.Payload
	}
	batchCalldata := r.buildProcessIntentBatchCalldata(payloads)
	executorAddr := common.BytesToAddress([]byte{116})
	executorCalldata := r.buildExecuteAndCallbackCalldata(
		calls[0].SourceShardID,
		calls[0].CallbackAddr,
		calls[0].CallbackSel,
		calls[0].RequestID,
		masterAddr,
		batchCalldata,
	)
	event := &CrossShardRequestEvent{
		TargetShardID: targetShardID,
		Target:        executorAddr,
		Calldata:      executorCalldata,
		Value:         big.NewInt(0),
	}
	r.doSend(ctx, event)
}

// parseEvent 解析 CrossShardRequest 事件
func (r *Relayer) parseEvent(eventLog ethtypes.Log) (*CrossShardRequestEvent, error) {
	// 验证事件签名
	if len(eventLog.Topics) < 2 {
		return nil, fmt.Errorf("invalid event: insufficient topics")
	}
	if eventLog.Topics[0] != r.eventSignature {
		return nil, fmt.Errorf("invalid event signature")
	}

	// Topics[1] = requestId (indexed)
	requestID := new(big.Int).SetBytes(eventLog.Topics[1][:]).Uint64()

	// 解析 Data
	// 格式：shardID (4 bytes) + target (20 bytes) + calldataLen (4 bytes) + calldata + value (32 bytes) + callbackAddr (20 bytes) + callbackSelector (4 bytes)
	data := eventLog.Data
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
		TxHash:           eventLog.TxHash,
		BlockNumber:      eventLog.BlockNumber,
		LogIndex:         eventLog.Index,
	}, nil
}

// getClient 获取或创建 RPC 客户端
func (r *Relayer) getClient(shardID uint32) (*ethclient.Client, error) {
	r.clientsLock.RLock()
	client, exists := r.rpcClients[shardID]
	r.clientsLock.RUnlock()

	if exists && client != nil {
		return client, nil
	}

	// 获取 RPC URL
	r.clientsLock.RLock()
	rpcURL, exists := r.shardRPCs[shardID]
	r.clientsLock.RUnlock()

	if !exists {
		return nil, fmt.Errorf("RPC URL not configured for shard %d", shardID)
	}

	// 创建新的客户端
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("failed to dial RPC for shard %d: %w", shardID, err)
	}

	// 保存客户端
	r.clientsLock.Lock()
	r.rpcClients[shardID] = client
	r.clientsLock.Unlock()

	return client, nil
}

// getSendLock 按分片获取锁，多 Sender 可并行发往不同分片
func (r *Relayer) getSendLock(shardID uint32) *sync.Mutex {
	v, _ := r.sendLockMap.LoadOrStore(shardID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// sendTransaction 发送交易到目标分片
func (r *Relayer) sendTransaction(ctx context.Context, event *CrossShardRequestEvent) {
	// 按目标分片加锁，不同分片可并行发送
	lock := r.getSendLock(event.TargetShardID)
	lock.Lock()
	defer lock.Unlock()

	// 创建超时上下文
	ctx, cancel := context.WithTimeout(ctx, r.clientTimeout)
	defer cancel()

	// 获取 RPC 客户端
	client, err := r.getClient(event.TargetShardID)
	if err != nil {
		log.Printf("[ERROR] failed to get RPC client for shard %d: %v", event.TargetShardID, err)
		return
	}

	// 获取发送者地址
	from := crypto.PubkeyToAddress(r.privateKey.PublicKey)

	// 准备交易参数
	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Printf("[ERROR] failed to get chain ID: %v", err)
		return
	}

	targetShardID := event.TargetShardID
	r.sendNonceCacheMu.Lock()
	nonce, hasCached := r.sendNonceCache[targetShardID]
	r.sendNonceCacheMu.Unlock()
	if !hasCached {
		nonce, err = client.PendingNonceAt(ctx, from)
		if err != nil {
			log.Printf("[ERROR] failed to get nonce: %v", err)
			return
		}
	}

	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		log.Printf("[ERROR] failed to get gas price: %v", err)
		return
	}

	// 构建并签名交易（gas 由 --gas-limit 配置；chainspace / 多腿 prepare 易超过 1M）
	tx := harmonytypes.NewTransaction(nonce, event.Target, event.TargetShardID, event.Value, r.gasLimit, gasPrice, event.Calldata)
	signer := harmonytypes.NewEIP155Signer(chainID)
	signedTx, err := harmonytypes.SignTx(tx, signer, r.privateKey)
	if err != nil {
		log.Printf("[ERROR] failed to sign transaction: %v", err)
		return
	}

	// 将 Harmony 交易编码为 RLP
	encodedTx, err := rlp.EncodeToBytes(signedTx)
	if err != nil {
		log.Printf("[ERROR] failed to encode transaction: %v", err)
		return
	}

	// 通过 RPC 发送交易
	rpcURL := r.shardRPCs[event.TargetShardID]
	rpcClient, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		log.Printf("[ERROR] failed to dial RPC: %v", err)
		return
	}
	defer rpcClient.Close()

	var txHash common.Hash
	err = rpcClient.CallContext(ctx, &txHash, "hmy_sendRawTransaction", hexutil.Bytes(encodedTx))
	if err != nil {
		log.Printf("[ERROR] failed to send transaction via RPC: %v", err)
		r.sendNonceCacheMu.Lock()
		delete(r.sendNonceCache, targetShardID)
		r.sendNonceCacheMu.Unlock()
		return
	}
	r.sendNonceCacheMu.Lock()
	r.sendNonceCache[targetShardID] = nonce + 1
	r.sendNonceCacheMu.Unlock()

	log.Printf("[INFO] Sent transaction successfully (txHash: %s, requestId: %d, targetShard: %d, target: %s, nonce: %d, gasLimit: %d)",
		signedTx.Hash().Hex(), event.RequestID, event.TargetShardID, event.Target.Hex(), nonce, r.gasLimit)
}

func main() {
	var (
		rpcURL          = flag.String("rpc", "http://127.0.0.1:9500", "源分片 RPC URL（监听事件的分片）")
		shardID         = flag.Uint("shard-id", 0, "源分片 ID")
		privateKey      = flag.String("private-key", "", "发送交易的私钥（hex，带或不带 0x）")
		targetShardRPCs = flag.String("target-shard-rpcs", "", "目标分片 RPC 地址（格式：shardID=rpcURL,shardID=rpcURL）")
		fromBlock       = flag.Uint64("from-block", 0, "开始监听的区块号（0 表示从最新区块开始）")
		pollInterval    = flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
		concurrent      = flag.Int("concurrent", 0, "0=顺序模式（2PC 安全），1=并发模式（JOYUE 高吞吐）")
		numSenders      = flag.Int("num-senders", 3, "concurrent=1 时 Sender 协程数，多 Sender 可并行发往不同分片")
		maxBatchSize    = flag.Int("max-batch-size", 5, "Agent→Master 聚合时每批最多多少个（0=不限制）")
		gasLimit        = flag.Uint64("gas-limit", 3_000_000, "转发到目标分片的子交易 gas limit（chainspace / MEV bot 等多腿 prepare 建议 ≥2.5M；0 表示使用默认 3M）")
	)

	flag.Parse()

	if *privateKey == "" {
		log.Fatal("缺少参数：--private-key")
	}

	concurrentMode := *concurrent == 1
	gl := *gasLimit
	if gl == 0 {
		gl = 3_000_000
	}

	// 创建 Relayer
	relayer, err := NewRelayer(*privateKey, *rpcURL, uint32(*shardID), *targetShardRPCs, concurrentMode, *numSenders, *maxBatchSize, gl)
	if err != nil {
		log.Fatalf("创建 Relayer 失败: %v", err)
	}

	// 启动 Relayer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *fromBlock == 0 {
		// 从最新区块开始
		latestBlock, err := relayer.ethClient.BlockNumber(ctx)
		if err != nil {
			log.Fatalf("获取最新区块失败: %v", err)
		}
		*fromBlock = latestBlock
		if *fromBlock > 100 {
			*fromBlock -= 100 // 回退 100 个区块，避免遗漏
		}
		log.Printf("从区块 %d 开始监听（最新区块: %d）", *fromBlock, latestBlock)
	}

	if err := relayer.StartFromBlock(ctx, *fromBlock, *pollInterval); err != nil {
		log.Fatalf("启动 Relayer 失败: %v", err)
	}

	log.Printf("Relayer 已启动")
	log.Printf("  RPC URL: %s", *rpcURL)
	log.Printf("  分片 ID: %d", *shardID)
	log.Printf("  开始区块: %d", *fromBlock)
	log.Printf("  轮询间隔: %v", *pollInterval)
	log.Printf("  并发模式: %v (0=顺序/2PC, 1=并发/JOYUE)", *concurrent)
	log.Printf("  Sender 数: %d (concurrent=1 时生效)", *numSenders)
	log.Printf("  最大批大小: %d (0=不限制)", *maxBatchSize)
	log.Printf("  子交易 gas limit: %d", gl)

	// 等待中断信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("正在停止 Relayer...")
	relayer.Stop()
	log.Println("Relayer 已停止")
}
