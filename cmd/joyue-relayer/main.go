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
JOYUE relayer（第一阶段：最小可跑通版本）

目标：
1）在 shard0（master shard）部署 JoyueMaster 时，constructor emit 一个 MasterDeployed 事件。
2）Relayer 监听到 MasterDeployed 事件后，自动在其它分片发送“合约创建交易”，部署 JoyueAgent。

注意：
- 这是“链下 relayer”方案，不改共识、不改协议；所以最终一致性依赖网络与交易上链情况。
- 本实现不要求 agent 地址跨分片一致（每个分片部署出来的 agent 地址可能不同）。

实现策略（尽量简单但可用）：
- 轮询 master shard 的 RPC，通过 FilterLogs 扫描指定事件 topic0。
- 对每条事件，逐个 shard 发送 agent 合约创建交易（data = agent creation bytecode + constructor args）。
- 支持：
  - confirmations：等 master shard 事件确认若干块，降低重组误触发。
  - dry-run：只打印不发交易。
  - wait-receipt：发送后可选等待 receipt，并把 agent 合约地址记录到 state 文件里。
  - 本地 state 文件：避免重复处理同一个 (masterAddr, agentShardId)。

兼容性：
- 本仓库 go.mod 把 go-ethereum replace 到 v1.11.2，因此不要使用新版本才有的 API（例如 Arguments.UnpackIntoInterface）。
*/

const (
	// 事件 ABI：必须与 contracts/joyue/JoyueMaster.sol 的 MasterDeployed 一致
	masterEventABI = `[
		{"anonymous":false,"inputs":[
			{"indexed":true,"internalType":"address","name":"master","type":"address"},
			{"indexed":true,"internalType":"bytes32","name":"salt","type":"bytes32"},
			{"indexed":false,"internalType":"bytes","name":"agentCreationCode","type":"bytes"},
			{"indexed":false,"internalType":"uint32","name":"masterShardId","type":"uint32"}
		],"name":"MasterDeployed","type":"event"}
	]`
)

// AgentResult 事件 ABI：必须与 contracts/joyue/JoyueAgent.sol 的 AgentResult 一致
const agentEventABI = `[
  {"anonymous":false,"inputs":[
    {"indexed":true,"internalType":"address","name":"master","type":"address"},
    {"indexed":true,"internalType":"bytes32","name":"requestId","type":"bytes32"},
    {"indexed":true,"internalType":"address","name":"user","type":"address"},
    {"indexed":false,"internalType":"bytes","name":"payload","type":"bytes"}
  ],"name":"AgentResult","type":"event"}
]`

// JoyueMaster.submitAgentResult 的函数签名（用于计算 selector）
const masterSubmitSig = "submitAgentResult(bytes32,address,uint32,bytes)"

// 分片 RPC 配置：例如 0=http://127.0.0.1:9500
type shardRPC struct {
	ID  uint32
	URL string
}

