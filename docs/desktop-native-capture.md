# 原生投屏实现与验证边界

## 桥合同

- `capabilities`：`native_publish`、`platform`、`app_audio`、`publish_codecs`（实际硬编产帧探测得到的 `h264` / `h265`）、`publish_encoders`（各编码实际选中的 GStreamer 元素名，如 `h265: nvh265enc`）、`native_publish_error`（探测失败时逐个候选的原因：插件未注册、只有软件实现、启动失败或超时）、`local_server`。壳内设置页的编码标注只看 `publish_encoders`，浏览器 `MediaCapabilities` 的软/硬编预测说的是浏览器自己怎么编，与原生管线无关。
- `list_sources`：每项增加 `audio_scope: application | system | none`。源 ID 是不透明值。Windows 只列用户能认出、也能真的投出去的顶层窗口：cloaked（`DWMWA_CLOAKED`）、`WS_EX_TOOLWINDOW`、有属主且无 `WS_EX_APPWINDOW`、本进程、外壳类名（`Progman`/`WorkerW`/托盘/任务视图）与过小的窗口都不进列表，`EnumWindows` 的 Z 序保持不变。
- `start_publish`：扁平参数 `endpoint, token, source_id, codec, width, height, fps, bitrate_kbps, audio`；返回 `{codec}`，表示实际编码。HEVC 不可用可回落 H.264。
- `update_publish`：扁平参数 `width, height, fps, bitrate_kbps`。不改变音频范围、源或实际编码；编码偏好下次开始生效。
- `source_preview`：扁平参数 `source_id`；返回 JPEG data URL 或 `null`。调用方串行按需请求，关闭面板后取消尚未发出的请求。列表不等预览，只给视口内、悬停、选中的源排队（优先级 选中 > 悬停 > 视口顺序）；壳侧两次预览最小间隔 100 ms，单次预览 3 秒上限，超时返回 `null` 让 UI 显占位图，不堵住后面的源。

尺寸为上限，保持比例、不放大、NV12 向下取偶数。限制为宽 2–7680、高 2–4320、帧率 1–120、码率 500–100000 kbps；越界报错，不静默截断。

## 生命周期与音频

画质更新保留同一 `whipclientsink`、信令对象、视频请求 pad、音频支路。Rust 阻断旧视频出口，仅替换视频 bin；不调用 `Publisher::start`，不重新 POST，不重取或复用过期设备票。新支路必须在期限内产出编码画面，失败则明确停止，要求重新开始走网页的短票签发和入场检查。旧发布看门狗按实例标记判定，旧尺寸查询也不能覆盖更新后的设置。

Windows 使用 WGC 和 WASAPI 插件；HWND、PID、进程创建时间由 Rust 枚举并复验，视频/音频帧输出前检查目标是否仍有效。应用名来自进程文件名，读取失败为空。窗口音频只取所属进程树，失败不回落系统声；进程树音频需要 Windows build 20348+。整屏为默认播放设备的系统 loopback，可能包含其他应用声音。WGC 实际 CAPS 决定缩放尺寸，避免窗口边框与 DPI 的差异，并在 CAPS 变化时重新按上限缩放。

macOS 使用 ScreenCaptureKit → appsrc；窗口音频属于应用，整屏音频属于系统，排除本进程音频。`audio=false` 不创建音频支路、不注册 SCK 音频输出，`capturesAudio=false`。内容尺寸低频复查后仅更新视频支路；SCK 流重启时复用原音频 appsrc，范围不变，可能有短暂音频间断。旧 SCK 回调停用后不能误报新流失败。

桌面壳强制单实例（三平台一致）：协议激活与再次启动都会另起进程，单实例插件把 argv 转发回已运行实例后让新进程退出，壳只做 `show`/`unminimize`/`set_focus`；`hearth://` URL 由 single-instance 的 `deep-link` feature 交给深链插件，壳不重复 `emit`（一次性码送两遍会提前作废），转发时向 stderr 打一行 `deep-link forwarded: hearth://…`（不含码）。

预览没有 WHIP 或音频支路：最大 480×270、2 fps、JPEG quality 65、单图原始 JPEG 不超过 128 KiB。队列限时等待，SCK 枚举/启动和取帧分别限时；每次函数返回前释放采集和管线，无后台持续预览。关闭 UI 时已发出的单次请求在期限内结束，后续请求由 UI 取消。

## 官方 API 依据

- [GStreamer WGC 源](https://gstreamer.freedesktop.org/documentation/d3d11/d3d11screencapturesrc.html)：`capture-api=wgc`、`window-handle` / `monitor-handle`。
- [WASAPI2 源](https://gstreamer.freedesktop.org/documentation/wasapi2/wasapi2src.html)：`loopback-mode=include-process-tree`、`loopback-target-pid`；窗口音频隔离及系统版本要求。
- [NVENC](https://gstreamer.freedesktop.org/documentation/nvcodec/nvh264enc.html)、[QSV](https://gstreamer.freedesktop.org/documentation/qsv/qsvh264enc.html)、[AMF](https://gstreamer.freedesktop.org/documentation/amfcodec/amfh264enc.html)、[Media Foundation](https://gstreamer.freedesktop.org/documentation/mediafoundation/mfh264enc.html)：各自的 GOP、码率、低延迟属性。MF 还排除没有 Hardware 分类的实现；候选均以短管线实际产帧判定。
- [VideoToolbox 硬编](https://gstreamer.freedesktop.org/documentation/applemedia/vtenc_h264_hw.html)、[WHIP sink](https://gstreamer.freedesktop.org/documentation/rswebrtc/whipclientsink.html)。
- [窗口所属进程](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-getwindowthreadprocessid)、[进程创建时间](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getprocesstimes)。
- [SCK capturesAudio](https://developer.apple.com/documentation/screencapturekit/scstreamconfiguration/capturesaudio)。Objective-C 与 Win32 绑定同时对照所用版本的本地生成声明。

## 已验证与待验证

本机已通过 macOS `cargo build`、尺寸/范围测试、SCK 配置的音频关闭测试，以及需要运行时的隔离管线测试：

```sh
cargo test --lib
cargo test --lib runtime_update_keeps_sink_and_audio_and_continues_frames -- --ignored --nocapture
```

运行时测试用真实硬件编码和彩条，在 640×360/30 → 960×540/60 → 480×270/15 更新后继续产帧，检查 sink/音频支路/连接 pad 身份不变、GStreamer segment 换算后的 running-time 连续，并生成受限 JPEG 预览。测试以 fakesink 隔离网络，更新路径不消费 endpoint/token；它证明局部支路替换，**不等于真实 WHIP 接收端或超过十分钟会话的端到端验收**。

Windows Actions 34931957820 已通过 `cargo check`、release/NSIS 构建、安装文件 manifest 与 exe 同目录 DLL 校验、插件检查及清除全局 GStreamer PATH 后的 GUI 启动检查；这些结果仅覆盖 CI 编译与安装启动。

Windows 真机 WGC/进程树音频/各 GPU 硬编、两端真实 WHIP 热更新接收、SCK 更新音频连续性和源预览关闭体验仍未验证。Windows WebView2 私有 CA 信任尚未实现；上述测试也不涵盖 macOS ad-hoc 签名或关闭库校验改动的验收。
