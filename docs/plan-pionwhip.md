# 计划：pionwhip——内嵌 WHIP 直通推流网关（OBS 高效编码的唯一路径）

命名更新（2026-09-02）：本计划中的 pionwhip 已定名 **Bellows**（`rtc/bellows`，选择器值 `bellows`）；语音内核 pionvoice 同期定名 **Ember**（`rtc/ember`）。旧值 `pion`/`pion_*` 仅 v0.3.0 做兼容映射，v0.3.1 起回落默认值并在启动日志告警。

状态：P1+P2 核心已实现（2026-09-02）。已完成：WHIP 握手（POST/PATCH/DELETE、201+Location+显式
Content-Length）、codec 白名单显式校验（opus/h264/h265/av1，VP8 等 400 拒绝）、pion 收流、
lksdk bot（identity={user}-obs）零转码直通发布、PLI/FIR 桥接（lksdk `WithRTCPHandler` 回调）、
同 key 重推顶替、断流/DELETE/DeleteEndpoint 清理、`ingest_provider=pion` 动态切换。
已验证：`go build/vet/test` 全过；信令测试覆盖握手/白名单/DELETE；本机端到端实测
（docker livekit v1.9.6 + ffmpeg 9 WHIP 推 h264+opus）：bot 进房、双轨发布成功、
断流后会话自动清理（房间在线数回落）。未做：OBS HEVC 实测与 P3 部署收尾（-full 档退役等）。
遗留坑：lksdk `PublishTrack` 的 opts 参数不能传 nil（内部日志行解引用 opts.Name 会 panic）。

## 动机（三合一）

1. **OBS 推 HEVC/AV1 的唯一路径**。已被源码证伪的备选：livekit-ingress 最新版（1.5.0 =
   GitHub latest，Pi 上已在跑）的 WHIP 视频白名单硬编码只有 VP8+H.264，无配置项，
   main 分支同样——升级路线不存在。而链路其余环节全绿（见"已验证地基"）。
2. **`-full` 镜像档退役**：OBS 推流不再依赖 ingress+redis+GStreamer，600MB Ubuntu 档
   消失，`-livekit` 档（~110MB）即全功能。现有部署的 ingress/redis 容器同步下线。
3. **摆脱不受控上游**：codec 白名单、鉴权、行为全部进自己进程（与 pionvoice 同哲学）。

## 已验证的地基（2026-09-01/02 实测）

- 浏览器观众收 H265：capabilities + decodingInfo(powerEfficient) + loopback 四编码全通。
- livekit-server 转发 H265：changelog "Enable H265 by default"，v1.13.6 已含。
- livekit-client 白名单已含 h265（投屏 HEVC 档已上线即活证据）。
- AV1 全链早已成熟（投屏 AV1 档在用）。
- 现有 `/w` 路由、禁言拦截（canPublishByStreamKey）、streamKey 存储（ingresses 表）、
  OBS 填法（服务器 /w + Bearer）全部保留兼容——**切换 ingest_provider 后 OBS 侧零改动**。

## 架构

`rtc.IngestProvider` 第二实现 `server/internal/rtc/pionwhip/`，进程内嵌（同 pionvoice）：

```
OBS ──WHIP(POST /w + Bearer)──> hearth（同源，握手直达不再反代）
                                  │ 验 streamKey（ingresses 表）+ admission（禁言拦截，现有）
                                  ▼
                            pion PeerConnection 收 RTP（ICE-Lite + UDP 单端口 mux，
                            复用 pionvoice 的公网探测/端口模式；白名单 h264/h265/av1 + opus）
                                  │ 原样转发（零转码）
                                  ▼
                            lksdk.ConnectToRoom（bot 参与者，identity={user}-obs 沿现有约定）
                            PublishTrack(TrackLocalStaticRTP) ──> livekit-server ──> 观众
```

要点：

- **RTCP/PLI 桥接**：观众关键帧请求经 SFU 到 sdk 侧，读出后向 OBS 发 PLI
  （lksdk 发布轨的 RTCP 读取路径为技术验证点 #2）；NACK 各段自理。
- **信令兼容**：POST /w（bearer）与 /w/{key} 两种填法、精确路径规范化、
  DELETE 资源清理——语义照抄现有代理层行为；**响应必须带 Content-Length**
  （ffmpeg 的 WHIP muxer 读不了 chunked——已踩过的坑，源头修复）。
- **管理操作零适配**：bot 参与者在 LiveKit 房间里，现有 MuteUserAudio/踢出
  （UpdateParticipant/RemoveParticipant 按 identity 前缀）天然生效。
- **配置**：`pionwhip_*` 命名空间（UDP 端口默认 47710、public IP 留空自动探测——
  与 pionvoice 共享探测工具函数）；`ingest_provider` 枚举加 `pion`，动态切换即时生效。
- **部署**：单机场景只多开一个 UDP 端口转发；aio `-livekit` 档内置本实现后即全功能。

## 阶段

**P1 骨架（h264 直通打通）**
- WHIP 握手（offer/answer、201+Location、DELETE）；pion 收流；lksdk 发布进房。
- 验证工具现成：ffmpeg 9 的 WHIP muxer（仅 h264，正好测 P1）+ 既有 UFO 测试流程。
- 验收：ffmpeg 推流 → 房间可见可听 → webrtc-internals 观众端指标（对齐上次 UFO 验收：
  满帧零丢包）。

**P2 高效编码（目标达成）**
- 白名单加 h265/av1；OBS（NVENC/VT HEVC）实测：推流 → 观众硬解 → 同 8Mbps 与
  h264 的清晰度对比（与投屏 HEVC 档同一验收标准）。
- 禁言/踢出/流量徽章回归。

**P3 收尾**
- 流状态（在线徽标）、断流清理与重推；`-full` 档退役（Dockerfile.aio/CI/README）；
  现有 compose 下线 ingress+redis；CLAUDE.md 增补 pionwhip 约定。

## 动手前技术验证点

1. pion/webrtc v4 的 H265 RTP payload（packetizer/depacketizer）支持程度。
2. lksdk PublishTrack 对 mime video/H265、video/AV1 的接受与 RTCP（PLI）读取 API。
3. OBS WHIP 对非 ingress 服务端的握手兼容（ICE-Lite answer、trickle 空适配）。

## 风险与回退

- pion H265 payload 缺口 → 先上 P1(h264)+av1，H265 贡献上游或裁剪本地实现。
- OBS 兼容性毛刺 → 对照 ingress 的 whip_handler 行为逐项对齐（源码公开）。
- 回退零成本：`ingest_provider` 切回 `livekit` 即恢复现有 ingress 路径，两实现并存。
