#!/bin/bash
# 开发容器：仅映射常用离散端口（兼容 2 分片交错 + 4/8/16 分片首节点 stride=32）。
# 若要从宿主机连「非首节点」的 P2P/RPC，再自行追加 -p 或改下面数组。
#
# 环境变量：HARMONY_DEV_IMAGE（默认 harmony-dev）、HARMONY_DEV_CONTAINER、
# HARMONY_DEV_MOUNT、HARMONY_DEV_MOUNT_DST 同前。

set -euo pipefail

IMAGE="${HARMONY_DEV_IMAGE:-harmony-dev}"
NAME="${HARMONY_DEV_CONTAINER:-harmony-node}"
MOUNT_SRC="${HARMONY_DEV_MOUNT:-$(pwd)}"
MOUNT_DST="${HARMONY_DEV_MOUNT_DST:-/root/go/src/github.com/harmony-one/harmony}"

# --- 各分片「首 validator」HTTP（P2P+500）；16 片：9500 + i*32，i=0..15 ---
http_ports=(
	9500 9532 9564 9596 9628 9660 9692 9724 9756 9788 9820 9852 9884 9916 9948 9980
	9502
	9501
	9598 9599
	9524 9526
)
# --- 对应 WS（P2P+800）；2 分片多一个 9802 ---
ws_ports=(
	9800 9832 9864 9896 9928 9960 9992 10024 10056 10088 10120 10152 10184 10216 10248 10280
	9802
	9898 9899
)
# --- 首节点 P2P（9000 + i*32）+ 2 分片第二片首节点 9002 ---
p2p_ports=(
	9000 9032 9064 9096 9128 9160 9192 9224 9256 9288 9320 9352 9384 9416 9448 9480
	9002
)

docker_ports=()
for p in "${http_ports[@]}"; do docker_ports+=(-p "${p}:${p}"); done
for p in "${ws_ports[@]}";  do docker_ports+=(-p "${p}:${p}"); done
for p in "${p2p_ports[@]}"; do docker_ports+=(-p "${p}:${p}"); done
docker_ports+=(-p 8888:8888 -p 8889:8889 -p 19876:19876)

echo "=== 步骤 1: 停止并删除当前容器 ==="
docker stop "${NAME}" 2>/dev/null || true
docker rm "${NAME}" 2>/dev/null || true
echo "✅ 容器已停止并删除"
echo ""

echo "=== 步骤 2: 创建容器（离散端口：HTTP ${#http_ports[@]} + WS ${#ws_ports[@]} + P2P ${#p2p_ports[@]} + bootnode）==="
if docker run -d -it --name "${NAME}" \
	"${docker_ports[@]}" \
	-v "${MOUNT_SRC}:${MOUNT_DST}" \
	"${IMAGE}" /bin/bash
then
	echo "✅ 容器已创建：${NAME}（镜像 ${IMAGE}）"
	echo ""
	echo "=== 步骤 3: 进入容器 ==="
	echo "  docker exec -it ${NAME} bash"
	echo ""
	echo "容器内：make debug | make debug-4 | make debug-8 | make debug-16"
	echo ""
	echo "=== 宿主机 curl 示例 ==="
	echo "  2 分片 shard1 HTTP: 9502"
	echo "  4 分片 shard1 HTTP: 9532"
else
	echo "❌ 容器创建失败"
	exit 1
fi
