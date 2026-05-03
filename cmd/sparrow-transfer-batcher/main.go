/*
Sparrow Transfer Batcher — 监听 SparrowTransferIntent（IntentShop 发事件前调 Wallet precheckLine：不检查锁，仅 from 余额等），聚批后调用 SparrowTransferCoordinator.transferWave

单分片：

	go run ./cmd/sparrow-transfer-batcher --rpc http://127.0.0.1:9500 \
	  --intent-shop 0x... --coordinator 0x... --private-key <hex>

多分片（与 sparrow-batcher 相同 KV 格式）：

	go run ./cmd/sparrow-transfer-batcher \
	  --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9502 \
	  --intent-shops 0=0x...,1=0x... \
	  --coordinators 0=0x...,1=0x... \
	  --private-key <hex>

Intent 与 Coordinator 不同分片时加 --coordinator-rpc（同 sparrow-batcher）。
*/
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"flag"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// SparrowTransferIntent(address indexed from, address indexed to, uint256 amount, bytes32 intentId, bool precheckOk)
var sparrowTransferIntentSig = crypto.Keccak256Hash([]byte("SparrowTransferIntent(address,address,uint256,bytes32,bool)"))

type pendingTransferIntent struct {
	From       common.Address
	To         common.Address
	Amount     *big.Int
	IntentID   common.Hash
	PrecheckOk bool
}

type transferWaveItem struct {
	From             common.Address `abi:"from"`
	To               common.Address `abi:"to"`
	Amount           *big.Int       `abi:"amount"`
	TxID             [32]byte       `abi:"txId"`
	IntentPrecheckOk bool           `abi:"intentPrecheckOk"`
}

const coordinatorABIJSON = `[{"name":"transferWave","type":"function","stateMutability":"nonpayable","inputs":[{"name":"items","type":"tuple[]","components":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"},{"name":"txId","type":"bytes32"},{"name":"intentPrecheckOk","type":"bool"}]}],"outputs":[]}]`

func main() {
	rpcs := flag.String("rpcs", "", "多分片 RPC")
	intentShops := flag.String("intent-shops", "", "多分片 SparrowTransferIntentShop 地址")
	coordinators := flag.String("coordinators", "", "多分片 SparrowTransferCoordinator 地址")
	rpcURL := flag.String("rpc", "", "单分片 RPC")
	intentShop := flag.String("intent-shop", "", "单分片 Intent 地址")
	coordinator := flag.String("coordinator", "", "单分片 Coordinator 地址")
	shardID := flag.String("shard-id", "0", "单分片日志标签")
	privHex := flag.String("private-key", "", "私钥 hex")
	batchMs := flag.Uint("batch-interval-ms", 500, "批间隔 ms")
	maxBatch := flag.Uint("max-batch", 64, "满批条数")
	pollMs := flag.Uint("poll-ms", 500, "扫链间隔 ms")
	fromBlock := flag.Uint64("from-block", 0, "起始区块，0=当前链头")
	gasLimit := flag.Uint64("gas", 3_000_000, "transferWave gas")
	gasTipGwei := flag.Int64("gas-tip-gwei", 1, "")
	coordinatorRPC := flag.String("coordinator-rpc", "", "提交用 RPC（Coordinator 所在分片）")
	flag.Parse()

	rpcMulti := strings.TrimSpace(*rpcs)
	shopMulti := strings.TrimSpace(*intentShops)
	coordMulti := strings.TrimSpace(*coordinators)
	rpcSingle := strings.TrimSpace(*rpcURL)
	shopSingle := strings.TrimSpace(*intentShop)
	coordSingle := strings.TrimSpace(*coordinator)
	if rpcMulti == "" && rpcSingle != "" && looksLikeShardRPCList(rpcSingle) {
		log.Printf("[WARN] 多分片应使用 --rpcs；已从 --rpc 迁移")
		rpcMulti, rpcSingle = rpcSingle, ""
	}
	if rpcMulti != "" && shopMulti == "" && shopSingle != "" && strings.Contains(shopSingle, "=") {
		log.Printf("[WARN] 多分片应使用 --intent-shops；已从 --intent-shop 迁移")
		shopMulti, shopSingle = shopSingle, ""
	}
	if rpcMulti != "" && coordMulti == "" && coordSingle != "" && strings.Contains(coordSingle, "=") {
		log.Printf("[WARN] 多分片应使用 --coordinators；已从 --coordinator 迁移")
		coordMulti, coordSingle = coordSingle, ""
	}

	type shardPair struct {
		id    string
		rpc   string
		shop  common.Address
		coord common.Address
	}
	var shards []shardPair

	if rpcMulti != "" {
		if shopMulti == "" || coordMulti == "" {
			flag.Usage()
			log.Fatal("多分片需 --rpcs、--intent-shops、--coordinators")
		}
		rpcMap := parseKv(rpcMulti)
		shopMap := parseAddrMap(shopMulti)
		coordMap := parseAddrMap(coordMulti)
		for sid, url := range rpcMap {
			shop := shopMap[sid]
			if shop == (common.Address{}) {
				shop = shopMap["0"]
			}
			coord := coordMap[sid]
			if coord == (common.Address{}) {
				coord = coordMap["0"]
			}
			if shop == (common.Address{}) || coord == (common.Address{}) {
				log.Printf("[WARN] 分片 %s 缺 intent 或 coordinator，跳过", sid)
				continue
			}
			shards = append(shards, shardPair{id: sid, rpc: url, shop: shop, coord: coord})
		}
	} else {
		if rpcSingle == "" || shopSingle == "" || coordSingle == "" || *privHex == "" {
			flag.Usage()
			os.Exit(1)
		}
		sid := strings.TrimSpace(*shardID)
		if sid == "" {
			sid = "0"
		}
		shards = []shardPair{{id: sid, rpc: rpcSingle, shop: common.HexToAddress(shopSingle), coord: common.HexToAddress(coordSingle)}}
	}

	if len(shards) == 0 {
		log.Fatal("没有可用分片配置")
	}
	if *privHex == "" {
		log.Fatal("缺少 --private-key")
	}

	priv, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(*privHex), "0x"))
	if err != nil {
		log.Fatalf("私钥无效: %v", err)
	}

	coordinatorABI, err := abi.JSON(strings.NewReader(coordinatorABIJSON))
	if err != nil {
		log.Fatalf("ABI: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	submitDefault := strings.TrimSpace(*coordinatorRPC)
	var sendMu *sync.Mutex
	if len(shards) > 1 {
		sendMu = &sync.Mutex{}
	}

	for _, sh := range shards {
		sh := sh
		submitRPC := sh.rpc
		if submitDefault != "" {
			submitRPC = submitDefault
		}
		log.Printf("[sparrow-transfer-batcher] shard %s listen=%s submit=%s intent=%s coord=%s", sh.id, sh.rpc, submitRPC, sh.shop.Hex(), sh.coord.Hex())
		go runShard(ctx, sh.id, sh.rpc, submitRPC, sh.shop, sh.coord, priv, coordinatorABI, sendMu, *fromBlock, *batchMs, *maxBatch, *pollMs, *gasLimit, *gasTipGwei)
	}

	<-ctx.Done()
	log.Printf("[sparrow-transfer-batcher] stopping")
}

func looksLikeShardRPCList(s string) bool {
	kv := parseKv(s)
	if len(kv) == 0 {
		return false
	}
	for _, v := range kv {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return false
		}
	}
	return true
}

