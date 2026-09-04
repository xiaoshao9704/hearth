#!/usr/bin/env bash
# 从 deploy/.env 生成配置到 deploy/generated/：
#   livekit.yaml（单节点）/ Caddyfile
set -euo pipefail
cd "$(dirname "$0")"

if [ ! -f .env ]; then
  echo "错误: 缺少 deploy/.env，请先执行: cp .env.example .env 并填写 DOMAIN 与密钥" >&2
  exit 1
fi

if ! command -v envsubst >/dev/null 2>&1; then
  echo "错误: 需要 envsubst（gettext 包）。" >&2
  echo "  macOS:  brew install gettext && brew link --force gettext" >&2
  echo "  Debian: apt-get install gettext-base" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
source .env
set +a

# 占位/空密钥警告（不阻断，但部署前务必修改）
if [ -z "${LIVEKIT_API_SECRET:-}" ] || [ "${LIVEKIT_API_SECRET}" = "change-me-livekit-secret" ]; then
  echo "警告: LIVEKIT_API_SECRET 仍是占位值 change-me-livekit-secret，请修改 deploy/.env 后再上线" >&2
fi
if [ -z "${DOMAIN:-}" ] || [ "${DOMAIN}" = "livekit.example.com" ]; then
  echo "警告: DOMAIN 仍是占位值 livekit.example.com，请修改 deploy/.env" >&2
fi

mkdir -p generated
for f in livekit.yaml Caddyfile; do
  envsubst < "$f.template" > "generated/$f"
done
echo "已生成 deploy/generated/{livekit.yaml, Caddyfile}"
