package joyuemetrics

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	agentResultEmittedSig = crypto.Keccak256Hash([]byte("AgentResultEmitted(bytes32,bool,uint8)"))
	intentRejectedSig     = crypto.Keccak256Hash([]byte("IntentRejected(bytes32,uint8)"))
)

// PollShard 轮询一分片：从多个 Master 收 AgentResultEmitted，从多个 Agent 收 IntentRejected。
func PollShard(ctx context.Context, rpcURL, shardID string, masters, agents []common.Address,
	processed *map[string]bool, processedMu *sync.Mutex, writeMu *sync.Mutex, writer *csv.Writer, format string, outFile *os.File,
	fromBlock uint64, pollInterval time.Duration, debug bool) {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		log.Printf("[ERROR] 分片 %s 连接 RPC 失败: %v", shardID, err)
		return
	}
	defer client.Close()

	if fromBlock == 0 {
		bn, err := client.BlockNumber(ctx)
		if err != nil {
			log.Printf("[ERROR] 分片 %s 获取 block number 失败: %v", shardID, err)
			return
		}
		fromBlock = bn
		log.Printf("[INFO] 分片 %s 从区块 %d 开始", shardID, fromBlock)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentBlock, err := client.BlockNumber(ctx)
			if err != nil {
				continue
			}
			if currentBlock < fromBlock {
				continue
			}

			if len(masters) > 0 {
				queryMaster := ethereum.FilterQuery{
					FromBlock: big.NewInt(int64(fromBlock)),
					ToBlock:   big.NewInt(int64(currentBlock)),
					Addresses: masters,
					Topics:    [][]common.Hash{{agentResultEmittedSig}},
				}
				logsCoord, err := client.FilterLogs(ctx, queryMaster)
				if err != nil {
					log.Printf("[WARN] 分片 %s AgentResultEmitted FilterLogs 错误 (区块 %d-%d): %v", shardID, fromBlock, currentBlock, err)
					continue
				}
				if len(logsCoord) > 0 {
					log.Printf("[INFO] 分片 %s 区块 %d-%d: AgentResultEmitted %d 条", shardID, fromBlock, currentBlock, len(logsCoord))
				}
				for _, l := range logsCoord {
					txHashTopic := common.Hash{}
					if len(l.Topics) >= 2 {
						txHashTopic = l.Topics[1]
					}
					txId := txHashTopic.Hex()
					success, completionType := parseAgentResultEmittedData(l.Data)
					key := fmt.Sprintf("%s:%s:%d", shardID, txId, l.Index)
					processedMu.Lock()
					if (*processed)[key] {
						processedMu.Unlock()
						continue
					}
					(*processed)[key] = true
					processedMu.Unlock()

					block, _ := client.BlockByNumber(ctx, big.NewInt(int64(l.BlockNumber)))
					completionTime := int64(0)
					if block != nil && block.Time() > 0 {
						completionTime = int64(block.Time()) * 1000
					}
					completionTimeStr := ""
					if completionTime > 0 {
						completionTimeStr = strconv.FormatInt(completionTime, 10)
					}

					writeMu.Lock()
					writeRecord(writer, format, outFile, txId, txId, shardID, completionType, success, "", completionTimeStr, "", l.BlockNumber)
					writeMu.Unlock()
					log.Printf("[INFO] 分片 %s 写入 AgentResultEmitted: txId=%s type=%d success=%v block=%d from=%s", shardID, txId, completionType, success, l.BlockNumber, l.Address.Hex())
				}
			}

			if len(agents) > 0 {
				queryAgent := ethereum.FilterQuery{
					FromBlock: big.NewInt(int64(fromBlock)),
					ToBlock:   big.NewInt(int64(currentBlock)),
					Addresses: agents,
					Topics:    [][]common.Hash{{intentRejectedSig}},
				}
				logsAgent, err := client.FilterLogs(ctx, queryAgent)
				if err != nil {
					log.Printf("[WARN] 分片 %s IntentRejected FilterLogs 错误 (区块 %d-%d): %v", shardID, fromBlock, currentBlock, err)
					continue
				}
				if len(logsAgent) > 0 {
					log.Printf("[INFO] 分片 %s 区块 %d-%d: IntentRejected %d 条", shardID, fromBlock, currentBlock, len(logsAgent))
				}
				for _, l := range logsAgent {
					txIdTopic := common.Hash{}
					if len(l.Topics) >= 2 {
						txIdTopic = l.Topics[1]
					}
					txId := txIdTopic.Hex()
					key := fmt.Sprintf("%s:reject:%s:%d", shardID, txId, l.Index)
					processedMu.Lock()
					if (*processed)[key] {
						processedMu.Unlock()
						continue
					}
					(*processed)[key] = true
					processedMu.Unlock()

					block, _ := client.BlockByNumber(ctx, big.NewInt(int64(l.BlockNumber)))
					completionTime := int64(0)
					if block != nil && block.Time() > 0 {
						completionTime = int64(block.Time()) * 1000
					}
					completionTimeStr := ""
					if completionTime > 0 {
						completionTimeStr = strconv.FormatInt(completionTime, 10)
					}

					writeMu.Lock()
					writeRecord(writer, format, outFile, txId, txId, shardID, 6, false, "", completionTimeStr, "", l.BlockNumber)
					writeMu.Unlock()
					log.Printf("[INFO] 分片 %s 写入 IntentRejected: txId=%s block=%d from=%s", shardID, txId, l.BlockNumber, l.Address.Hex())
				}
			}

			fromBlock = currentBlock + 1
			if debug || ((fromBlock-1)%50 == 0 && fromBlock > 1) {
				log.Printf("[INFO] 分片 %s 已扫描至区块 %d", shardID, fromBlock-1)
			}
		}
	}
}

func parseAgentResultEmittedData(data []byte) (bool, uint8) {
	if len(data) < 64 {
		return false, 0
	}
	success := data[31] != 0
	completionType := data[63]
	return success, completionType
}

func writeRecord(writer *csv.Writer, format string, outFile *os.File, txHash, txId, shardID string, completionType uint8, success bool, sendTimeStr string, completionTimeStr string, latencyStr string, blockNum uint64) {
	createdAt := time.Now().UnixMilli()
	successStr := "false"
	if success {
		successStr = "true"
	}

	switch format {
	case "csv":
		if writer != nil {
			writer.Write([]string{txHash, txId, shardID, strconv.Itoa(int(completionType)), successStr, sendTimeStr, completionTimeStr, latencyStr, strconv.FormatUint(blockNum, 10), strconv.FormatInt(createdAt, 10)})
			writer.Flush()
		}
	case "jsonl":
		if outFile != nil {
			rec := map[string]interface{}{
				"tx_hash":         txHash,
				"tx_id":           txId,
				"shard_id":        shardID,
				"completion_type": completionType,
				"success":         success,
				"send_time":       sendTimeStr,
				"completion_time": completionTimeStr,
				"latency_ms":      latencyStr,
				"block_number":    blockNum,
				"created_at":      createdAt,
			}
			enc := json.NewEncoder(outFile)
			_ = enc.Encode(rec)
		}
	default:
		log.Printf("[WARN] 未知 format=%q，跳过写入", format)
	}
}
