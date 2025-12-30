#!/bin/bash
# 检查部署账户在各个 shard 的余额

PRIVATE_KEY="3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6"

echo "=== 计算部署者地址 ==="
# 使用 joyue-check-deploy 工具计算地址
DEPLOYER_ADDR=$(go run cmd/joyue-check-deploy/main.go --rpc http://127.0.0.1:9500 --private-key "$PRIVATE_KEY" --nonce 0 2>&1 | grep "从私钥计算的部署者地址" | awk '{print $NF}')
if [ -z "$DEPLOYER_ADDR" ]; then
    echo "无法计算部署者地址，尝试直接查询..."
    # 如果无法计算，使用 Python 或其他方式
    DEPLOYER_ADDR="0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6"  # 这是之前看到的地址
fi
echo "部署者地址: $DEPLOYER_ADDR"
echo ""

echo "=== 检查 Shard 0 余额 ==="
curl -s -X POST http://127.0.0.1:9500 \
  -H "Content-Type: application/json" \
  -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$DEPLOYER_ADDR\",\"latest\"],\"id\":1}" | \
  python3 -c "import sys, json; r=json.load(sys.stdin); print('余额:', int(r['result'], 16) / 1e18, 'ONE')" 2>/dev/null || \
  echo "需要手动解析余额"
echo ""

echo "=== 检查 Shard 1 余额 ==="
curl -s -X POST http://127.0.0.1:9502 \
  -H "Content-Type: application/json" \
  -d "{\"jsonrpc\":\"2.0\",\"method\":\"eth_getBalance\",\"params\":[\"$DEPLOYER_ADDR\",\"latest\"],\"id\":1}" | \
  python3 -c "import sys, json; r=json.load(sys.stdin); print('余额:', int(r['result'], 16) / 1e18, 'ONE')" 2>/dev/null || \
  echo "需要手动解析余额"
echo ""

echo "=== 如果 Shard 1 余额为 0，需要先转账 ==="
echo "可以使用以下命令从 shard0 转账到 shard1："
echo "  hmy transfer --from <from_addr> --to $DEPLOYER_ADDR --from-shard 0 --to-shard 1 --amount <amount> --node http://127.0.0.1:9500"
echo ""
echo "或者使用跨分片转账功能"