// 本地状态：用于避免重复处理
//
// key = "<masterAddr>-<agentShardId>"
type deployRecord struct {
	// DeployTxHash: 发送部署交易的 hash（可能为空）
	DeployTxHash string `json:"deployTxHash,omitempty"`
	// AgentAddress: 部署成功后 receipt 返回的合约地址（可选；只有 waitReceipt 才会填）
	AgentAddress string `json:"agentAddress,omitempty"`
	// Status: "sent" | "mined" | "failed"
	Status string `json:"status,omitempty"`
	// LastErr: 最近一次错误（便于排查）
	LastErr string `json:"lastErr,omitempty"`
	// ReceiptStatus: receipt.Status（若 waitReceipt=true 且已拿到 receipt）
	ReceiptStatus uint64 `json:"receiptStatus,omitempty"`
	// ReceiptBlock: receipt.BlockNumber（若 waitReceipt=true 且已拿到 receipt）
	ReceiptBlock uint64 `json:"receiptBlock,omitempty"`
	// UpdatedAt: 更新时间
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type stateFile struct {
	// LastBlock: master shard 已扫描到的区块高度（包含）
	LastBlock uint64 `json:"lastBlock"`
	// Deployments: 记录每个 (master, shard) 的部署结果
	Deployments map[string]deployRecord `json:"deployments"`

	// MasterAddress: 最近一次观察到的 master 合约地址（用于方案A转发）
	MasterAddress string `json:"masterAddress,omitempty"`
	// MasterShardID: masterShardId（从事件里读）
	MasterShardID uint32 `json:"masterShardId,omitempty"`

	// LastAgentBlock: 每个 shard 扫描 AgentResult 到的区块高度（包含）
	// key 使用 shardID 的十进制字符串（例如 "1"）
	LastAgentBlock map[string]uint64 `json:"lastAgentBlock,omitempty"`

	// Relayed: 记录已经转发到 master 的 requestId（避免重复转发）
	// key: "<fromShardId>-<requestIdHex>"
	Relayed map[string]string `json:"relayed,omitempty"` // value: tx hash
}

func main() {
	var (
		// master shard（通常 shard0）RPC
		masterRPC = flag.String("master-rpc", "", "master 分片 RPC（HTTP），用于扫描 MasterDeployed 事件")
		// shards 列表：包含 master shard 与其它 shard
		shardsFlag = flag.String("shards", "", "分片列表：id=url,id=url（建议包含 master shard 以及其它 shard）")
		// relayer 私钥：必须在每个 shard 上都有余额
		privKeyHex = flag.String("private-key", "", "relayer 私钥 hex（不带 0x）")
		// 保护阈值：master 事件里携带的 agentCreationCode 如果太大，直接跳过（避免误操作把超大数据广播出去）
		maxAgentCodeBytes = flag.Uint64("max-agent-code-bytes", 256*1024, "允许的最大 agent creation code 字节数（默认 256KB）")

		// 扫描起始高度：若 stateFile.LastBlock=0 则使用它
		fromBlock = flag.Uint64("from-block", 0, "从哪个区块开始扫（仅在 state-file 没有 lastBlock 时生效）")
		// 每次最多扫多少个块（防止 RPC 一次查询太大）
		maxRange = flag.Uint64("max-range", 2000, "每次 FilterLogs 的最大区块跨度")
		// master shard 事件确认块数（降低重组误触发）
		confirmations = flag.Uint64("confirmations", 1, "master shard 事件确认数（例如 2 表示只处理 latest-2 之前的事件）")
		// 轮询间隔
		poll = flag.Duration("poll-interval", 3*time.Second, "轮询间隔")

		// 部署交易 gas limit
		gasLimit = flag.Uint64("gas", 2_500_000, "agent 部署交易 gasLimit")
		// EIP1559 tip（如果链支持 baseFee）；否则走 legacy gasPrice
		gasTipGwei = flag.Int64("gas-tip-gwei", 1, "EIP1559 priority fee（gwei），仅在 baseFee 存在时使用")

		// dry-run：不发交易
		dryRun = flag.Bool("dry-run", false, "只打印将要做的动作，不发送交易")

		// wait-receipt：发送后等待 receipt，并记录 agent 地址
		waitReceipt    = flag.Bool("wait-receipt", false, "发送部署交易后等待 receipt，并记录 agent 合约地址")
		receiptTimeout = flag.Duration("receipt-timeout", 90*time.Second, "等待 receipt 的超时时间")

		// state 文件路径
		statePath = flag.String("state-file", "joyue-relayer-state.json", "本地 state 文件（用于避免重复处理）")

		// 方案A：是否监听 AgentResult 并转发到 master
		forwardAgent       = flag.Bool("forward-agent-result", true, "监听 JoyueAgent.AgentResult 并转发到 JoyueMaster（方案A）")
		agentConfirmations = flag.Uint64("agent-confirmations", 0, "agent shard 事件确认数（默认 0；担心重组可设 1/2）")
	)
	flag.Parse()

	if *masterRPC == "" || *shardsFlag == "" || *privKeyHex == "" {
		flag.Usage()
		os.Exit(2)
	}

	shards, err := parseShards(*shardsFlag)
	if err != nil {
		log.Fatalf("解析 shards 失败: %v", err)
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].ID < shards[j].ID })

	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*privKeyHex, "0x"))
	if err != nil {
		log.Fatalf("私钥格式错误: %v", err)
	}
	relayerAddr := crypto.PubkeyToAddress(privKey.PublicKey)

	mABI, err := abi.JSON(strings.NewReader(masterEventABI))
	if err != nil {
		log.Fatalf("解析 master event ABI 失败: %v", err)
	}
	masterEvent := mABI.Events["MasterDeployed"]

	agentABIObj, err := abi.JSON(strings.NewReader(agentEventABI))
	if err != nil {
		log.Fatalf("解析 agent event ABI 失败: %v", err)
	}
	agentEvent := agentABIObj.Events["AgentResult"]

	ctx := context.Background()
	masterClient, err := ethclient.DialContext(ctx, *masterRPC)
	if err != nil {
		log.Fatalf("连接 master RPC 失败: %v", err)
	}

	st := loadState(*statePath)
	if st.Deployments == nil {
		st.Deployments = map[string]deployRecord{}
	}
	if st.LastAgentBlock == nil {
		st.LastAgentBlock = map[string]uint64{}
	}
	if st.Relayed == nil {
		st.Relayed = map[string]string{}
	}
	// 如果 state 文件没有 lastBlock，则用 from-block 初始化
	if st.LastBlock == 0 && *fromBlock != 0 {
		st.LastBlock = *fromBlock
	}

	log.Printf("JOYUE relayer 启动: relayer=%s masterRPC=%s lastBlock=%d confirmations=%d waitReceipt=%v dryRun=%v",
		relayerAddr.Hex(), *masterRPC, st.LastBlock, *confirmations, *waitReceipt, *dryRun,
	)

	for {
		err := tick(ctx, tickArgs{
			masterClient:      masterClient,
			masterEvent:       masterEvent,
			shards:            shards,
			privKey:           privKey,
			relayerAddr:       relayerAddr,
			maxAgentCodeBytes: *maxAgentCodeBytes,
			confirmations:     *confirmations,
			maxRange:          *maxRange,
			gasLimit:          *gasLimit,
			gasTipGwei:        *gasTipGwei,
			waitReceipt:       *waitReceipt,
			receiptTimeout:    *receiptTimeout,
			dryRun:            *dryRun,
			statePath:         *statePath,
			state:             st,

			forwardAgent:       *forwardAgent,
			agentEvent:         agentEvent,
			agentConfirmations: *agentConfirmations,
		})
		if err != nil {
			log.Printf("tick error: %v", err)
		}
		saveState(*statePath, st)
		time.Sleep(*poll)
	}
}

