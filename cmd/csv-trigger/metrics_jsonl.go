package main

import (
	"encoding/json"
	"os"
)

// writeSentMetricsJSONL 写入与 joyue-trigger 兼容的一行 JSONL（供 twopc-metrics / joyue-metrics 消费）。
func writeSentMetricsJSONL(w *os.File, txHash, txID, shardID string, sendTime int64, blockNumber, blockTime uint64) {
	rec := map[string]interface{}{
		"event":     "sent",
		"tx_hash":   txHash,
		"send_time": sendTime,
		"shard_id":  shardID,
	}
	if txID != "" {
		rec["tx_id"] = txID
	}
	if blockNumber > 0 {
		rec["block_number"] = blockNumber
	}
	if blockTime > 0 {
		rec["receipt_block_time"] = blockTime * 1000
	}
	_ = json.NewEncoder(w).Encode(rec)
}
