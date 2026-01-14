#!/bin/bash

# FruitStore 跨分片调用测试脚本
# 使用前请设置以下变量：
# - FRUIT_STORE_ADDR: FruitStore 合约地址
# - CURRENCY_CONTRACT_ADDR: CurrencyContract 合约地址（分片 1）
# - POINTS_CONTRACT_ADDR: PointsContract 合约地址（分片 1）
# - EXECUTOR_ADDR: JoyueCrossShardExecutor 合约地址（分片 1）
# - USER_ADDR: 测试用户地址
# - PRIVATE_KEY: 私钥（不带 0x）

# 配置（请根据实际部署地址修改）
FRUIT_STORE_ADDR="${FRUIT_STORE_ADDR:-0x63169D7049Fa7cfA842179AeC59CED7B00B32733}"
CURRENCY_CONTRACT_ADDR="${CURRENCY_CONTRACT_ADDR:-0x61a049be2326C44637b6d6AfdF92480f67DCf076}"
POINTS_CONTRACT_ADDR="${POINTS_CONTRACT_ADDR:-0x81ac14c04310feDFeC065997f07E83d934659ae7}"
EXECUTOR_ADDR="${EXECUTOR_ADDR:-0x12a4113F44E5689C93df233008FD805BDc882ABb}"
USER_ADDR="${USER_ADDR:-0x6a87346f3Ba9958d08D09484A2b7fDBbE42b0df6}"
PRIVATE_KEY="${PRIVATE_KEY:-3836e3675817a46abfadd55b5caec4682a06a919377df79924e75cedbd6eedb6}"

# RPC 配置
SHARD0_RPC="http://127.0.0.1:9500"
SHARD1_RPC="http://127.0.0.1:9502"

echo "=========================================="
echo "FruitStore 跨分片调用测试"
echo "=========================================="
echo "FruitStore: $FRUIT_STORE_ADDR (Shard 0)"
echo "CurrencyContract: $CURRENCY_CONTRACT_ADDR (Shard 1)"
echo "PointsContract: $POINTS_CONTRACT_ADDR (Shard 1)"
echo "Executor: $EXECUTOR_ADDR (Shard 1)"
echo "User: $USER_ADDR"
echo ""

# 颜色输出
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 步骤 1: 给用户充值货币（分片 1）
echo -e "${YELLOW}[步骤 1] 给用户充值货币（分片 1）${NC}"
go run cmd/joyue-call/main.go \
  --rpc "$SHARD1_RPC" \
  --to "$CURRENCY_CONTRACT_ADDR" \
  --sig "deposit(address,uint256)()" \
  --arg "$USER_ADDR" \
  --arg "1000000" \
  --send \
  --private-key "$PRIVATE_KEY" \
  --wait

if [ $? -eq 0 ]; then
  echo -e "${GREEN}✓ 充值成功${NC}"
else
  echo -e "${RED}✗ 充值失败${NC}"
  exit 1
fi

echo ""

# 步骤 2: 查询用户货币余额（分片 1）
echo -e "${YELLOW}[步骤 2] 查询用户货币余额（分片 1）${NC}"
go run cmd/joyue-call/main.go \
  --rpc "$SHARD1_RPC" \
  --to "$CURRENCY_CONTRACT_ADDR" \
  --sig "getBalance(address)(uint256)" \
  --arg "$USER_ADDR"

echo ""

# 步骤 3: 查询用户积分余额（分片 1）
echo -e "${YELLOW}[步骤 3] 查询用户积分余额（分片 1）${NC}"
go run cmd/joyue-call/main.go \
  --rpc "$SHARD1_RPC" \
  --to "$POINTS_CONTRACT_ADDR" \
  --sig "getBalance(address)(uint256)" \
  --arg "$USER_ADDR"

echo ""

# 步骤 4: 添加水果到商店（分片 0）
echo -e "${YELLOW}[步骤 4] 添加水果到商店（分片 0）${NC}"
echo "添加苹果..."
go run cmd/joyue-call/main.go \
  --rpc "$SHARD0_RPC" \
  --to "$FRUIT_STORE_ADDR" \
  --sig "addFruit(string,uint256,uint256,uint256)()" \
  --arg "apple" \
  --arg "100" \
  --arg "10" \
  --arg "100" \
  --send \
  --private-key "$PRIVATE_KEY" \
  --wait

if [ $? -eq 0 ]; then
  echo -e "${GREEN}✓ 添加水果成功${NC}"
else
  echo -e "${RED}✗ 添加水果失败${NC}"
  exit 1
fi

echo ""

# 步骤 5: 查询水果信息（分片 0）
echo -e "${YELLOW}[步骤 5] 查询水果信息（分片 0）${NC}"
go run cmd/joyue-call/main.go \
  --rpc "$SHARD0_RPC" \
  --to "$FRUIT_STORE_ADDR" \
  --sig "getFruit(string)(string,uint256,uint256,uint256,bool)" \
  --arg "apple"

echo ""

# 步骤 6: 购买水果（分片 0，触发跨分片调用）
echo -e "${YELLOW}[步骤 6] 购买水果（分片 0，触发跨分片调用）${NC}"
echo "购买 2 个苹果..."
OUTPUT=$(go run cmd/joyue-call/main.go \
  --rpc "$SHARD0_RPC" \
  --to "$FRUIT_STORE_ADDR" \
  --sig "buyFruit(string,uint256)(uint256)" \
  --arg "apple" \
  --arg "2" \
  --send \
  --private-key "$PRIVATE_KEY" \
  --gas 500000 2>&1)

