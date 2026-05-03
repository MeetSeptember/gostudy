/*
JOYUE Trigger - 多分片并行触发工具（v1）

与 joyue-trigger-v2 共用 internal/joyuetrigger；v1 若 YAML 含分片级 targets 将报错并提示改用 v2。
*/
package main

import "github.com/harmony-one/harmony/internal/joyuetrigger"

func main() {
	joyuetrigger.Main(joyuetrigger.Options{
		RejectTargets: true,
		ProgramName:   "joyue-trigger",
	})
}
