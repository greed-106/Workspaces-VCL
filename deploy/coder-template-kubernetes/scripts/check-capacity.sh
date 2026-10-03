#!/bin/sh
# 申请前的容量把关(由 main.tf 的 terraform_data.capacity_check 调用)。
# 参数通过环境变量传入,只做比较与报错,没有任何副作用。
set -u

if [ "${START_COUNT:-0}" = "0" ]; then
  echo "停止构建,跳过容量检查"
  exit 0
fi

fail=0
msg=""

if [ "$REQ_CPU" -gt "$FREE_CPU" ]; then
  fail=1
  msg="${msg}CPU:  申请 ${REQ_CPU} 核,当前可分配 ${FREE_CPU} 核
"
fi

if [ "$REQ_MEM" -gt "$FREE_MEM" ]; then
  fail=1
  msg="${msg}内存: 申请 ${REQ_MEM} GB,当前可分配 ${FREE_MEM} GB
"
fi

if [ "$REQ_DISK" -gt "$FREE_DISK" ]; then
  fail=1
  msg="${msg}磁盘: 申请 ${REQ_DISK} GB,当前可分配 ${FREE_DISK} GB
"
fi

if [ "${USE_GPU:-false}" = "true" ] && [ "$REQ_GPU" -gt "$FREE_GPU" ]; then
  fail=1
  msg="${msg}GPU:  ${GPU_MODEL} 申请 ${REQ_GPU} 张,当前空闲 ${FREE_GPU} 张
"
fi

if [ "$fail" = "1" ]; then
  echo "" >&2
  echo "集群剩余资源不足,本次申请被拒绝(没有创建任何资源):" >&2
  printf '%s' "$msg" >&2
  echo "请降低规格,或等其他人释放资源后重试。" >&2
  exit 1
fi

echo "容量检查通过"
