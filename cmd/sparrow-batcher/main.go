/*
Sparrow Batcher — 模拟 Relayer 批处理

监听 SparrowIntentShop 的 SparrowBuyIntent，在内存中累积意图；
按定时或达到 max-batch 触发，将本批所有意图编码为 SparrowCoordinator.buyFruitWave（含 BuyItem.txId = intentId）。

单分片：

	go run ./cmd/sparrow-batcher --rpc http://127.0.0.1:9500 \
	  --intent-shop 0x... --coordinator 0x... --private-key <hex>

多分片（每分片独立 buffer；格式对齐 joyue-metrics --rpcs）：

	go run ./cmd/sparrow-batcher \
	  --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501 \
	  --intent-shops 0=0x...,1=0x... \
	  --coordinators 0=0x...,1=0x... \
	  --private-key <hex>

Intent 在 shard1、Coordinator 仅在 shard0（监听与提交异 RPC）：

	go run ./cmd/sparrow-batcher \
	  --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9501 \
	  --intent-shops 0=0x...,1=0x... \
	  --coordinators 0=0x...,1=0x... \
	  --coordinator-rpc http://127.0.0.1:9500 \
	  --private-key <hex>
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

// SparrowBuyIntent(address indexed buyer, bytes32 indexed fruitType, uint256 quantity, bytes32 intentId)
var sparrowBuyIntentSig = crypto.Keccak256Hash([]byte("SparrowBuyIntent(address,bytes32,uint256,bytes32)"))

type pendingIntent struct {
	Buyer     common.Address
	FruitType common.Hash
	Quantity  *big.Int
	IntentID  common.Hash
}

type buyItem struct {
	Buyer     common.Address `abi:"buyer"`
	FruitType [32]byte       `abi:"fruitType"`
	Quantity  *big.Int       `abi:"quantity"`
	TxID      [32]byte       `abi:"txId"`
}

const coordinatorABIJSON = `[{"name":"buyFruitWave","type":"function","stateMutability":"nonpayable","inputs":[{"name":"items","type":"tuple[]","components":[{"name":"buyer","type":"address"},{"name":"fruitType","type":"bytes32"},{"name":"quantity","type":"uint256"},{"name":"txId","type":"bytes32"}]}],"outputs":[]}]`

func main() {
	rpcs := flag.String("rpcs", "", "多分片 RPC：0=http://...,1=http://...")
	intentShops := flag.String("intent-shops", "", "多分片 IntentShop：0=0x...,1=0x...")
	coordinators := flag.String("coordinators", "", "多分片 Coordinator：0=0x...,1=0x...")
	rpcURL := flag.String("rpc", "", "单分片：与 IntentShop 同分片 RPC")
	intentShop := flag.String("intent-shop", "", "单分片：SparrowIntentShop 地址")
	coordinator := flag.String("coordinator", "", "单分片：SparrowCoordinator 地址")
	shardID := flag.String("shard-id", "0", "单分片：日志标签（默认 0）")
	privHex := flag.String("private-key", "", "发送 buyFruitWave 的私钥 hex（可带 0x）")
	batchMs := flag.Uint("batch-interval-ms", 500, "最长等待多久后刷新一批（毫秒）")
	maxBatch := flag.Uint("max-batch", 64, "达到该条数立即刷新一批")
	pollMs := flag.Uint("poll-ms", 500, "扫链间隔（毫秒）")
	fromBlock := flag.Uint64("from-block", 0, "起始区块，0 表示当前链头（各分片独立推进）")
	gasLimit := flag.Uint64("gas", 3_000_000, "buyFruitWave gas")
	gasTipGwei := flag.Int64("gas-tip-gwei", 1, "")
	coordinatorRPC := flag.String("coordinator-rpc", "", "可选：发送 buyFruitWave 使用的 RPC（Coordinator 所在分片）。留空则与各路监听 rpc 相同。Intent 与 Coordinator 不同分片时必须设为 Coordinator 分片")
	flag.Parse()

	rpcMulti := strings.TrimSpace(*rpcs)
	shopMulti := strings.TrimSpace(*intentShops)
	coordMulti := strings.TrimSpace(*coordinators)
	rpcSingle := strings.TrimSpace(*rpcURL)
	shopSingle := strings.TrimSpace(*intentShop)
	coordSingle := strings.TrimSpace(*coordinator)
	if rpcMulti == "" && rpcSingle != "" && looksLikeShardRPCList(rpcSingle) {
		log.Printf("[WARN] 多分片 RPC 应使用 --rpcs（不是 --rpc）；已按多分片自动解析当前 --rpc 值")
		rpcMulti, rpcSingle = rpcSingle, ""
	}
	if rpcMulti != "" && shopMulti == "" && shopSingle != "" && strings.Contains(shopSingle, "=") {
		log.Printf("[WARN] 多分片应使用 --intent-shops（不是 --intent-shop）；已自动解析当前 --intent-shop 值")
		shopMulti, shopSingle = shopSingle, ""
	}
	if rpcMulti != "" && coordMulti == "" && coordSingle != "" && strings.Contains(coordSingle, "=") {
		log.Printf("[WARN] 多分片应使用 --coordinators（不是 --coordinator）；已自动解析当前 --coordinator 值")
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
			log.Fatal("多分片需同时指定 --rpcs、--intent-shops、--coordinators（或把 kv 列表误写在单分片参数上，程序会尝试自动迁移）")
		}
		rpcMap := parseKv(rpcMulti)
		shopMap := parseAddrMap(shopMulti)
		coordMap := parseAddrMap(coordMulti)
		if len(rpcMap) == 0 {
			log.Fatal("--rpcs 解析失败")
		}
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
				log.Printf("[WARN] 分片 %s 缺少 intent-shop 或 coordinator，跳过", sid)
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
		shards = []shardPair{
			{id: sid, rpc: rpcSingle, shop: common.HexToAddress(shopSingle), coord: common.HexToAddress(coordSingle)},
		}
	}

	if len(shards) == 0 {
		log.Fatal("没有可用的分片配置")
	}
	if *privHex == "" {
		flag.Usage()
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
		if submitRPC != sh.rpc {
			log.Printf("[sparrow-batcher] 分片 %s: listen=%s submit=%s shop=%s coord=%s", sh.id, sh.rpc, submitRPC, sh.shop.Hex(), sh.coord.Hex())
		} else {
			log.Printf("[sparrow-batcher] 分片 %s: rpc=%s shop=%s coord=%s", sh.id, sh.rpc, sh.shop.Hex(), sh.coord.Hex())
		}
		go runShardBatcher(ctx, sh.id, sh.rpc, submitRPC, sh.shop, sh.coord, priv, coordinatorABI, sendMu, *fromBlock, *batchMs, *maxBatch, *pollMs, *gasLimit, *gasTipGwei)
	}

	<-ctx.Done()
	log.Printf("[sparrow-batcher] stopping")
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

func runShardBatcher(ctx context.Context, shardID, listenRPC, submitRPC string, shopAddr, coordAddr common.Address, priv *ecdsa.PrivateKey, coordinatorABI abi.ABI, sendMu *sync.Mutex, fromBlockFlag uint64, batchMs, maxBatch, pollMs uint, gasLimit uint64, gasTipGwei int64) {
	listenClient, err := ethclient.DialContext(ctx, listenRPC)
	if err != nil {
		log.Printf("[shard %s] listen RPC: %v", shardID, err)
		return
	}
	defer listenClient.Close()

	submitClient := listenClient
	if submitRPC != listenRPC {
		c, err := ethclient.DialContext(ctx, submitRPC)
		if err != nil {
			log.Printf("[shard %s] submit RPC: %v", shardID, err)
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
	log.Printf("[shard %s] overlap scan (lastTo=%d), shop=%s coord=%s", shardID, lastScannedTo, shopAddr.Hex(), coordAddr.Hex())

	var mu sync.Mutex
	buffer := make([]pendingIntent, 0, maxBatch)
	processed := make(map[common.Hash]struct{})

	sendBuyFruitWave := func(items []buyItem) {
		if len(items) == 0 {
			return
		}
		calldata, err := coordinatorABI.Pack("buyFruitWave", items)
		if err != nil {
			log.Printf("[shard %s] [ERROR] Pack buyFruitWave: %v", shardID, err)
			return
		}
		if sendMu != nil {
			sendMu.Lock()
			defer sendMu.Unlock()
		}
		from := crypto.PubkeyToAddress(priv.PublicKey)
		nonce, err := submitClient.PendingNonceAt(ctx, from)
		if err != nil {
			log.Printf("[shard %s] [ERROR] nonce: %v", shardID, err)
			return
		}
		head, err := submitClient.HeaderByNumber(ctx, nil)
		if err != nil {
			log.Printf("[shard %s] [ERROR] header: %v", shardID, err)
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
				log.Printf("[shard %s] [ERROR] gasPrice: %v", shardID, err)
				return
			}
			tx = types.NewTx(&types.LegacyTx{
				Nonce: nonce, GasPrice: gp, Gas: gasLimit, To: &coordAddr, Value: big.NewInt(0), Data: calldata,
			})
		}
		stx, err := types.SignTx(tx, signer, priv)
		if err != nil {
			log.Printf("[shard %s] [ERROR] sign: %v", shardID, err)
			return
		}
		if err := submitClient.SendTransaction(ctx, stx); err != nil {
			log.Printf("[shard %s] [ERROR] send buyFruitWave: %v", shardID, err)
			return
		}
		log.Printf("[shard %s] [INFO] buyFruitWave sent: %s (n=%d items)", shardID, stx.Hash().Hex(), len(items))
	}

	drainToBuyItems := func() []buyItem {
		mu.Lock()
		defer mu.Unlock()
		if len(buffer) == 0 {
			return nil
		}
		items := make([]buyItem, len(buffer))
		for i := range buffer {
			p := buffer[i]
			var ft [32]byte
			copy(ft[:], p.FruitType.Bytes())
			var tid [32]byte
			copy(tid[:], p.IntentID.Bytes())
			items[i] = buyItem{Buyer: p.Buyer, FruitType: ft, Quantity: new(big.Int).Set(p.Quantity), TxID: tid}
		}
		buffer = buffer[:0]
		return items
	}

	flush := func() { sendBuyFruitWave(drainToBuyItems()) }

	ticker := time.NewTicker(time.Duration(batchMs) * time.Millisecond)
	defer ticker.Stop()
	pollTicker := time.NewTicker(time.Duration(pollMs) * time.Millisecond)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[shard %s] stopping", shardID)
			return
		case <-ticker.C:
			flush()
		case <-pollTicker.C:
			head, err := listenClient.HeaderByNumber(ctx, nil)
			if err != nil {
				log.Printf("[shard %s] [WARN] HeaderByNumber: %v", shardID, err)
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
				Topics:    [][]common.Hash{{sparrowBuyIntentSig}},
			})
			if err != nil {
				log.Printf("[shard %s] [WARN] FilterLogs: %v", shardID, err)
				continue
			}
			lastScannedTo = hn
			if len(logs) > 0 {
				log.Printf("[shard %s] [INFO] FilterLogs blocks %d-%d: %d SparrowBuyIntent log(s)", shardID, scanFrom, hn, len(logs))
			}

			mu.Lock()
			for _, lg := range logs {
				key := logDedupKey(lg)
				if _, ok := processed[key]; ok {
					continue
				}
				processed[key] = struct{}{}
				buyer, fruit, qty, iid, ok := parseIntentLog(lg)
				if !ok {
					t0 := ""
					if len(lg.Topics) > 0 {
						t0 = lg.Topics[0].Hex()
					}
					log.Printf("[shard %s] [WARN] skip malformed log tx=%s topics=%d dataLen=%d topic0=%s", shardID, lg.TxHash.Hex(), len(lg.Topics), len(lg.Data), t0)
					continue
				}
				buffer = append(buffer, pendingIntent{Buyer: buyer, FruitType: fruit, Quantity: qty, IntentID: iid})
				log.Printf("[shard %s] [DEBUG] intent buyer=%s fruit=%x qty=%s txId=%s", shardID, buyer.Hex(), fruit.Bytes()[:4], qty.String(), iid.Hex())
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

func parseIntentLog(l types.Log) (buyer common.Address, fruit common.Hash, qty *big.Int, intentID common.Hash, ok bool) {
	if len(l.Topics) != 3 || len(l.Data) < 64 {
		return
	}
	if l.Topics[0] != sparrowBuyIntentSig {
		return
	}
	buyer = common.BytesToAddress(l.Topics[1].Bytes()[12:])
	fruit = l.Topics[2]
	qty = new(big.Int).SetBytes(l.Data[:32])
	intentID = common.BytesToHash(l.Data[32:64])
	ok = true
	return
}
