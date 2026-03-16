package main

import (
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

使用方式：
  go run cmd/joyue-relayer/main.go \
    --rpc http://127.0.0.1:9500 \
    --shard-id 0 \
    --private-key 0x... \
    --target-shard-rpcs 1=http://127.0.0.1:9502,2=http://127.0.0.1:9504
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
type Relayer struct {
	privateKey    *ecdsa.PrivateKey
	shardRPCs     map[uint32]string // shardID -> RPC URL
	rpcClients    map[uint32]*ethclient.Client
	clientsLock   sync.RWMutex
	clientTimeout time.Duration
	sendLock      sync.Mutex // 保护 SendTransaction 的并发访问

	sendNonceCache map[uint32]uint64 // targetShardID -> next nonce

	ethClient       *ethclient.Client
	rpcURL          string
	shardID         uint32
	eventSignature  common.Hash
	processedEvents map[common.Hash]bool
	processedLock   sync.RWMutex
	stopChan        chan struct{}
	running         bool
	runningLock     sync.Mutex
}

// NewRelayer 创建新的 Relayer
func NewRelayer(privateKeyHex string, sourceRPCURL string, sourceShardID uint32, targetShardRPCs string) (*Relayer, error) {
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

	log.Printf("[INFO] Relayer created (address: %s, source shard: %d, target shards: %d)",
		crypto.PubkeyToAddress(privateKey.PublicKey).Hex(),
		sourceShardID,
		len(shardRPCs))

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

	// 启动事件监听 goroutine
	go r.eventLoop(ctx, fromBlock, pollInterval)

	log.Printf("[INFO] Relayer started (RPC: %s, Shard: %d, FromBlock: %d, PollInterval: %v)",
		r.rpcURL, r.shardID, fromBlock, pollInterval)

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

	if r.ethClient != nil {
		r.ethClient.Close()
	}

	log.Println("[INFO] Relayer stopped")
}

// eventLoop 事件监听循环
func (r *Relayer) eventLoop(ctx context.Context, fromBlock uint64, pollInterval time.Duration) {
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
		log.Printf("[ERROR] failed to get latest block number: %v", err)
		return
	}

	// 如果 fromBlock 为 0，从最新区块开始
	if fromBlock == 0 {
		fromBlock = latestBlock
		if fromBlock > 100 {
			fromBlock -= 100 // 回退 100 个区块，避免遗漏
		}
		log.Printf("[INFO] Auto-detected fromBlock: %d (latest: %d)", fromBlock, latestBlock)
	} else {
		log.Printf("[INFO] Using manual fromBlock: %d (latest: %d)", fromBlock, latestBlock)
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
				log.Printf("[ERROR] failed to get current block number: %v", err)
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
				log.Printf("[ERROR] failed to filter logs: %v", err)
				continue
			}

			// 按区块分组处理（支持 Master→Agent 隔离与 Agent→Master 聚合）
			r.processBlockEvents(ctx, logs)

			// 更新 fromBlock
			fromBlock = currentBlock + 1
		}
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
			r.sendTransaction(ctx, event)
			r.processedLock.Lock()
			r.processedEvents[eventKey] = true
			r.processedLock.Unlock()
			continue
		}

		// Agent→Master：尝试解析 executeAndCallback，成功则参与聚合
		parsed, err := r.parseExecuteAndCallbackCalldata(event)
		if err != nil {
			log.Printf("[WARN] parseExecuteAndCallbackCalldata failed (txHash=%s), fallback to single send: %v", evtLog.TxHash.Hex(), err)
			r.sendTransaction(ctx, event)
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

	// 对 Agent→Master 聚合组发送
	for key, calls := range groups {
		if len(calls) == 1 {
			r.sendTransaction(ctx, calls[0].Event)
		} else {
			r.sendBatchTransaction(ctx, key.shardID, key.master, calls)
		}
	}
}

