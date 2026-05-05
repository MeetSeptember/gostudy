#!/bin/bash
# 开发容器端口映射：兼容 classic 2 分片（HTTP 9500/9502 步长 2）与
# local-resharding-{4,8,16}.txt（首节点 HTTP/WS 步长 32，16 分片末片 HTTP 约 9980，
# 单节点 P2P 最高可到 9xxx 末段，HTTP 可达 10000+）。
# 可按需改 IMAGE / 挂载路径。

set -euo pipefail

IMAGE="${HARMONY_DEV_IMAGE:-harmony-dev}"
NAME="${HARMONY_DEV_CONTAINER:-harmony-node}"
MOUNT_SRC="${HARMONY_DEV_MOUNT:-$(pwd)}"
MOUNT_DST="${HARMONY_DEV_MOUNT_DST:-/root/go/src/github.com/harmony-one/harmony}"

# HTTP RPC：覆盖 9500–约 10180（16 分片首节点 9980 + 余量）；含 9501(auth)、9524/9526 等常见偏移
HTTP_LO=9500
HTTP_HI=10180
# WebSocket：与 HTTP 同 stride，16 分片约 10280
WS_LO=9800
WS_HI=10500
# P2P（localnet deploy 首段 9000 起、多分片块布局）
P2P_LO=9000
P2P_HI=9660

echo "=== 步骤 1: 停止并删除当前容器 ==="
docker stop "${NAME}" 2>/dev/null || true
docker rm "${NAME}" 2>/dev/null || true
echo "✅ 容器已停止并删除"
echo ""

echo "=== 步骤 2: 创建容器（端口范围兼容 2/4/8/16 分片；9598/9599 等在 HTTP 范围内）==="
# 范围映射：宿主机与容器同一区间（勿再对区间内端口单独 -p，以免 Docker 冲突）
if docker run -d -it --name "${NAME}" \
  -p "${HTTP_LO}-${HTTP_HI}:${HTTP_LO}-${HTTP_HI}" \
  -p "${WS_LO}-${WS_HI}:${WS_LO}-${WS_HI}" \
  -p "${P2P_LO}-${P2P_HI}:${P2P_LO}-${P2P_HI}" \
  -p 8888:8888 \
  -p 8889:8889 \
  -p 19876:19876 \
  -v "${MOUNT_SRC}:${MOUNT_DST}" \
  "${IMAGE}" /bin/bash
then
  echo "✅ 容器已创建：${NAME}（镜像 ${IMAGE}）"
  echo ""
  echo "已映射：HTTP ${HTTP_LO}-${HTTP_HI}，WS ${WS_LO}-${WS_HI}，P2P ${P2P_LO}-${P2P_HI}；bootnode 8888/8889/19876（9598/9599/9898/9899 已含在 HTTP/WS 范围内）"
  echo ""
  echo "=== 步骤 3: 进入容器 ==="
  echo "  docker exec -it ${NAME} bash"
  echo ""
  echo "容器内示例："
  echo "  make debug              # 2 分片 local-resharding.txt"
  echo "  make debug-4            # 4 分片"
  echo "  VERBOSE=true make debug-8"
  echo ""
  echo "=== 步骤 4: 宿主机 curl 示例（RPC HTTP）==="
  echo "  2 分片 shard1:  curl -sS -X POST http://127.0.0.1:9502 -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"method\":\"hmy_shardId\",\"params\":[],\"id\":1}'"
  echo "  4 分片 shard1:  curl -sS -X POST http://127.0.0.1:9532 -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"method\":\"hmy_shardId\",\"params\":[],\"id\":1}'"
else
  echo "❌ 容器创建失败"
  exit 1
fi
