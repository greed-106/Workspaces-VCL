#!/usr/bin/env bash
# 把仓库里的配置模板渲染成本机配置并安装。
#
# 用法:
#   cp deploy/local.env.example deploy/local.env    # 首次
#   ${EDITOR:-vi} deploy/local.env
#   sudo deploy/install.sh                          # 安装 systemd 单元、配额脚本与 logrotate
#   sudo deploy/install.sh --render-only            # 只渲染到 deploy/generated/,不改系统文件
#
# 渲染规则:模板里的 __KEY__ 会被 deploy/local.env 中同名的 KEY 值替换。
# 安装目标:
#   systemd/*.service|*.timer -> /etc/systemd/system/
#   scripts/*.py              -> /usr/local/sbin/(0755)
#   logrotate/*               -> /etc/logrotate.d/
#   hdd-volumes/*.yaml        -> deploy/generated/(供 kubectl apply)
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
deploy_dir="$repo_dir/deploy"
env_file="$deploy_dir/local.env"
render_dir="$deploy_dir/generated"
render_only=0
[[ "${1:-}" == "--render-only" ]] && render_only=1

if [[ ! -f "$env_file" ]]; then
  echo "缺少 $env_file,先复制 deploy/local.env.example 再按本机情况修改。" >&2
  exit 1
fi

# shellcheck disable=SC1090
set -a && source "$env_file" && set +a

# render <模板文件> <输出文件>:按 local.env 的键逐个替换 __KEY__。
render() {
  local src="$1" dst="$2" key value
  cp "$src" "$dst"
  while IFS= read -r key; do
    [[ -z "$key" ]] && continue
    value="${!key-}"
    if [[ -z "$value" ]]; then
      echo "local.env 里缺少 $key" >&2
      exit 1
    fi
    sed -i "s|__${key}__|${value}|g" "$dst"
  done < <(grep -oE '__[A-Z][A-Z0-9_]*[A-Z0-9]__' "$src" | sed -E 's/^__//; s/__$//' | sort -u)
  if grep -qE '__[A-Z0-9_]+__' "$dst"; then
    echo "$(basename "$src") 里还有没被替换的占位符:" >&2
    grep -oE '__[A-Z][A-Z0-9_]*[A-Z0-9]__' "$dst" | sort -u >&2
    exit 1
  fi
}

rm -rf "$render_dir"
mkdir -p "$render_dir/systemd" "$render_dir/scripts" "$render_dir/logrotate" "$render_dir/hdd-volumes" "$render_dir/nginx"

for f in "$deploy_dir"/systemd/*; do
  render "$f" "$render_dir/systemd/$(basename "$f")"
done
for f in "$deploy_dir"/scripts/*.py; do
  render "$f" "$render_dir/scripts/$(basename "$f")"
done
for f in "$deploy_dir"/logrotate/*; do
  render "$f" "$render_dir/logrotate/$(basename "$f")"
done
for f in "$deploy_dir"/hdd-volumes/*.yaml; do
  render "$f" "$render_dir/hdd-volumes/$(basename "$f")"
done
for f in "$deploy_dir"/nginx/*.conf; do
  render "$f" "$render_dir/nginx/$(basename "$f")"
done

echo "已渲染到 $render_dir"

if (( render_only )); then
  exit 0
fi

if [[ $EUID -ne 0 ]]; then
  echo "安装系统文件需要 root,请用 sudo 运行(或加 --render-only 只渲染)。" >&2
  exit 1
fi

install -m 0644 "$render_dir"/systemd/* /etc/systemd/system/
install -m 0755 "$render_dir"/scripts/*.py /usr/local/sbin/
install -m 0644 "$render_dir"/logrotate/* /etc/logrotate.d/
systemctl daemon-reload

# GPU 历史指标服务:需要时用本机 Go 工具链重新构建,数据库口令写进 0600 的环境文件。
if [[ -n "${METRICS_PG_URL:-}" ]]; then
  if [[ -f "$render_dir/systemd/gpu-metrics.service" ]]; then
    go_bin="$(command -v go || true)"
    for cand in /usr/local/go/bin/go /usr/lib/go/bin/go; do
      [[ -z "$go_bin" && -x "$cand" ]] && go_bin="$cand"
    done
    if [[ -n "$go_bin" ]]; then
      (cd "$repo_dir/deploy/gpu-metrics" && GOTOOLCHAIN=local "$go_bin" build -o /tmp/gpu-metrics-build .) \
        && install -m 0755 /tmp/gpu-metrics-build /usr/local/bin/gpu-metrics && rm -f /tmp/gpu-metrics-build
    fi
    if [[ -x /usr/local/bin/gpu-metrics ]]; then
      printf 'METRICS_PG_URL=%s\n' "$METRICS_PG_URL" > /etc/gpu-metrics.env
      chmod 0600 /etc/gpu-metrics.env
    else
      echo "跳过 gpu-metrics 单元:找不到 Go 工具链,先手动构建:" >&2
      echo "  cd $repo_dir/deploy/gpu-metrics && go build -o /usr/local/bin/gpu-metrics ." >&2
      rm -f /etc/systemd/system/gpu-metrics.service
    fi
  fi
fi

# 反向代理:安装站点配置,并停用发行版自带的默认站点,避免抢占 80 端口。
if command -v nginx >/dev/null 2>&1; then
  install -m 0644 "$render_dir"/nginx/*.conf /etc/nginx/conf.d/
  rm -f /etc/nginx/sites-enabled/default
  nginx -t
  systemctl reload nginx
fi

cat <<EOF
已安装:
  systemd 单元 -> /etc/systemd/system/
  配额脚本     -> /usr/local/sbin/
  logrotate    -> /etc/logrotate.d/
  nginx 站点   -> /etc/nginx/conf.d/(发行版默认站点已停用)
  GPU 指标服务 -> /usr/local/bin/gpu-metrics + /etc/gpu-metrics.env(0600)

接下来:
  sudo systemctl enable --now coder-dev cluster-capacity
  sudo systemctl enable --now coder-workspace-quota.timer coder-hdd-volume-quota.timer
  kubectl apply -f $render_dir/hdd-volumes/storageclass.yaml
  kubectl apply -f $render_dir/hdd-volumes/placeholder.yaml
EOF
