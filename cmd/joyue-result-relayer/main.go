package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE Result Relayer（方案A：Agent ->(event)-> Relayer -> Master）

这个 relayer 的职责：
1) 在各个 agent shard 上监听 JoyueAgent 发出的 AgentResult 事件（包括 master shard，如果 master shard 也部署了 agent）；
2) 将事件里的 payload 转发到 master shard（例如 shard0）的 JoyueMaster.submitAgentResult；
3) 用户之后可以在 master shard 上调用 JoyueMaster.getResult(requestId) 查询“返回值/意图”。

重要说明：
- 这是“功能演示版”，不验证跨分片 proof，不做权限控制。
- 安全模型：信任 relayer 搬运日志；生产应增加：签名/证明/白名单/费控等。
- 如果 master shard 也部署了 agent 合约，用户可以在 master shard 上调用 agent.execute()，
  此时 relayer 也会监听 master shard 的 AgentResult 事件并转发到 master 合约（同分片内调用）。

兼容性：
- 本仓库 go-ethereum replace 到 v1.11.2，因此事件 data 解码使用 Arguments.Unpack（不要用 UnpackIntoInterface）。

JoyueAgent 事件（需与 contracts/joyue/JoyueAgent.sol 保持一致）：
event AgentResult(address indexed master, bytes32 indexed requestId, address indexed user, bytes payload);

JoyueMaster 方法（需与 contracts/joyue/JoyueMaster.sol 保持一致）：
function submitAgentResult(bytes32 requestId, address sender, uint32 fromShardId, bytes calldata payload)
*/

const (
	agentEventABI = `[
		{"anonymous":false,"inputs":[
			{"indexed":true,"internalType":"address","name":"master","type":"address"},
			{"indexed":true,"internalType":"bytes32","name":"requestId","type":"bytes32"},
			{"indexed":true,"internalType":"address","name":"user","type":"address"},
			{"indexed":false,"internalType":"bytes","name":"payload","type":"bytes"}
		],"name":"AgentResult","type":"event"}
	]`
)

type shardRPC struct {
	ID  uint32
	URL string
}

// state 记录每个 shard 扫描到的高度，以及已经处理过的日志 key，避免重复转发
type stateFile struct {
	// 每个 shard 上次处理到的区块（包含）
	LastBlockByShard map[string]uint64 `json:"lastBlockByShard"`
	// processedKey -> txHash（便于排查）
	Processed map[string]string `json:"processed"`
}

