#!/usr/bin/env bash
# 构建内置 code-server 的工作区镜像,并导入到 k8s 使用的 containerd 命名空间。
#
#   ./build.sh              # 构建两个镜像:Ubuntu 24.04 基础版 + CUDA 12.8 版
#   ./build.sh base         # 只构建基础版
#   ./build.sh cuda         # 只构建 CUDA 版
#   CS_VERSION=4.140.0 ./build.sh
set -euo pipefail
cd "$(dirname "$0")"

CS_VERSION="${CS_VERSION:-$(grep -m1 '^ARG CODE_SERVER_VERSION=' Dockerfile | cut -d= -f2)}"
TARBALL="code-server.tar.gz"
MIRROR_PREFIX="${MIRROR_PREFIX-https://ghproxy.net/}"

BASE_BASE="${BASE_BASE:-docker.1panel.live/library/ubuntu:24.04}"
BASE_CUDA="${BASE_CUDA:-docker.1panel.live/nvidia/cuda:12.8.1-devel-ubuntu24.04}"
IMAGE_BASE="${IMAGE_BASE:-coder-workspace:ubuntu24.04-code-server-${CS_VERSION}}"
IMAGE_CUDA="${IMAGE_CUDA:-coder-workspace:ubuntu24.04-cuda12.8-code-server-${CS_VERSION}}"

if [ ! -f "$TARBALL" ]; then
  url="https://github.com/coder/code-server/releases/download/v${CS_VERSION}/code-server-${CS_VERSION}-linux-amd64.tar.gz"
  echo "==> 下载 code-server ${CS_VERSION}"
  for attempt in 1 2 3 4 5; do
    if curl -fsSL --http1.1 --retry 3 --retry-delay 3 -o "$TARBALL" "${MIRROR_PREFIX}${url}"; then break; fi
    echo "    第 ${attempt} 次下载失败,重试..."
    sleep 3
    [ "$attempt" = 5 ] && { echo "下载失败:请手动放置 $TARBALL 后重跑"; exit 1; }
  done
fi

build_one() {
  local image="$1" base="$2"
  echo "==> 构建 $image (base=$base)"
  sudo podman build --format docker --build-arg "BASE_IMAGE=$base" -t "$image" .
  echo "==> 导入 containerd(k8s.io 命名空间)"
  sudo podman save "$image" | sudo ctr -n k8s.io images import -
  # kubelet 会把短镜像名规范化成 docker.io/library/<name>,几个标签都打上。
  sudo ctr -n k8s.io images tag "localhost/$image" "docker.io/library/$image" 2>/dev/null || true
  sudo ctr -n k8s.io images tag "localhost/$image" "$image" 2>/dev/null || true
  echo "==> 完成 $image"
}

case "${1:-all}" in
  base) build_one "$IMAGE_BASE" "$BASE_BASE" ;;
  cuda) build_one "$IMAGE_CUDA" "$BASE_CUDA" ;;
  all)  build_one "$IMAGE_BASE" "$BASE_BASE"; build_one "$IMAGE_CUDA" "$BASE_CUDA" ;;
  *) echo "用法: $0 [all|base|cuda]"; exit 1 ;;
esac

echo "==> 已导入的镜像:"
sudo ctr -n k8s.io images ls -q | grep -F "coder-workspace:" | sort || true
