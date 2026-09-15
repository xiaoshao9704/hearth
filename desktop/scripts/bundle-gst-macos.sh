#!/usr/bin/env bash
# 把 GStreamer 运行时打进 macOS .app：tauri build 产出 .app 之后运行。
#
# 不做成 tauri 的 beforeBundleCommand：那个钩子在打包之前跑，那时 .app 还不存在，
# 改不到里面的二进制；签名也必须发生在改完 install name 之后。所以这里是后处理，
# 由 package.json 的 build:mac 串起来。
#
# 取舍：
#   - 只带实测跑通一次 WHIP 推流所加载的插件（见下面的 PLUGINS），不整包搬运。
#   - 不引额外工具（dylibbundler 之类）：otool + install_name_tool 是系统自带的，
#     依赖走一遍就够，多一个 Homebrew 依赖反而与「自包含」的目标相悖。
#   - 库平铺进 Contents/Frameworks，插件放 Contents/Resources/gstreamer-1.0：
#     codesign 把 Frameworks 下的任何子目录都当 bundle 解析，装一堆散装 dylib 会被判
#     「bundle format unrecognized」；Resources 下的 Mach-O 按已签名的嵌套代码封存，
#     --deep --strict 能过。引用一律改成 @rpath/<文件名>，两个目录每次重建。
set -euo pipefail

APP=${1:-}
if [ -z "$APP" ]; then
  APP="$(cd "$(dirname "$0")/.." && pwd)/src-tauri/target/release/bundle/macos/Hearth.app"
fi
[ -d "$APP" ] || { echo "找不到 .app：$APP" >&2; exit 1; }

BIN="$APP/Contents/MacOS/hearth-desktop"
[ -f "$BIN" ] || { echo "找不到主二进制：$BIN" >&2; exit 1; }
ENTITLEMENTS="$(cd "$(dirname "$0")/.." && pwd)/src-tauri/Entitlements.plist"

# 实测清单：测试源模式跑通一次完整 WHIP 推流、再跑一次源预览单帧管线
# （两次都开 GST_DEBUG=GST_PLUGIN_LOADING:4、用同一份预热注册表，只看真正被加载的）后
# 合并的结果，再补一个不会出现在那两条日志里的兜底项。
PLUGINS=(
  coreelements        # queue/capsfilter/tee/fakesink 等基础元素，管线里到处是
  app                 # appsrc：真实采集（ScreenCaptureKit）喂管线的入口
  typefindfunctions   # caps 探测兜底；不在实测日志里，但缺它 caps 协商会在边角情况上炸
  videotestsrc        # 测试源模式（HEARTH_DESKTOP_TESTSRC=1）的画面
  audiotestsrc        # 测试源模式的声音
  videoconvertscale   # videoconvert/videoscale：SCK 直出 NV12 时是直通，caps 不匹配时兜底
  jpeg                # jpegenc：源预览那条单帧管线（preview.rs），推流路径上不出现
  applemedia          # vtenc_h264 / vtenc_h265，VideoToolbox 硬编
  videoparsersbad     # h264parse / h265parse
  audioconvert        # 采集音频 F32LE → opusenc 要的格式
  audioresample
  opus                # opusenc
  rswebrtc            # whipclientsink（gst-plugins-rs）
  webrtc              # webrtcbin
  rtp                 # rtp 载荷打包
  rtpmanager          # rtpbin / rtpsession
  srtp                # 媒体加密
  dtls                # DTLS 握手
  nice                # ICE（Homebrew 里是指向 libnice-gstreamer 的符号链接）
  debugutilsbad       # webrtcbin 内部会拉起这里的元素，实测被加载
)
# 没带：videorate（当前管线不用它，采集侧自己控帧率）。
# videoscale 在 videoconvertscale 里，不是单独的插件；app 同时给 appsrc 与 appsink。

# Homebrew 的 gstreamer lib 目录从主二进制的链接路径反推，不依赖 brew 在 PATH 里。
GST_LIBDIR=$(otool -L "$BIN" | awk '/libgstreamer-1\.0\.0\.dylib/ {print $1; exit}' | xargs -I{} dirname {})
if [ -z "$GST_LIBDIR" ] || [ ! -d "$GST_LIBDIR" ]; then
  # 二进制已经改过 install name（重复运行）时反推不出来，回落到默认前缀
  GST_LIBDIR=/opt/homebrew/opt/gstreamer/lib