type tickArgs struct {
	masterClient *ethclient.Client
	masterEvent  abi.Event

	shards            []shardRPC
	privKey           *ecdsa.PrivateKey
	relayerAddr       common.Address
	maxAgentCodeBytes uint64
	confirmations     uint64
	maxRange          uint64
	gasLimit          uint64
	gasTipGwei        int64

	waitReceipt    bool
	receiptTimeout time.Duration
	dryRun         bool

	statePath string
	state     *stateFile

	// 方案A：监听 AgentResult 并转发到 master
	forwardAgent       bool
	agentEvent         abi.Event
	agentConfirmations uint64
}

func tick(ctx context.Context, a tickArgs) error {
	// 1) 获取 master shard 最新高度，并计算 safeToAct（考虑 confirmations）
	head, err := a.masterClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return err
	}
	latest := head.Number.Uint64()
	if latest <= a.confirmations {
		return nil
	}
	safeToAct := latest - a.confirmations

	// 2) 计算本次扫描区间 [from, to]
	from := a.state.LastBlock + 1
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

	// 3) FilterLogs 只按 topic0 过滤（不限制 address）
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Topics:    [][]common.Hash{{a.masterEvent.ID}},
	}
	logs, err := a.masterClient.FilterLogs(ctx, q)
	if err != nil {
		return err
	}

	// 4) 处理每条事件
	for _, lg := range logs {
		evt, err := parseMasterDeployed(a.masterEvent, lg)
		if err != nil {
			log.Printf("跳过无法解析的 MasterDeployed log: err=%v", err)
			continue
		}

		// 4.1 基本保护：避免转发超大 creation code
		if a.maxAgentCodeBytes > 0 && uint64(len(evt.AgentCreationCode)) > a.maxAgentCodeBytes {
			log.Printf("跳过 master=%s：agentCreationCode 太大 len=%d limit=%d",
				evt.Master.Hex(), len(evt.AgentCreationCode), a.maxAgentCodeBytes,
			)
			continue
		}

		// 记录 master 地址与 shardId（供方案A：AgentResult -> Master.submitAgentResult 使用）
		a.state.MasterAddress = evt.Master.Hex()
		a.state.MasterShardID = evt.MasterShardID

		// 4.2 对每个非 master shard 发送部署交易
		for _, shard := range a.shards {
			if shard.ID == evt.MasterShardID {
				continue
			}
			key := makeKey(evt.Master, shard.ID)
			if rec, ok := a.state.Deployments[key]; ok {
				// 已处理过：直接跳过
				_ = rec
				continue
			}

			if a.dryRun {
				log.Printf("[dry-run] 将部署 agent：master=%s masterShard=%d -> agentShard=%d rpc=%s",
					evt.Master.Hex(), evt.MasterShardID, shard.ID, shard.URL,
				)
				a.state.Deployments[key] = deployRecord{
					Status:    "dry-run",
					UpdatedAt: time.Now().Format(time.RFC3339),
				}
				continue
			}

			txHash, agentAddr, err := deployAgent(ctx, deployArgs{
				shard:             shard,
				privKey:           a.privKey,
				relayerAddr:       a.relayerAddr,
				agentCreationCode: evt.AgentCreationCode,
				master:            evt.Master,
				masterShardID:     evt.MasterShardID,
				agentShardID:      shard.ID,
				gasLimit:          a.gasLimit,
				gasTipGwei:        a.gasTipGwei,
				waitReceipt:       a.waitReceipt,
				receiptTimeout:    a.receiptTimeout,
			})
			if err != nil {
				log.Printf("部署失败：master=%s agentShard=%d tx=%s err=%v", evt.Master.Hex(), shard.ID, txHash.Hex(), err)
				a.state.Deployments[key] = deployRecord{
					DeployTxHash: txHash.Hex(),
					Status:       "failed",
					LastErr:      err.Error(),
					UpdatedAt:    time.Now().Format(time.RFC3339),
				}
				continue
			}

			rec := deployRecord{
				DeployTxHash: txHash.Hex(),
				Status:       "sent",
				UpdatedAt:    time.Now().Format(time.RFC3339),
			}
			if (agentAddr != common.Address{}) {
				rec.AgentAddress = agentAddr.Hex()
				rec.Status = "mined"
			}
			a.state.Deployments[key] = rec
			log.Printf("部署已发送：master=%s agentShard=%d tx=%s agent=%s",
				evt.Master.Hex(), shard.ID, txHash.Hex(), rec.AgentAddress,
			)
		}
	}

	// 4.3 方案A：监听 agent shard 的 AgentResult，并转发到 master shard 的 submitAgentResult
	if a.forwardAgent {
		if err := forwardAgentResults(ctx, a); err != nil {
			log.Printf("forwardAgentResults error: %v", err)
		}
	}

	// 5) 更新 lastBlock：表示 [from,to] 都处理过了
	a.state.LastBlock = to
	return nil
}

