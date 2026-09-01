# 计划：有限码率下的清晰度提升（HEVC 亚秒路线优先，Oryx 降级备选）

状态：计划 v2，待评审。2026-09-01。
v1（Oryx HLS 主线）已被实测推翻——保留于文末"备选：Oryx 直播频道"。

## 目标

**在有限码率（家宽上行）下，不牺牲帧率的前提下提高清晰度。**
= 同码率换更高效编码：HEVC ≈ H.264 观感 1.5 倍（用户设备有 HEVC 硬编硬解；无 AV1 硬编）。

## 实测确立的事实（2026-09-01）

1. Chrome（151）`RTCRtpReceiver/Sender.getCapabilities('video')` 均含 `video/H265`；
   `mediaCapabilities.decodingInfo(webrtc, H265)` → supported + powerEfficient——
   **浏览器 WebRTC 收发 HEVC 已就绪，发送侧即平台硬编（VideoToolbox/NVENC）**。
2. livekit-server changelog："Enable H265 by default (#3773)"，SFU forwarder/buffer/默认
   codec 配置均有 H265 路径——生产 v1.13.6 已含。
3. livekit-ingress WHIP 直通白名单拒 HEVC/AV1（实测 unsupported codec）——**全链路唯一确定的墙**。
4. livekit-client 的 `videoCodec` TS 枚举无 'h265'——SDK 层面是最后一个未知数。

结论：亚秒级 HEVC 进现有房间体系，只剩 SDK/接入两个薄层要打穿，不需要新增任何服务。

## 路线（按性价比排序）

### P0 实验：浏览器投屏 HEVC 档（半天定生死）

浏览器硬编 HEVC → LiveKit SFU → 观众硬解。若通：零 CPU、亚秒、进现有房间、
不需要 OBS——目标一步达成。

- 实验：livekit-client 发布投屏时说服其协商 h265——依次尝试
  a) `videoCodec: 'h265' as VideoCodec`（运行时字符串直透）；
  b) 发布后拿 RTCRtpTransceiver `setCodecPreferences` 重排 H265 优先 + 重协商；
  c) SDK 内部 SDP munging 点。
- 验收：双端 webrtc-internals 确认收发均 H265 且 powerEfficient；同 8Mbps 与 H264 档
  对比游戏画面清晰度。
- 落地形态：投屏编码枚举加 hevc 档（软硬编标注体系自动生效——mediaCapabilities 会
  如实报告硬编）；观众端无硬解设备的兜底靠 `backupCodec: h264`（SDK 既有机制，验证
  其对 h265 主 codec 是否工作）。
- 风险：SDK 白名单硬校验导致 a/b/c 全堵 → 转 P1；或等 SDK 官方支持（协议侧已 ready，
  大概率是时间问题）。

### P1：pionwhip 直通网关（OBS 路径，几天量级）

OBS NVENC HEVC/H.264（+将来 AV1）→ 自研纯 Go WHIP 网关（白名单自定：h264/h265/av1）
→ lksdk 以 bot 参与者发布进房 → SFU 转发。

- 同时解决：OBS 高效编码接入、`-full` 镜像档退役（600MB→0）、ingress 依赖清零。
- 工程要点：pion/webrtc v4 的 H265 payload 支持验证、PLI/RTCP 桥接、
  lksdk PublishTrack(TrackLocalStaticRTP)。
- 与 P0 关系：互补不互斥——P0 管浏览器投屏，P1 管 OBS 推流，共享"SFU 转发 H265 已通"的地基。

### 验证顺序

1. P0 实验（先打最薄的一层）；
2. 无论 P0 成败，P1 立项（OBS 路径独立成立）；
3. 观众设备矩阵实测：朋友的每台设备跑一次 decodingInfo(H265)（自托管的优势：
   兼容性按可枚举的朋友设备算，不需要全网覆盖）。

## 推流端兜底矩阵（编码能力不构成硬约束）

发送端有多条独立可行的路，按优先尝试序：

1. **投屏机浏览器硬编**（P0 主路）：投屏所在机器的 Chrome H265 send 能力即平台硬编
   （Mac=VideoToolbox ✓；Windows=NVENC HEVC，GTX 10 系+基本都有，**逐台实测确认**，
   不从一台设备的能力推断另一台）。
2. **OBS 直推**（P1 主路）：Windows OBS NVENC HEVC，或 Mac OBS Apple VT HEVC。
3. **采集卡 + Mac OBS**：Windows 游戏画面 → 采集卡 → Mac 端 VideoToolbox HEVC 硬编推流——
   游戏机零编码负担的兜底。
4. **Mac ffmpeg 转码**：hevc_videotoolbox 硬转码可行，但注意 **ffmpeg 的 WHIP muxer
   只支持 h264（实测）**——ffmpeg+HEVC 只能配 RTMP/SRT 出口（即 Oryx 备选线），
   WHIP 推 HEVC 请用 OBS。

观众端才是需要逐台验证的一侧：朋友每台设备跑 `decodingInfo(webrtc, H265)`。

## 备选：Oryx 直播频道（降级挂起）

v1 计划的 Oryx 路线（频道级 stage_type + HLS HEVC 直通 + hls.js 播放引擎）整体挂起，
触发条件收窄为以下之一：
- 需要 RTMP/SRT 老工具推流接入；
- 需要转推大平台（B 站等，观众规模化的正解——家宽上行只出一份）；
- P0/P1 均被堵死（HEVC over WebRTC 链路出现未预见的墙）。

届时按 v1 设计执行（git 历史可查完整方案：频道级舞台类型、DDNS 域名+TLS 复活、
LL-HLS 2-5s 延迟档、多流并行）。

## 不变量

- 观众上限 = 家宽上行 ÷ 码率，任何路线不变；规模化 = 转推平台。
- 语音线不受影响（pion/livekit 照旧）；名册/禁言/踢出体系不动。
