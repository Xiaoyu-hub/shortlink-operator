#!/usr/bin/env bash
# 构建本地演示用的 manager 镜像并导入 k3d 集群（全程不走公网）。
#
# 用法：hack/build-local-image.sh [集群名]        # 默认 operator-demo
#       IMAGE=shortlink-operator:dev hack/build-local-image.sh
#
# 为什么不直接用仓库根目录的 Dockerfile：
#   1. 它是多阶段构建，需要拉 golang:1.26 与 gcr.io/distroless/static —— 本机到部分
#      registry 不稳定（见 docs/day05 第七节对 raw.githubusercontent 的实测），演示会卡在拉取；
#   2. 根目录 .dockerignore 默认忽略一切、只放行 Go 源码与 go.mod/go.sum，
#      所以 bin/ 下交叉编译出的二进制进不了构建上下文。
# 于是这里在 bin/ 下自拼一个最小上下文：只有 linux 二进制 + 一个极简 Dockerfile，
# 基础镜像用本机已有的 busybox。生产/CI 仍应使用根目录的 Dockerfile（distroless）。
set -euo pipefail

CLUSTER="${1:-operator-demo}"
IMAGE="${IMAGE:-shortlink-operator:dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CTX="$ROOT/bin/image-context"

cd "$ROOT"

echo "==> 交叉编译 manager（linux/amd64）"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/manager-linux cmd/main.go

echo "==> 组装构建上下文 $CTX"
rm -rf "$CTX"
mkdir -p "$CTX"
cp bin/manager-linux "$CTX/"
cat >"$CTX/Dockerfile" <<'EOF'
FROM busybox:latest
WORKDIR /
COPY manager-linux /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
EOF

echo "==> docker build -t $IMAGE"
docker build -t "$IMAGE" "$CTX"

echo "==> 导入 k3d 集群 $CLUSTER"
k3d image import "$IMAGE" -c "$CLUSTER"

echo "完成：$IMAGE 已进入集群 $CLUSTER（记得 kubectl config use-context k3d-$CLUSTER）"
