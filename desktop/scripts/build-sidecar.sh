#!/usr/bin/env bash
# 把 hearth 服务端编成 Tauri 的 externalBin（sidecar），供桌面端「在本机运行服务器」用。
#
# 取舍：
#   - 直接复用同一份服务端代码与 service CLI，不为桌面端另做一套常驻逻辑。
#   - 前端产物必须在 go build 之前拷进 server/internal/webui/dist（见 CLAUDE.md 单二进制一节），
#     否则 sidecar 起来只有 API 没有页面。
#   - 文件名后缀是 Tauri 约定的 host triple，打包时会被去掉，app 里就叫 hearth。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TRIPLE="$(rustc -vV | awk '/^host:/{print $2}')"
[ -n "$TRIPLE" ] || { echo "取不到 rustc host triple" >&2; exit 1; }

WEB_DIST="$ROOT/web/dist"
EMBED_DIST="$ROOT/server/internal/webui/dist"
[ -d "$WEB_DIST" ] || { echo "缺少前端产物 ${WEB_DIST}，先 cd web && npm run build" >&2; exit 1; }

# .keep 是 git 里唯一留下的文件，重建后补回去
rm -rf "$EMBED_DIST"
mkdir -p "$EMBED_DIST"
touch "$EMBED_DIST/.keep"
cp -R "$WEB_DIST"/. "$EMBED_DIST"/

OUT="$ROOT/desktop/src-tauri/binaries/hearth-$TRIPLE"
mkdir -p "$(dirname "$OUT")"
cd "$ROOT/server"
go build -o "$OUT" ./cmd/server

echo "sidecar 已生成：${OUT}（$(du -h "$OUT" | cut -f1)）"