// forwardAgentResults 扫描每个 shard 的 AgentResult 事件，并把结果转发到 master shard 的 JoyueMaster.submitAgentResult。
//
// 这是方案A的“第二段”：
// user -> agent.execute() -> emit AgentResult -> relayer -> master.submitAgentResult()
func forwardAgentResults(ctx context.Context, a tickArgs) error {
	// 需要知道 master 合约地址与 masterShardId
	if strings.TrimSpace(a.state.MasterAddress) == "" {
		return nil
	}
	masterAddr := common.HexToAddress(a.state.MasterAddress)
	masterShardID := a.state.MasterShardID

	// 找到 master shard 的 RPC
	var masterShardRPC string
	for _, s := range a.shards {
		if s.ID == masterShardID {
			masterShardRPC = s.URL
			break
		}
	}
	if strings.TrimSpace(masterShardRPC) == "" {
		return fmt.Errorf("cannot find master shard rpc for shardID=%d", masterShardID)
	}

	// 遍历所有 shard（除了 master shard）扫描 AgentResult
	for _, shard := range a.shards {
		if shard.ID == masterShardID {
			continue
		}

		client, err := ethclient.DialContext(ctx, shard.URL)
		if err != nil {
			log.Printf("agent shard dial failed shard=%d url=%s err=%v", shard.ID, shard.URL, err)
			continue
		}

		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Printf("agent shard header failed shard=%d err=%v", shard.ID, err)
			continue
		}
		latest := head.Number.Uint64()
		safeToAct := latest
		if latest > a.agentConfirmations {
			safeToAct = latest - a.agentConfirmations
		}

		// 扫描范围
		lastKey := fmt.Sprintf("%d", shard.ID)
		from := a.state.LastAgentBlock[lastKey] + 1
		if from == 0 {
			from = 1
		}
		if from > safeToAct {
			continue
		}
		to := safeToAct
		if a.maxRange > 0 && to-from > a.maxRange {
			to = from + a.maxRange
		}

		q := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(from),
			ToBlock:   new(big.Int).SetUint64(to),
			Topics:    [][]common.Hash{{a.agentEvent.ID}},
		}
		logs, err := client.FilterLogs(ctx, q)
		if err != nil {
			log.Printf("agent shard filterLogs failed shard=%d err=%v", shard.ID, err)
			continue
		}

		for _, lg := range logs {
			ev, err := parseAgentResult(a.agentEvent, lg)
			if err != nil {
				log.Printf("skip bad AgentResult shard=%d err=%v", shard.ID, err)
				continue
			}

			relayKey := fmt.Sprintf("%d-%s", shard.ID, ev.RequestID.Hex())
			if _, ok := a.state.Relayed[relayKey]; ok {
				continue
			}

			// 发送到 master：submitAgentResult(requestId, sender, fromShardId, payload)
			txHash, err := submitAgentResult(ctx, submitArgs{
				masterRPC:      masterShardRPC,
				privKey:        a.privKey,
				relayerAddr:    a.relayerAddr,
				masterAddr:     masterAddr,
				requestID:      ev.RequestID,
				sender:         ev.Sender,
				fromShardID:    shard.ID,
				payload:        ev.Payload,
				gasLimit:       a.gasLimit,
				gasTipGwei:     a.gasTipGwei,
				waitReceipt:    a.waitReceipt,
				receiptTimeout: a.receiptTimeout,
			})
			if err != nil {
				log.Printf("submitAgentResult failed shard=%d request=%s err=%v", shard.ID, ev.RequestID.Hex(), err)
				continue
			}

			a.state.Relayed[relayKey] = txHash.Hex()
			log.Printf("[forward] shard=%d request=%s -> master=%s tx=%s", shard.ID, ev.RequestID.Hex(), masterAddr.Hex(), txHash.Hex())
		}

		// 更新该 shard 的 lastAgentBlock
		a.state.LastAgentBlock[lastKey] = to
	}

	return nil
}