// parseExecuteAndCallbackCalldata 解析 executeAndCallback 格式（仅 Agent→Master processIntent）
func (r *Relayer) parseExecuteAndCallbackCalldata(event *CrossShardRequestEvent) (*parsedExecutorCall, error) {
	cd := event.Calldata
	if len(cd) < 228 {
		return nil, fmt.Errorf("calldata too short (need 228, got %d)", len(cd))
	}
	masterAddr := common.BytesToAddress(cd[144:164])
	calldataOffset64 := binary.BigEndian.Uint64(cd[220:228])
	calldataOffset := int(calldataOffset64)
	if calldataOffset < 0 {
		return nil, fmt.Errorf("invalid targetCalldata offset: negative")
	}
	dataStart := 4 + calldataOffset
	if dataStart+32 > len(cd) {
		return nil, fmt.Errorf("invalid targetCalldata offset")
	}
	calldataLen64 := binary.BigEndian.Uint64(cd[dataStart+24 : dataStart+32])
	if calldataLen64 > uint64(len(cd)) || dataStart+32+int(calldataLen64) > len(cd) {
		return nil, fmt.Errorf("invalid targetCalldata length")
	}
	calldataLen := int(calldataLen64)
	targetCalldata := cd[dataStart+32 : dataStart+32+calldataLen]
	if len(targetCalldata) < 68 {
		return nil, fmt.Errorf("targetCalldata too short for processIntent")
	}
	payloadOffset64 := binary.BigEndian.Uint64(targetCalldata[28:36])
	payloadStart := 4 + int(payloadOffset64)
	if payloadStart+32 > len(targetCalldata) {
		return nil, fmt.Errorf("invalid payload offset")
	}
	payloadLen64 := binary.BigEndian.Uint64(targetCalldata[payloadStart+24 : payloadStart+32])
	if payloadLen64 > uint64(len(targetCalldata)) || payloadStart+32+int(payloadLen64) > len(targetCalldata) {
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
	r.sendTransaction(ctx, event)
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

// sendTransaction 发送交易到目标分片
func (r *Relayer) sendTransaction(ctx context.Context, event *CrossShardRequestEvent) {
	// 使用互斥锁保护整个发送过程
	r.sendLock.Lock()
	defer r.sendLock.Unlock()

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
	nonce, hasCached := r.sendNonceCache[targetShardID]
	if !hasCached {
		var err error
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

	const defaultGasLimit = uint64(500000) // 使用默认值

	// 构建并签名交易
	tx := harmonytypes.NewTransaction(nonce, event.Target, event.TargetShardID, event.Value, defaultGasLimit, gasPrice, event.Calldata)
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
		delete(r.sendNonceCache, targetShardID)
		return
	}
	r.sendNonceCache[targetShardID] = nonce + 1

	log.Printf("[INFO] Sent transaction successfully (txHash: %s, requestId: %d, targetShard: %d, target: %s, nonce: %d)",
		signedTx.Hash().Hex(), event.RequestID, event.TargetShardID, event.Target.Hex(), nonce)
}

func main() {
	var (
		rpcURL          = flag.String("rpc", "http://127.0.0.1:9500", "源分片 RPC URL（监听事件的分片）")
		shardID         = flag.Uint("shard-id", 0, "源分片 ID")
		privateKey      = flag.String("private-key", "", "发送交易的私钥（hex，带或不带 0x）")
		targetShardRPCs = flag.String("target-shard-rpcs", "", "目标分片 RPC 地址（格式：shardID=rpcURL,shardID=rpcURL）")
		fromBlock       = flag.Uint64("from-block", 0, "开始监听的区块号（0 表示从最新区块开始）")
		pollInterval    = flag.Duration("poll-interval", 2*time.Second, "轮询间隔")
	)

	flag.Parse()

	if *privateKey == "" {
		log.Fatal("缺少参数：--private-key")
	}

	// 创建 Relayer
	relayer, err := NewRelayer(*privateKey, *rpcURL, uint32(*shardID), *targetShardRPCs)
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

	// 等待中断信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("正在停止 Relayer...")
	relayer.Stop()
	log.Println("Relayer 已停止")
}
