package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/internal/joyuetrigger"
)

// pendingMetric 与 joyue-trigger 的 pendingTx 类似：发交易后不阻塞，由后台轮询 receipt。
type pendingMetric struct {
	txHash     string
	sendTime   int64
	rpcURL     string // 多片时用于从 pool 取对应 client 拉 receipt
	shardLabel string // 写入 metrics 的 shard_id；空则回退为调用方传入的 defaultShardID
}

// runReceiptCollector 在单独 goroutine 中轮询 TransactionReceipt，收到后写 JSONL（与 joyue-trigger/receiptCollector 同思路）。
func runReceiptCollector(ctx context.Context, pool *rpcClientPool, pendingCh <-chan pendingMetric, metricsFile *os.File, is2PC bool, defaultShardID string, poll time.Duration) {
	pending := make([]pendingMetric, 0, 256)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	ch := pendingCh
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-ch:
			if !ok {
				ch = nil
			} else {
				pending = append(pending, item)
			}
		case <-ticker.C:
			if len(pending) == 0 {
				continue
			}
			remaining := pending[:0]
			for _, p := range pending {
				c, err := pool.Get(ctx, p.rpcURL)
				if err != nil {
					log.Printf("[csv-trigger] metrics: dial %q: %v", p.rpcURL, err)
					remaining = append(remaining, p)
					continue
				}
				rec, err := c.TransactionReceipt(ctx, common.HexToHash(p.txHash))
				if err != nil || rec == nil {
					remaining = append(remaining, p)
					continue
				}
				var blockNum uint64
				if rec.BlockNumber != nil {
					blockNum = rec.BlockNumber.Uint64()
				}
				var blockTime uint64
				if blk, err := c.BlockByHash(ctx, rec.BlockHash); err == nil && blk != nil {
					blockTime = blk.Time()
				}
				txID := joyuetrigger.ParseTxIDFromReceipt(rec, is2PC)
				if rec.Status != 1 {
					log.Printf("[csv-trigger] metrics: tx %s receipt status=%d gas_used=%d (若大量失败：提高 -gas；chainspace 另查 UserClient.shard 的 Simulator 是否已对 sender 执行 bootstrap -kind chainspace)", p.txHash, rec.Status, rec.GasUsed)
				}
				shard := p.shardLabel
				if shard == "" {
					shard = defaultShardID
				}
				writeSentMetricsJSONL(metricsFile, p.txHash, txID, shard, p.sendTime, blockNum, blockTime)
			}
			pending = remaining
		}
		if ch == nil && len(pending) == 0 {
			return
		}
	}
}