func parseKv(s string) map[string]string {
	m := make(map[string]string)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		if k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

func parseAddrMap(s string) map[string]common.Address {
	m := make(map[string]common.Address)
	s = strings.TrimSpace(s)
	if s == "" {
		return m
	}
	if !strings.Contains(s, "=") {
		m["0"] = common.HexToAddress(s)
		return m
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		if k != "" && v != "" {
			m[k] = common.HexToAddress(v)
		}
	}
	return m
}

func runShard(ctx context.Context, shardID, listenRPC, submitRPC string, shopAddr, coordAddr common.Address, priv *ecdsa.PrivateKey, coordinatorABI abi.ABI, sendMu *sync.Mutex, fromBlockFlag uint64, batchMs, maxBatch, pollMs uint, gasLimit uint64, gasTipGwei int64) {
	listenClient, err := ethclient.DialContext(ctx, listenRPC)
	if err != nil {
		log.Printf("[shard %s] listen: %v", shardID, err)
		return
	}
	defer listenClient.Close()

	submitClient := listenClient
	if submitRPC != listenRPC {
		c, err := ethclient.DialContext(ctx, submitRPC)
		if err != nil {
			log.Printf("[shard %s] submit: %v", shardID, err)
			return
		}
		defer c.Close()
		submitClient = c
	}

	chainID, err := submitClient.ChainID(ctx)
	if err != nil {
		log.Printf("[shard %s] chainID: %v", shardID, err)
		return
	}

	var lastScannedTo uint64
	if fromBlockFlag > 0 {
		if fromBlockFlag > 1 {
			lastScannedTo = fromBlockFlag - 1
		}
	} else {
		head, err := listenClient.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Printf("[shard %s] header: %v", shardID, err)
			return
		}
		hn := head.Number.Uint64()
		if hn > 0 {
			lastScannedTo = hn - 1
		}
	}

	var mu sync.Mutex
	buffer := make([]pendingTransferIntent, 0, maxBatch)
	processed := make(map[common.Hash]struct{})

	sendWave := func(items []transferWaveItem) {
		if len(items) == 0 {
			return
		}
		calldata, err := coordinatorABI.Pack("transferWave", items)
		if err != nil {
			log.Printf("[shard %s] Pack transferWave: %v", shardID, err)
			return
		}
		if sendMu != nil {
			sendMu.Lock()
			defer sendMu.Unlock()
		}
		from := crypto.PubkeyToAddress(priv.PublicKey)
		nonce, err := submitClient.PendingNonceAt(ctx, from)
		if err != nil {
			log.Printf("[shard %s] nonce: %v", shardID, err)
			return
		}
		head, err := submitClient.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Printf("[shard %s] header: %v", shardID, err)
			return
		}
		signer := types.LatestSignerForChainID(chainID)
		tip := new(big.Int).Mul(big.NewInt(gasTipGwei), big.NewInt(1_000_000_000))
		var tx *types.Transaction
		if head.BaseFee != nil {
			feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
			tx = types.NewTx(&types.DynamicFeeTx{
				ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
				Gas: gasLimit, To: &coordAddr, Value: big.NewInt(0), Data: calldata,
			})
		} else {
			gp, err := submitClient.SuggestGasPrice(ctx)
			if err != nil {
				log.Printf("[shard %s] gasPrice: %v", shardID, err)
				return
			}
			tx = types.NewTx(&types.LegacyTx{
				Nonce: nonce, GasPrice: gp, Gas: gasLimit, To: &coordAddr, Value: big.NewInt(0), Data: calldata,
			})
		}
		stx, err := types.SignTx(tx, signer, priv)
		if err != nil {
			log.Printf("[shard %s] sign: %v", shardID, err)
			return
		}
		if err := submitClient.SendTransaction(ctx, stx); err != nil {
			log.Printf("[shard %s] send transferWave: %v", shardID, err)
			return
		}
		log.Printf("[shard %s] transferWave sent: %s (n=%d)", shardID, stx.Hash().Hex(), len(items))
	}

	drain := func() []transferWaveItem {
		mu.Lock()
		defer mu.Unlock()
		if len(buffer) == 0 {
			return nil
		}
		items := make([]transferWaveItem, len(buffer))
		for i := range buffer {
			p := buffer[i]
			var tid [32]byte
			copy(tid[:], p.IntentID.Bytes())
			items[i] = transferWaveItem{
				From: p.From, To: p.To, Amount: new(big.Int).Set(p.Amount), TxID: tid,
				IntentPrecheckOk: p.PrecheckOk,
			}
		}
		buffer = buffer[:0]
		return items
	}

	flush := func() { sendWave(drain()) }

	ticker := time.NewTicker(time.Duration(batchMs) * time.Millisecond)
	defer ticker.Stop()
	pollTicker := time.NewTicker(time.Duration(pollMs) * time.Millisecond)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			flush()
		case <-pollTicker.C:
			head, err := listenClient.HeaderByNumber(ctx, nil)
			if err != nil {
				continue
			}
			hn := head.Number.Uint64()
			scanFrom := lastScannedTo
			if scanFrom > 0 {
				scanFrom--
			}
			logs, err := listenClient.FilterLogs(ctx, ethereum.FilterQuery{
				FromBlock: big.NewInt(int64(scanFrom)),
				ToBlock:   head.Number,
				Addresses: []common.Address{shopAddr},
				Topics:    [][]common.Hash{{sparrowTransferIntentSig}},
			})
			if err != nil {
				log.Printf("[shard %s] FilterLogs: %v", shardID, err)
				continue
			}
			lastScannedTo = hn

			mu.Lock()
			for _, lg := range logs {
				key := logDedupKey(lg)
				if _, ok := processed[key]; ok {
					continue
				}
				processed[key] = struct{}{}
				from, to, amt, iid, preOk, ok := parseTransferIntentLog(lg)
				if !ok {
					continue
				}
				buffer = append(buffer, pendingTransferIntent{From: from, To: to, Amount: amt, IntentID: iid, PrecheckOk: preOk})
				if uint(len(buffer)) >= maxBatch {
					mu.Unlock()
					flush()
					mu.Lock()
				}
			}
			mu.Unlock()
		}
	}
}

func logDedupKey(l types.Log) common.Hash {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(l.Index))
	return crypto.Keccak256Hash(l.TxHash.Bytes(), b[:])
}

func parseTransferIntentLog(l types.Log) (from, to common.Address, amount *big.Int, intentID common.Hash, precheckOk, ok bool) {
	if len(l.Topics) != 3 || len(l.Data) < 96 {
		return
	}
	if l.Topics[0] != sparrowTransferIntentSig {
		return
	}
	from = common.BytesToAddress(l.Topics[1].Bytes()[12:])
	to = common.BytesToAddress(l.Topics[2].Bytes()[12:])
	amount = new(big.Int).SetBytes(l.Data[:32])
	intentID = common.BytesToHash(l.Data[32:64])
	precheckOk = new(big.Int).SetBytes(l.Data[64:96]).Sign() != 0
	ok = true
	return
}