if [ $? -ne 0 ]; then
  echo -e "${RED}✗ 购买失败${NC}"
  echo "$OUTPUT"
  exit 1
fi

# 提取交易哈希
TX_HASH=$(echo "$OUTPUT" | grep -oE 'tx=0x[0-9a-fA-F]+' | head -1 | cut -d'=' -f2)

if [ -z "$TX_HASH" ]; then
  echo -e "${RED}✗ 无法提取交易哈希${NC}"
  echo "$OUTPUT"
  exit 1
fi

echo -e "${GREEN}✓ 购买交易已发送: $TX_HASH${NC}"

# 等待几秒后检查交易状态
echo ""
echo -e "${YELLOW}等待 5 秒后检查交易状态...${NC}"
sleep 5

# 检查交易是否已打包
RECEIPT=$(hmy --node="$SHARD0_RPC" blockchain transaction-receipt "$TX_HASH" 2>&1)
if echo "$RECEIPT" | grep -q "null"; then
  echo -e "${YELLOW}⚠ 交易还在 pending，继续等待...${NC}"
else
  STATUS=$(echo "$RECEIPT" | jq -r '.result.status' 2>/dev/null || echo "unknown")
  if [ "$STATUS" = "0x0" ] || [ "$STATUS" = "0" ]; then
    echo -e "${RED}✗ 交易执行失败 (status=0)${NC}"
    echo "$RECEIPT"
    echo ""
    echo -e "${YELLOW}尝试获取 revert reason...${NC}"
    # 使用 debug_traceTransaction 获取 revert reason
    TRACE_OUTPUT=$(go run cmd/joyue-call/main.go \
      --rpc "$SHARD0_RPC" \
      --sig "debug_traceTransaction(string)()" \
      --arg "$TX_HASH" 2>&1 || echo "")
    if [ -n "$TRACE_OUTPUT" ]; then
      echo "$TRACE_OUTPUT"
    fi
    exit 1
  else
    echo -e "${GREEN}✓ 交易已打包 (status=1)${NC}"
  fi
fi

echo ""
echo -e "${YELLOW}等待跨分片回调完成（约 25 秒）...${NC}"
sleep 25

# 步骤 7: 查询 nextOrderId 确认订单是否已创建（分片 0）
echo -e "${YELLOW}[步骤 7] 查询 nextOrderId（分片 0）${NC}"
echo "查询当前订单数量..."
NEXT_ORDER_ID=$(go run cmd/joyue-call/main.go \
  --rpc "$SHARD0_RPC" \
  --to "$FRUIT_STORE_ADDR" \
  --sig "nextOrderId()(uint256)" 2>&1 | grep -oE '[0-9]+' | head -1)

if [ -z "$NEXT_ORDER_ID" ]; then
  echo -e "${RED}✗ 无法查询 nextOrderId${NC}"
  NEXT_ORDER_ID=0
else
  echo -e "${GREEN}✓ nextOrderId = $NEXT_ORDER_ID${NC}"
fi

# 计算最后一个订单 ID（nextOrderId - 1）
if [ "$NEXT_ORDER_ID" -gt "0" ]; then
  LAST_ORDER_ID=$((NEXT_ORDER_ID - 1))
  echo -e "${GREEN}✓ 最后一个订单 ID = $LAST_ORDER_ID${NC}"
else
  echo -e "${YELLOW}⚠ 还没有订单，可能交易还在 pending 或执行失败${NC}"
  echo -e "${YELLOW}提示：可以手动查询交易状态: hmy --node=\"$SHARD0_RPC\" blockchain transaction-receipt $TX_HASH${NC}"
  LAST_ORDER_ID=1  # 默认查询订单 1
fi

echo ""

# 步骤 8: 查询订单状态（分片 0）
echo -e "${YELLOW}[步骤 8] 查询订单状态（分片 0）${NC}"
echo "查询订单 ID $LAST_ORDER_ID..."
go run cmd/joyue-call/main.go \
  --rpc "$SHARD0_RPC" \
  --to "$FRUIT_STORE_ADDR" \
  --sig "getOrder(uint256)(address,string,uint256,uint256,uint256,uint8,uint64)" \
  --arg "$LAST_ORDER_ID"

echo ""

# 步骤 8: 再次查询用户货币余额（分片 1，应该已扣除）
echo -e "${YELLOW}[步骤 8] 查询用户货币余额（分片 1，应该已扣除）${NC}"
go run cmd/joyue-call/main.go \
  --rpc "$SHARD1_RPC" \
  --to "$CURRENCY_CONTRACT_ADDR" \
  --sig "getBalance(address)(uint256)" \
  --arg "$USER_ADDR"

echo ""

# 步骤 9: 再次查询用户积分余额（分片 1，应该已增加）
echo -e "${YELLOW}[步骤 9] 查询用户积分余额（分片 1，应该已增加）${NC}"
go run cmd/joyue-call/main.go \
  --rpc "$SHARD1_RPC" \
  --to "$POINTS_CONTRACT_ADDR" \
  --sig "getBalance(address)(uint256)" \
  --arg "$USER_ADDR"

echo ""

# 步骤 10: 查询用户总积分（分片 1）
echo -e "${YELLOW}[步骤 10] 查询用户总积分（分片 1）${NC}"
go run cmd/joyue-call/main.go \
  --rpc "$SHARD1_RPC" \
  --to "$POINTS_CONTRACT_ADDR" \
  --sig "getTotalEarned(address)(uint256)" \
  --arg "$USER_ADDR"

echo ""
echo -e "${GREEN}=========================================="
echo "测试完成！"
echo "==========================================${NC}"

