/*
JOYUE Trigger v2 - 支持同一分片多合约（targets）

配置在分片下增加可选 targets 列表，每项含 sig、args 与可选 to（空则同 v1：coordinator 优先，否则 agent）。
其余字段与 joyue-trigger 一致；JSONL 输出格式不变，无需 joyue-metrics-v2。
*/
package main

import "github.com/harmony-one/harmony/internal/joyuetrigger"

func main() {
	joyuetrigger.Main(joyuetrigger.Options{
		RejectTargets: false,
		ProgramName:   "joyue-trigger-v2",
	})
}
