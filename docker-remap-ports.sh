#!/bin/bash
# Docker 容器重新映射端口脚本

echo "=== 步骤 1: 停止并删除当前容器 ==="
docker stop harmony-node 2>/dev/null
docker rm harmony-node 2>/dev/null
echo "✅ 容器已停止并删除"
echo ""

echo "=== 步骤 2: 重新创建容器，添加 9502 端口映射 ==="
docker run -d --name harmony-node \
  -p 9500:9500 \
  -p 9501:9501 \
  -p 9502:9502 \
  -p 9599:9599 \
  -p 9598:9598 \
  -p 9800:9800 \
  -p 9899:9899 \
  -p 9898:9898 \
  -v "$(pwd):/root/go/src/github.com/harmony-one/harmony" \
  harmony-dev

if [ $? -eq 0 ]; then
  echo "✅ 容器已重新创建，端口映射已更新"
  echo ""
  echo "=== 步骤 3: 进入容器启动 harmony 节点 ==="
  echo "执行以下命令："
  echo "  docker exec -it harmony-node bash"
  echo "然后在容器内运行："
  echo "  VERBOSE=true make debug"
  echo ""
  echo "=== 步骤 4: 测试 shard 1 RPC（在宿主机上）==="
  echo "等待节点启动后，执行："
  echo '  curl -X POST "http://localhost:9502" \'
  echo '    -H "Content-Type: application/json" \'
  echo '    --data '"'"'{"jsonrpc":"2.0","method":"net_version","params":[],"id":1}'"'"
else
  echo "❌ 容器创建失败"
  exit 1
fi

