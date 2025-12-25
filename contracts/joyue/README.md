## 第一阶段：用 relayer 实现“部署 Master → 自动部署 Agent（跨分片）”

本阶段目标：先把“自动铺 agent”跑通，不改共识/协议。

### 合约

- `JoyueMaster.sol`
  - 部署时（constructor）emit `MasterDeployed(master, salt, agentCodeHash, masterShardId)`
  - relayer 监听这个事件后，去其它 shard 通过 RPC 发交易部署 `JoyueAgent`

- `JoyueAgent.sol`
  - 最小代理合约：只保存 `master/masterShardId/agentShardId` 三个字段

### Relayer

见：`cmd/joyue-relayer/`