func main() {
	var (
		// master shard RPC：用于发送 submitAgentResult 交易
		masterRPC = flag.String("master-rpc", "http://127.0.0.1:9500", "master 分片 RPC（HTTP），用于发送 submitAgentResult")
		// agent shards：需要监听的分片列表（如果 master shard 也部署了 agent，应该把 master shard 也加进来）
		shardsFlag = flag.String("agent-shards", "", "agent 分片列表：id=url,id=url,...（例如 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501，如果 master shard 也部署了 agent 则必须包含 master shard）")

		// 只处理某一个 master 合约（强烈建议填，减少误匹配）
		masterAddrStr = flag.String("master", "", "master 合约地址（0x...），用于 topic 过滤（必填）")

		privateKeyHex = flag.String("private-key", "", "relayer 私钥 hex（不带 0x）")

		fromBlock     = flag.Uint64("from-block", 0, "从哪个区块开始扫（仅在 state-file 没有记录时生效）")
		maxRange      = flag.Uint64("max-range", 2000, "每次 FilterLogs 的最大区块跨度")
		confirmations = flag.Uint64("confirmations", 1, "agent shard 事件确认数（降低重组误触发）")
		poll          = flag.Duration("poll-interval", 3*time.Second, "轮询间隔")

		gasLimit   = flag.Uint64("gas", 500000, "提交 master 的交易 gasLimit")
		gasTipGwei = flag.Int64("gas-tip-gwei", 1, "EIP-1559 priority fee（gwei）")

		waitReceipt    = flag.Bool("wait-receipt", true, "提交到 master 后是否等待 receipt")
		receiptTimeout = flag.Duration("receipt-timeout", 90*time.Second, "等待 receipt 超时时间")

		statePath = flag.String("state-file", "joyue-result-relayer-state.json", "本地 state 文件路径")
		dryRun    = flag.Bool("dry-run", false, "只打印将要做的动作，不发送交易")
	)
	flag.Parse()

	if strings.TrimSpace(*shardsFlag) == "" || strings.TrimSpace(*privateKeyHex) == "" || strings.TrimSpace(*masterAddrStr) == "" {
		flag.Usage()
		log.Fatal("缺少参数：--agent-shards / --private-key / --master")
	}
	masterAddr := common.HexToAddress(*masterAddrStr)

	shards, err := parseShards(*shardsFlag)
	if err != nil {
		log.Fatalf("解析 agent-shards 失败: %v", err)
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].ID < shards[j].ID })

	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*privateKeyHex, "0x"))
	if err != nil {
		log.Fatalf("私钥格式错误: %v", err)
	}
	relayerAddr := crypto.PubkeyToAddress(privKey.PublicKey)

	abiObj, err := abi.JSON(strings.NewReader(agentEventABI))
	if err != nil {
		log.Fatalf("解析 ABI 失败: %v", err)
	}
	agentEvent := abiObj.Events["AgentResult"]

	ctx := context.Background()
	masterClient, err := ethclient.DialContext(ctx, *masterRPC)
	if err != nil {
		log.Fatalf("连接 master RPC 失败: %v", err)
	}

	st := loadState(*statePath)
	if st.LastBlockByShard == nil {
		st.LastBlockByShard = map[string]uint64{}
	}
	if st.Processed == nil {
		st.Processed = map[string]string{}
	}

	log.Printf("JOYUE result relayer 启动: relayer=%s master=%s masterRPC=%s shards=%d confirmations=%d waitReceipt=%v dryRun=%v",
		relayerAddr.Hex(), masterAddr.Hex(), *masterRPC, len(shards), *confirmations, *waitReceipt, *dryRun,
	)

	for {
		for _, sh := range shards {
			if err := tickShard(ctx, tickShardArgs{
				shard:          sh,
				masterAddr:     masterAddr,
				agentEvent:     agentEvent,
				confirmations:  *confirmations,
				fromBlock:      *fromBlock,
				maxRange:       *maxRange,
				masterClient:   masterClient,
				privKey:        privKey,
				relayerAddr:    relayerAddr,
				gasLimit:       *gasLimit,
				gasTipGwei:     *gasTipGwei,
				waitReceipt:    *waitReceipt,
				receiptTimeout: *receiptTimeout,
				dryRun:         *dryRun,
				state:          st,
			}); err != nil {
				log.Printf("[shard=%d] tick error: %v", sh.ID, err)
			}
			saveState(*statePath, st)
		}
		time.Sleep(*poll)
	}
}

type tickShardArgs struct {
	shard         shardRPC
	masterAddr    common.Address
	agentEvent    abi.Event
	confirmations uint64
	fromBlock     uint64
	maxRange      uint64

	masterClient   *ethclient.Client
	privKey        *ecdsa.PrivateKey
	relayerAddr    common.Address
	gasLimit       uint64
	gasTipGwei     int64
	waitReceipt    bool
	receiptTimeout time.Duration
	dryRun         bool

	state *stateFile
}