fi
PLUGIN_SRCDIRS=("$GST_LIBDIR/gstreamer-1.0" /opt/homebrew/lib/gstreamer-1.0)
# @rpath 依赖在 rpath 里找不着时的兜底：/opt/homebrew/lib 里有各 keg 的链接，够全
LIB_SRCDIRS=("$GST_LIBDIR" /opt/homebrew/lib)

FW="$APP/Contents/Frameworks"
PLUGDIR="$APP/Contents/Resources/gstreamer-1.0"

# --- 依赖收集 ---------------------------------------------------------------

# otool -L 的第一行是文件自己的 id，不是依赖，要按 otool -D 的结果剔掉。
deps_of() {
  local f=$1 id
  id=$(otool -D "$f" | tail -n +2 || true)
  otool -L "$f" | tail -n +2 | awk '{print $1}' | while read -r d; do
    [ "$d" = "$id" ] && continue
    case "$d" in /usr/lib/*|/System/*) continue ;; esac
    echo "$d"
  done
}

rpaths_of() {
  otool -l "$1" | awk '/^ *cmd LC_RPATH$/{f=1} f && /^ *path /{print $2; f=0}'
}

# @rpath/@loader_path 形式的依赖解析成真实文件路径
resolve_dep() {
  local f=$1 dep=$2 dir base rp
  dir=$(dirname "$f")
  case "$dep" in
    @rpath/*)
      base=${dep#@rpath/}
      for rp in $(rpaths_of "$f"); do
        rp=${rp//@loader_path/$dir}
        rp=${rp//@executable_path/$dir}
        [ -e "$rp/$base" ] && { echo "$rp/$base"; return 0; }
      done
      for rp in "${LIB_SRCDIRS[@]}"; do
        [ -e "$rp/$base" ] && { echo "$rp/$base"; return 0; }
      done
      return 1 ;;
    @loader_path/*) echo "$dir/${dep#@loader_path/}" ;;
    @executable_path/*) echo "$dir/${dep#@executable_path/}" ;;
    *) echo "$dep" ;;
  esac
}

# 先清空上一轮的产物再收集依赖：留着的话主二进制的 @rpath 会指回这里，
# 等于拿改过 install name 的副本当源。
rm -rf "$FW" "$PLUGDIR"

SEEN="|"   # bash 3.2 没有关联数组，用竖线分隔的文件名串当集合
LIBS=""    # 换行分隔的待拷贝库源路径

mark_seen() { SEEN="$SEEN$1|"; }
is_seen() { case "$SEEN" in *"|$1|"*) return 0 ;; esac; return 1; }

walk() {
  local f=$1 dep src base
  while read -r dep; do
    [ -z "$dep" ] && continue
    if ! src=$(resolve_dep "$f" "$dep"); then
      echo "解析不了依赖：$dep（来自 $f）" >&2; exit 1
    fi
    [ -e "$src" ] || { echo "依赖文件不存在：$src（来自 $f）" >&2; exit 1; }
    base=$(basename "$src")
    is_seen "$base" && continue
    mark_seen "$base"
    LIBS="$LIBS$src"$'\n'
    walk "$src"
  done < <(deps_of "$f")
}

PLUGIN_SRCS=""
for p in "${PLUGINS[@]}"; do
  found=""
  for d in "${PLUGIN_SRCDIRS[@]}"; do
    if [ -e "$d/libgst$p.dylib" ]; then found="$d/libgst$p.dylib"; break; fi
  done
  [ -n "$found" ] || { echo "找不到插件 libgst$p.dylib（先装 Homebrew 的 gstreamer 与 libnice-gstreamer）" >&2; exit 1; }
  PLUGIN_SRCS="$PLUGIN_SRCS$found"$'\n'
  mark_seen "libgst$p.dylib"
done

walk "$BIN"
while read -r p; do [ -n "$p" ] && walk "$p"; done <<< "$PLUGIN_SRCS"

# --- 拷贝 -------------------------------------------------------------------

mkdir -p "$FW" "$PLUGDIR"
# cp -L 解引用符号链接（nice 插件在 Homebrew 里就是个链接），bundle 里必须是真实文件
while read -r src; do [ -n "$src" ] && cp -L "$src" "$FW/$(basename "$src")"; done <<< "$LIBS"
while read -r src; do [ -n "$src" ] && cp -L "$src" "$PLUGDIR/$(basename "$src")"; done <<< "$PLUGIN_SRCS"
chmod -R u+w "$FW" "$PLUGDIR"

# --- 改 install name --------------------------------------------------------

# install_name_tool 每改一次都会警告「签名将失效」——后面统一重签，这里只在真出错时吐出来
int() {
  local err
  if ! err=$(install_name_tool "$@" 2>&1); then echo "$err" >&2; exit 1; fi
}

add_rpath_once() {
  local f=$1 rp=$2
  rpaths_of "$f" | grep -qx "$rp" || int -add_rpath "$rp" "$f"
}

drop_abs_rpaths() {
  local f=$1 rp
  for rp in $(rpaths_of "$f"); do
    case "$rp" in /*) int -delete_rpath "$rp" "$f" ;; esac
  done
}

retarget() {
  local f=$1 dep base
  while read -r dep; do
    [ -z "$dep" ] && continue
    base=$(basename "$dep")
    [ "$dep" = "@rpath/$base" ] && continue
    [ -e "$FW/$base" ] || continue
    int -change "$dep" "@rpath/$base" "$f"
  done < <(deps_of "$f")
  drop_abs_rpaths "$f"
}

for f in "$FW"/*.dylib; do
  int -id "@rpath/$(basename "$f")" "$f"
  retarget "$f"
  add_rpath_once "$f" "@loader_path"
done
for f in "$PLUGDIR"/*.dylib; do
  int -id "@rpath/gstreamer-1.0/$(basename "$f")" "$f"
  retarget "$f"
  # 插件在 Resources/gstreamer-1.0 下，库在 Frameworks 下：@rpath 要跨过去
  add_rpath_once "$f" "@loader_path/../../Frameworks"
done
retarget "$BIN"
add_rpath_once "$BIN" "@executable_path/../Frameworks"

# --- 重签 -------------------------------------------------------------------

# 先库后 app：--deep 只管 bundle 形式的嵌套代码，Resources 下的散装 dylib 得自己先签，
# 之后 app 这一签才封得住它们（否则 --verify --deep --strict 会判 unsealed contents）。
#
# 不加 --options runtime：ad-hoc 签名没有 Team ID，而 hardened runtime 的库校验按
# Team ID 比对，ad-hoc 的库对 ad-hoc 的主程序也会被判「different Team IDs」而拒载——
# 这是签名顺序改不了的，实测每个 GStreamer 库都在 dlopen 时被挡。换成 Developer ID
# 签名（公证需要）时再开 runtime：那时库与主程序同一 Team ID，库校验天然放行，
# 仍然不需要 disable-library-validation。
find "$FW" "$PLUGDIR" -name '*.dylib' -print0 | xargs -0 -n1 codesign --force --timestamp=none -s - >/dev/null 2>&1
codesign --force --deep --entitlements "$ENTITLEMENTS" -s - "$APP"

# --- 自检 -------------------------------------------------------------------

bad=0
while read -r f; do
  if otool -L "$f" | grep -q /opt/homebrew; then
    echo "残留 Homebrew 依赖：$f" >&2; otool -L "$f" | grep /opt/homebrew >&2; bad=1
  fi
  if rpaths_of "$f" | grep -q /opt/homebrew; then
    echo "残留 Homebrew rpath：$f" >&2; bad=1
  fi
done < <(printf '%s\n' "$BIN" "$FW"/*.dylib "$PLUGDIR"/*.dylib)
[ "$bad" = 0 ] || exit 1

codesign --verify --deep --strict "$APP"

echo "GStreamer 已打进 bundle：$(ls "$FW"/*.dylib | wc -l | tr -d ' ') 个库 + $(ls "$PLUGDIR"/*.dylib | wc -l | tr -d ' ') 个插件，$(du -sh "$APP" | cut -f1)"
