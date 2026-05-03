/*
JOYUE Metrics v2 - 支持同一分片多个 Master、多个 Agent

与 joyue-metrics 相同的事件与输出格式（CSV/JSONL），FilterLogs 对每分片传入多个合约地址。NFT（NftJoyue*）与其它 JOYUE 共用 AgentResultEmitted / IntentRejected，仅需配置对应 Master/Agent 地址。

--master / --agent 语法（在 v1 基础上扩展）：
  - 单分片单地址（与 v1 相同）：0xABC 或 0=0xABC,1=0xDEF
  - 同一分片多地址：分号分隔
    0=0xMasterA;0xMasterB,1=0xMasterC
    0=0xAgentA;0xAgentB,1=0xAgentC

示例：

	joyue-metrics-v2 --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9502 \
	  --master '0=0x1111111111111111111111111111111111111111;0x2222222222222222222222222222222222222222' \
	  --agent '0=0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA;0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB' \
	  --output ./metrics-v2.csv --from-block 0
*/
package main

import "github.com/harmony-one/harmony/internal/joyuemetrics"

func main() {
	joyuemetrics.Main(joyuemetrics.Options{
		ProgramName:       "joyue-metrics-v2",
		MultiAddrPerShard: true,
	})
}