func tickShard(ctx context.Context, a tickShardArgs) error {
	agentClient, err := ethclient.DialContext(ctx, a.shard.URL)
	if err != nil {
		return err
	}

	// 1) 确定 safeToAct（考虑 confirmations）
	head, err := agentClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return err
	}
	latest := head.Number.Uint64()
	if latest <= a.confirmations {
		return nil
	}
	safeToAct := latest - a.confirmations

	shKey := fmt.Sprintf("%d", a.shard.ID)
	last := a.state.LastBlockByShard[shKey]
	if last == 0 && a.fromBlock != 0 {
		last = a.fromBlock
	}
	from := last + 1
	if from == 0 {
		from = 1
	}
	if from > safeToAct {
		return nil
	}
	to := safeToAct
	if a.maxRange > 0 && to-from > a.maxRange {
		to = from + a.maxRange
	}

	// 2) FilterLogs：topic0=AgentResult，topic1=masterAddr（indexed）
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Topics: [][]common.Hash{
			{a.agentEvent.ID},
			{common.BytesToHash(a.masterAddr.Bytes())},
		},
	}
	logs, err := agentClient.FilterLogs(ctx, q)
	if err != nil {
		return err
	}

	for _, lg := range logs {
		// processedKey：shardID-txHash-logIndex
		pk := fmt.Sprintf("%d-%s-%d", a.shard.ID, lg.TxHash.Hex(), lg.Index)
		if _, ok := a.state.Processed[pk]; ok {
			continue
		}

		ev, err := decodeAgentResult(a.agentEvent, lg)
		if err != nil {
			log.Printf("[shard=%d] skip bad AgentResult: err=%v", a.shard.ID, err)
			a.state.Processed[pk] = "decode-error"
			continue
		}

		if a.dryRun {
			log.Printf("[dry-run] forward result: shard=%d requestId=%s user=%s payloadLen=%d -> master=%s",
				a.shard.ID, ev.RequestID.Hex(), ev.User.Hex(), len(ev.Payload), a.masterAddr.Hex(),
			)
			a.state.Processed[pk] = "dry-run"
			continue
		}

		txHash, err := sendSubmitAgentResult(ctx, a.masterClient, a.privKey, a.relayerAddr, a.masterAddr, ev.RequestID, ev.User, a.shard.ID, ev.Payload, a.gasLimit, a.gasTipGwei, a.waitReceipt, a.receiptTimeout)
		if err != nil {
			log.Printf("[shard=%d] submitAgentResult failed requestId=%s tx=%s err=%v", a.shard.ID, ev.RequestID.Hex(), txHash.Hex(), err)
			// 即便失败也标记处理过，避免死循环（demo 行为）
			a.state.Processed[pk] = "failed:" + txHash.Hex()
			continue
		}
		log.Printf("[shard=%d] forwarded requestId=%s -> master tx=%s", a.shard.ID, ev.RequestID.Hex(), txHash.Hex())
		a.state.Processed[pk] = txHash.Hex()
	}

	a.state.LastBlockByShard[shKey] = to
	return nil
}

type agentResult struct {
	Master    common.Address
	RequestID common.Hash
	User      common.Address
	Payload   []byte
}

func decodeAgentResult(ev abi.Event, lg ethtypes.Log) (*agentResult, error) {
	// topics: [sig, master, requestId, user]
	if len(lg.Topics) < 4 {
		return nil, fmt.Errorf("topics 不足: %d", len(lg.Topics))
	}
	master := common.BytesToAddress(lg.Topics[1].Bytes()[12:])
	requestID := common.BytesToHash(lg.Topics[2].Bytes())
	user := common.BytesToAddress(lg.Topics[3].Bytes()[12:])

	vals, err := ev.Inputs.NonIndexed().Unpack(lg.Data)
	if err != nil {
		return nil, err
	}
	if len(vals) != 1 {
		return nil, fmt.Errorf("non-indexed 字段数量不对: %d", len(vals))
	}
	var payload []byte
	switch v := vals[0].(type) {
	case []byte:
		payload = append(payload[:0:0], v...)
	default:
		return nil, fmt.Errorf("payload 类型未知: %T", vals[0])
	}
	return &agentResult{Master: master, RequestID: requestID, User: user, Payload: payload}, nil
}