type agentResult struct {
	Master    common.Address
	RequestID common.Hash
	User      common.Address
	Payload   []byte
}

func parseAgentResult(ev abi.Event, lg ethtypes.Log) (*agentResult, error) {
	// topics: [sig, indexed master, indexed requestId, indexed user]
	if len(lg.Topics) < 4 {
		return nil, fmt.Errorf("topics not enough: %d", len(lg.Topics))
	}
	master := common.BytesToAddress(lg.Topics[1].Bytes()[12:])
	req := common.BytesToHash(lg.Topics[2].Bytes())
	user := common.BytesToAddress(lg.Topics[3].Bytes()[12:])

	vals, err := ev.Inputs.NonIndexed().Unpack(lg.Data)
	if err != nil {
		return nil, err
	}
	if len(vals) != 1 {
		return nil, fmt.Errorf("non-indexed field count unexpected: %d", len(vals))
	}
	payload, ok := vals[0].([]byte)
	if !ok {
		return nil, fmt.Errorf("payload type unexpected: %T", vals[0])
	}
	return &agentResult{
		Master:    master,
		RequestID: req,
		User:      user,
		Payload:   append([]byte(nil), payload...),
	}, nil
}

type submitArgs struct {
	masterRPC   string
	privKey     *ecdsa.PrivateKey
	relayerAddr common.Address
	masterAddr  common.Address

	requestID   common.Hash
	sender      common.Address
	fromShardID uint32
	payload     []byte

	gasLimit       uint64
	gasTipGwei     int64
	waitReceipt    bool
	receiptTimeout time.Duration
}

