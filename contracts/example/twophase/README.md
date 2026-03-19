# 2PC 对照实验：传统两阶段提交 buyFruit + buyBook

与 JOYUE 的 `fruitstore-joyue` 分离，使用传统两阶段提交。buyFruit 与 buyBook 共享 Wallet/Points，可触发锁竞争。

## 合约

| 合约 | 分片 | 说明 |
|------|------|------|
| TwoPhaseCoordinator | 0 | 协调者 |
| FruitStore2PC | 0 | 水果库存 Participant |
| BookStore2PC | 0 | 书籍库存 Participant |
| Wallet2PC | 1 | 余额 Participant（共享） |
| Points2PC | 1 | 积分 Participant（共享） |

## 部署顺序

1. 在 shard 0 部署 FruitStore2PC、BookStore2PC
2. 在 shard 1 部署 Wallet2PC、Points2PC
3. 在 shard 0 部署 TwoPhaseCoordinator(fruitStore, bookStore, wallet, points, 0, 0, 1, 1)
4. 为 Wallet2PC、Points2PC 设置初始余额/积分

## 锁竞争测试

设置 `Wallet.setBalance(user, 25)`（小于 apple 20 + math 30），然后运行：

```bash
go run cmd/joyue-trigger/main.go --config cmd/joyue-trigger/trigger-2pc-lock.yaml --private-key <key>
```

并行发送 buy fruit(20) 和 buy book(30)，其中一个 Wallet.prepare 会因余额不足失败，触发 Abort。

## 与 JOYUE 的区别

- JOYUE：Agent 乐观执行 + Coordinator 聚合
- 2PC：Coordinator 先 Prepare 再 Commit/Abort，显式锁