// sendSubmitAgentResult 发送 master.submitAgentResult(...) 交易
func sendSubmitAgentResult(
	ctx context.Context,
	client *ethclient.Client,
	privKey *ecdsa.PrivateKey,
	from common.Address,
	master common.Address,
	requestID common.Hash,
	user common.Address,
	fromShardID uint32,
	payload []byte,
	gasLimit uint64,
	gasTipGwei int64,
	waitReceipt bool,
	receiptTimeout time.Duration,
) (common.Hash, error) {
	// calldata = selector + abi.encode(args)
	calldata, err := encodeSubmitAgentResultCall(requestID, user, fromShardID, payload)
	if err != nil {
		return common.Hash{}, err
	}

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, err
	}

	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, err
	}

	var tx *ethtypes.Transaction
	if head.BaseFee != nil {
		tipCap, tipErr := client.SuggestGasTipCap(ctx)
		if tipErr != nil || tipCap == nil {
			tipCap = new(big.Int).Mul(big.NewInt(gasTipGwei), big.NewInt(1_000_000_000))
		}
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tipCap)
		tx = ethtypes.NewTx(&ethtypes.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       gasLimit,
			To:        &master,
			Value:     big.NewInt(0),
			Data:      calldata,
		})
	} else {
		gasPrice, err := client.SuggestGasPrice(ctx)
		if err != nil {
			return common.Hash{}, err
		}
		tx = ethtypes.NewTx(&ethtypes.LegacyTx{
			Nonce:    nonce,
			GasPrice: gasPrice,
			Gas:      gasLimit,
			To:       &master,
			Value:    big.NewInt(0),
			Data:     calldata,
		})
	}

	signer := ethtypes.LatestSignerForChainID(chainID)
	signedTx, err := ethtypes.SignTx(tx, signer, privKey)
	if err != nil {
		return common.Hash{}, err
	}
	log.Printf("[submit] tx=%s to(master)=%s dataLen=%d", signedTx.Hash().Hex(), master.Hex(), len(calldata))

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return signedTx.Hash(), err
	}

	if !waitReceipt {
		return signedTx.Hash(), nil
	}
	ctx2, cancel := context.WithTimeout(ctx, receiptTimeout)
	defer cancel()
	receipt, err := waitReceiptFunc(ctx2, client, signedTx.Hash())
	if err != nil {
		return signedTx.Hash(), err
	}
	if receipt.Status != 1 {
		return signedTx.Hash(), fmt.Errorf("master tx failed status=%d", receipt.Status)
	}
	return signedTx.Hash(), nil
}

func encodeSubmitAgentResultCall(requestID common.Hash, user common.Address, fromShardID uint32, payload []byte) ([]byte, error) {
	// function submitAgentResult(bytes32,address,uint32,bytes)
	sel := crypto.Keccak256([]byte("submitAgentResult(bytes32,address,uint32,bytes)"))[:4]

	tBytes32, err := abi.NewType("bytes32", "", nil)
	if err != nil {
		return nil, err
	}
	tAddr, err := abi.NewType("address", "", nil)
	if err != nil {
		return nil, err
	}
	tU32, err := abi.NewType("uint32", "", nil)
	if err != nil {
		return nil, err
	}
	tBytes, err := abi.NewType("bytes", "", nil)
	if err != nil {
		return nil, err
	}
	args := abi.Arguments{
		{Type: tBytes32},
		{Type: tAddr},
		{Type: tU32},
		{Type: tBytes},
	}
	enc, err := args.Pack(requestID, user, fromShardID, payload)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+len(enc))
	out = append(out, sel...)
	out = append(out, enc...)
	return out, nil
}

func waitReceiptFunc(ctx context.Context, client *ethclient.Client, tx common.Hash) (*ethtypes.Receipt, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		receipt, err := client.TransactionReceipt(ctx, tx)
		if err == nil && receipt != nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func parseShards(s string) ([]shardRPC, error) {
	parts := strings.Split(s, ",")
	out := make([]shardRPC, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad shard entry: %q", p)
		}
		id64, err := strconv.ParseUint(strings.TrimSpace(kv[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad shard id %q: %w", kv[0], err)
		}
		url := strings.TrimSpace(kv[1])
		if url == "" {
			return nil, fmt.Errorf("empty url for shard %d", id64)
		}
		out = append(out, shardRPC{ID: uint32(id64), URL: url})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no shards provided")
	}
	return out, nil
}

func loadState(path string) *stateFile {
	b, err := os.ReadFile(path)
	if err != nil {
		return &stateFile{LastBlockByShard: map[string]uint64{}, Processed: map[string]string{}}
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return &stateFile{LastBlockByShard: map[string]uint64{}, Processed: map[string]string{}}
	}
	if st.LastBlockByShard == nil {
		st.LastBlockByShard = map[string]uint64{}
	}
	if st.Processed == nil {
		st.Processed = map[string]string{}
	}
	return &st
}

func saveState(path string, st *stateFile) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		log.Printf("state marshal error: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("state write error: %v", err)
		return
	}
	_ = os.Rename(tmp, path)
}

func decodeHexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if s == "" {
		return nil, errors.New("empty hex")
	}
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}