// submitAgentResult 发送合约调用交易到 master shard：JoyueMaster.submitAgentResult(...)
func submitAgentResult(ctx context.Context, a submitArgs) (common.Hash, error) {
	client, err := ethclient.DialContext(ctx, a.masterRPC)
	if err != nil {
		return common.Hash{}, err
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	nonce, err := client.PendingNonceAt(ctx, a.relayerAddr)
	if err != nil {
		return common.Hash{}, err
	}

	// calldata = selector + abi.encode(args...)
	calldata, err := encodeSubmitCalldata(a.requestID, a.sender, a.fromShardID, a.payload)
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
			tipCap = new(big.Int).Mul(big.NewInt(a.gasTipGwei), big.NewInt(1_000_000_000))
		}
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tipCap)
		tx = ethtypes.NewTx(&ethtypes.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       a.gasLimit,
			To:        &a.masterAddr,
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
			Gas:      a.gasLimit,
			To:       &a.masterAddr,
			Value:    big.NewInt(0),
			Data:     calldata,
		})
	}

	signer := ethtypes.LatestSignerForChainID(chainID)
	signedTx, err := ethtypes.SignTx(tx, signer, a.privKey)
	if err != nil {
		return common.Hash{}, err
	}
	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return signedTx.Hash(), err
	}

	if !a.waitReceipt {
		return signedTx.Hash(), nil
	}

	ctx2, cancel := context.WithTimeout(ctx, a.receiptTimeout)
	defer cancel()
	receipt, err := waitReceipt(ctx2, client, signedTx.Hash())
	if err != nil {
		return signedTx.Hash(), err
	}
	if receipt.Status != 1 {
		return signedTx.Hash(), fmt.Errorf("master submit reverted status=%d", receipt.Status)
	}
	return signedTx.Hash(), nil
}

func encodeSubmitCalldata(requestID common.Hash, sender common.Address, fromShardID uint32, payload []byte) ([]byte, error) {
	selector := crypto.Keccak256([]byte(masterSubmitSig))[:4]

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
	enc, err := args.Pack(requestID, sender, fromShardID, payload)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+len(enc))
	out = append(out, selector...)
	out = append(out, enc...)
	return out, nil
}

// masterDeployed 是事件解析后的结构
type masterDeployed struct {
	Master            common.Address
	Salt              [32]byte
	AgentCreationCode []byte
	MasterShardID     uint32
}

// parseMasterDeployed 解析日志：
// - indexed 字段来自 topics
// - 非 indexed 字段来自 log.Data（使用 Arguments.Unpack，兼容 go-ethereum v1.11.x）
func parseMasterDeployed(ev abi.Event, lg ethtypes.Log) (*masterDeployed, error) {
	// topics: [sig, indexed master, indexed salt]
	if len(lg.Topics) < 3 {
		return nil, fmt.Errorf("topics 不足: %d", len(lg.Topics))
	}
	out := &masterDeployed{
		Master: common.BytesToAddress(lg.Topics[1].Bytes()[12:]),
	}
	copy(out.Salt[:], lg.Topics[2].Bytes())

	// 非 indexed：agentCreationCode(bytes) + masterShardId(uint32)
	vals, err := ev.Inputs.NonIndexed().Unpack(lg.Data)
	if err != nil {
		return nil, err
	}
	if len(vals) != 2 {
		return nil, fmt.Errorf("non-indexed 字段数量不对: %d", len(vals))
	}

	// agentCreationCode（动态 bytes，一般会被解成 []byte）
	switch v := vals[0].(type) {
	case []byte:
		out.AgentCreationCode = append(out.AgentCreationCode[:0:0], v...)
	default:
		return nil, fmt.Errorf("agentCreationCode 类型未知: %T", vals[0])
	}

	// masterShardId（不同版本 ABI 解码可能返回 uint32/uint64/*big.Int）
	switch v := vals[1].(type) {
	case uint32:
		out.MasterShardID = v
	case uint64:
		out.MasterShardID = uint32(v)
	case *big.Int:
		out.MasterShardID = uint32(v.Uint64())
	default:
		return nil, fmt.Errorf("masterShardId 类型未知: %T", vals[1])
	}

	return out, nil
}

type deployArgs struct {
	shard       shardRPC
	privKey     *ecdsa.PrivateKey
	relayerAddr common.Address

	// agentCreationCode：完整 creation code（可直接作为合约创建交易 data）
	agentCreationCode []byte

	master        common.Address
	masterShardID uint32
	agentShardID  uint32

	gasLimit   uint64
	gasTipGwei int64

	waitReceipt    bool
	receiptTimeout time.Duration
}

