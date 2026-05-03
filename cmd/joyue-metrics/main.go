/*
JOYUE Metrics - 指标采集工具

功能：
- 订阅链上事件：AgentResultEmitted（Master 发出）、IntentRejected（Agent 发出）
- NFT 场景（NftJoyueSalesMasterV2 / NftJoyueShopAgentV2）事件名相同，仅需把 --master/--agent 换成对应部署地址
- 输出 completion_type、success、block 时间等，不依赖 Trigger 文件

使用方式：

	joyue-metrics --rpcs 0=http://127.0.0.1:9500,1=http://127.0.0.1:9502 \
	  --master 0x61a049be2326C44637b6d6AfdF92480f67DCf076 \
	  --agent 0=0x61a049be2326C44637b6d6AfdF92480f67DCf076,1=0x61a049be2326C44637b6d6AfdF92480f67DCf076 \
	  --output ./metrics.csv --from-block 0

同一分片多 Master / 多 Agent 请使用 joyue-metrics-v2（--master/--agent 分片内地址用分号分隔）。
*/
package main

import "github.com/harmony-one/harmony/internal/joyuemetrics"

func main() {
	joyuemetrics.Main(joyuemetrics.Options{
		ProgramName:       "joyue-metrics",
		MultiAddrPerShard: false,
	})
}