// deployAgent 在指定 shard 上发送“合约创建交易”部署 JoyueAgent
//
// 返回：
// - txHash：交易 hash
// - agentAddr：如果 waitReceipt=true 且部署成功，则返回合约地址；否则返回空地址
func deployAgent(ctx context.Context, a deployArgs) (common.Hash, common.Address, error) {
	client, err := ethclient.DialContext(ctx, a.shard.URL)
	if err != nil {
		return common.Hash{}, common.Address{}, err
	}

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return common.Hash{}, common.Address{}, err
	}

	// nonce：每个 shard 都是独立链状态，所以必须对每个 shard 单独取 nonce
	nonce, err := client.PendingNonceAt(ctx, a.relayerAddr)
	if err != nil {
		return common.Hash{}, common.Address{}, err
	}

	// 直接使用 master 事件携带的“完整 creation code”
	// 注意：这意味着 agent 的 constructor args（如果有）必须已经包含在这段 code 里
	creationData := common.CopyBytes(a.agentCreationCode)

	// gas price：如果 header 有 baseFee，则使用 EIP-1559；否则使用 legacy gasPrice
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, common.Address{}, err
	}

	var tx *ethtypes.Transaction
	if head.BaseFee != nil {
		tipCap, tipErr := client.SuggestGasTipCap(ctx)
		if tipErr != nil || tipCap == nil {
			tipCap = new(big.Int).Mul(big.NewInt(a.gasTipGwei), big.NewInt(1_000_000_000))
		}
		// feeCap = 2*baseFee + tip（简单策略）
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tipCap)
		tx = ethtypes.NewTx(&ethtypes.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       a.gasLimit,
			To:        nil, // 合约创建
			Value:     big.NewInt(0),
			Data:      creationData,
		})
	} else {
		gasPrice, err := client.SuggestGasPrice(ctx)
		if err != nil {
			return common.Hash{}, common.Address{}, err
		}
		tx = ethtypes.NewTx(&ethtypes.LegacyTx{
			Nonce:    nonce,
			GasPrice: gasPrice,
			Gas:      a.gasLimit,
			To:       nil, // 合约创建
			Value:    big.NewInt(0),
			Data:     creationData,
		})
	}

	signer := ethtypes.LatestSignerForChainID(chainID)
	signedTx, err := ethtypes.SignTx(tx, signer, a.privKey)
	if err != nil {
		return common.Hash{}, common.Address{}, err
	}
	// 打印一次 txHash，方便你在失败时也能搜日志/查 receipt
	log.Printf("[deployAgent] shard=%d tx=%s dataLen=%d gas=%d", a.shard.ID, signedTx.Hash().Hex(), len(creationData), a.gasLimit)
	if err := client.SendTransaction(ctx, signedTx); err != nil {
		// 发送失败也把 txhash 带回去（便于日志对齐）
		return signedTx.Hash(), common.Address{}, err
	}

	// 可选：等待 receipt，得到合约地址与 status
	if !a.waitReceipt {
		return signedTx.Hash(), common.Address{}, nil
	}

	ctx2, cancel := context.WithTimeout(ctx, a.receiptTimeout)
	defer cancel()
	fmt.Printf("交易哈希: %s\n", signedTx.Hash().Hex())
	receipt, err := waitReceipt(ctx2, client, signedTx.Hash())
	if err != nil {
		return signedTx.Hash(), common.Address{}, err
	}
	if receipt.Status != 1 {
		// 即便失败，也把 txhash 返回给上层用于排查（receipt/日志查询）
		return signedTx.Hash(), receipt.ContractAddress, fmt.Errorf("部署交易执行失败 status=%d", receipt.Status)
	}
	return signedTx.Hash(), receipt.ContractAddress, nil
}

func waitReceipt(ctx context.Context, client *ethclient.Client, tx common.Hash) (*ethtypes.Receipt, error) {
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

func makeKey(master common.Address, agentShardID uint32) string {
	return fmt.Sprintf("%s-%d", master.Hex(), agentShardID)
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
		// 文件不存在：返回空 state
		return &stateFile{Deployments: map[string]deployRecord{}}
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		// state 文件坏了：为了安全，直接重新开始（你也可以手动备份）
		return &stateFile{Deployments: map[string]deployRecord{}}
	}
	if st.Deployments == nil {
		st.Deployments = map[string]deployRecord{}
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
