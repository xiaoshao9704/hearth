# 计划：自研舞台内核（Ember 补视频能力，LiveKit 退场）

状态：**路线 A 已定，待其他会话实施。2026-09-03。** 前置（Provider 注册制、Bun 迁移、自动端口映射 `plan-portmap.md`）均已合入。
路线 A = 下文「第 2 段实施路线」的第 1 步（模块方式引用 `pkg/sfu`）+ 第 2 步（写胶水）**提到最前作为起手**，
转发从第一天就建在 `pkg/sfu` 上，不再自写一次性的单层直通转发器；第 3 步（vendor + 去 protocol 化）后置到 LiveKit 退场前。
决策依据是 2026-09-03 的 spike（见该节）。**起手交付**是一条最小链路：Bellows 的 `TrackRemote` 进程内直递给 `sfu.Receiver`，
经 `DownTrack` 转发到一个浏览器——跑通即舞台内核骨架立住。

## 动机与边界

LiveKit 是最后一个外部内核依赖：投屏/摄像头/OBS 观众侧全靠它，带来 `-livekit`/`-full` 镜像档、
redis、`use_external_ip` 探测靠 watchdog 重启、HEVC 走不通 ingress 等一串外部约束。目标是让 Ember
实现 `rtc.StageProvider`，把舞台线收进自己进程，LiveKit 降级为可选的注册实例，功能对齐后退役。

本计划**不做**的：转推大平台（RTMP 出口）、录制、多 SFU 级联、TURN 中继（服务器公网直达是前提）。

三段交付，每段可独立上线；LiveKit 全程保留到第 3 段，注册制保证两者并存、切选择器即回退。

## 目标拓扑

```
浏览器 ──语音信令/媒体──▶ hearth 进程内 Ember（语音，云上，现状）
浏览器 ──舞台信令 /providers/{alias}/voice（hearth 反代 + 通行证）──▶ cmd/ember 远端进程（舞台，上行充足的网络）
                                                                    ▲ 媒体直连（LAN host + 公网 srflx 双候选）
OBS ──WHIP /providers/{alias}/w（hearth 反代 + 通行证）──▶ 同一个 cmd/ember 进程内的 Bellows，直通发布进房
```

要点：
- **远端舞台进程 = Ember 独立形态**（`cmd/ember`），与今天 `cmd/bellows` 同一模式：hearth 做入场判定并签
  通行证，远端只验签，无回调。`cmd/bellows` 并入 `cmd/ember`（同进程内 Bellows 直接把轨写进房间，
  少掉现在经 LiveKit 的一跳；`lksdk` 依赖随之移除）。单机形态（Windows 单文件版）则是进程内 Ember 同时
  承担语音与舞台。
- 注册制里新增实例类型 `ember-remote`（能力 voice+stage+ingest，params `remote_url`/`shared_secret`），
  内建 `ember` 补 stage 能力。前端不变：两条线都是 `ember` 引擎，各一个实例。
- 语音线继续留在 hearth 进程内（云上，语音永远流畅的物理隔离原则不变）。

## 第 1 段：直通 SFU（目标：自己投屏给几个人看，不再经 LiveKit）

### 1.1 传输基建（`rtc/lite`）

- `NewAPI` 增加拦截器注册：`RegisterDefaultInterceptors`（NACK 生成/应答、RTCP 报告、PLI 转发所需的
  `ReceiverReport`）。今天的 Ember/Bellows 没有注册拦截器，纯音频靠 opus 容错，视频不行。
- **双候选通告、公网 IP 动态恢复、探测源：已完成**（2026-09-03，见 `plan-portmap.md` 与 `rtc/lite`）。
  现状：pion 只宣告本机 host 候选；外部地址在 SDP 出口由 `lite.Announcer.Announce` 追加为 srflx 候选
  （端口映射结果与 STUN 结果并列、按地址去重），显式 `*_public_ip` 仍走 pion 改写规则；`lite.Transport` 持有
  UDP mux 与 MediaEngine 只建一次、`webrtc.API` 按 PC 组装；探测由进程内周期任务刷新、端口映射变化再触发一次，
  公网 IP 变化不重启。自动端口映射（PCP/NAT-PMP/UPnP、向上游级联、v6 pinhole）已上线，watchdog 不再需要。
  舞台内核直接复用这套，不要再加第二套探测/宣告。
- **硬性要求：StageProvider 必须接上端口映射与宣告自愈，与 Ember 语音线、Bellows 完全同款**（用户 2026-09-03 明确）：
  - **建连**：PC 一律经 `lite.Transport.NewAPI(announcer.Rules(ctx))` 组装（mux 与 MediaEngine 只建一次，API 按 PC 组装）。
  - **宣告**：所有 SDP 出口（offer 与 answer）都经 `announcer.Announce(ctx, sdp)` 追加外部 srflx 候选；
    host 候选由 pion 按 PC 逐个收集，本机 LAN 地址变了新 PC 自然拿到新地址。
  - **自愈**：实现 `RefreshAnnounce(ctx)` / `AnnounceSnapshot()`（即 `api.refreshableAnnouncer`），纳入
    `api.RefreshAnnounce` 的周期刷新（`lite.DefaultAnnounceTTL`）与 `Mapper.OnChange` 的映射变化回调；
    公网 IP 或映射变化后新会话即拿到新候选，不重启、不动在途会话。
  - **端口映射**：构造函数接收 `lite.MappedFunc`（进程内为 `Mapper.UDPExternal`），舞台媒体 UDP 端口进
    `api.PortWants`（`stage_provider` 选中内建 ember 时加入，与 `ember_udp_port`/`bellows_udp_port` 同列）；
    v6 pinhole 随同一份 wants 自动覆盖，不另配。
  - **远端形态** `cmd/ember`：`portmap.New()` → `NewRemote(..., mapper.UDPExternal)` → `OnChange` 起协程调
    `RefreshAnnounce` → `go mapper.Run(ctx, wants)` → 周期 ticker → 退出 `Close(新 ctx)`，**逐行照抄 `cmd/bellows/main.go`**，
    `PORTMAP_MODE=off` 时 wants 返回空。
- **ICE-TCP 候选**：`SetICETCPMux` 在同一端口号上提供 TCP 候选，覆盖客户端网络封 UDP 的情况（不是 TURN，
  服务器仍是公网直达）。

### 1.2 服务端房间模型（`rtc/ember`）

- `participant` 从「一条上行 opus 轨」扩展为「上行轨集合」：`audio`、`camera`、`screen`（+ `screen-audio`
  伴音）。**每条上行轨不自写直通转发，直接交给 `pkg/sfu`**：`TrackRemote` 建 `sfu.Receiver`，每个订阅者一个
  `DownTrack`（实现 `webrtc.TrackLocal`）挂到其 PC 的 transceiver；订阅关系仍是 N-1 全量，走现有的服务端主动
  `renegotiate`。单层与分层是同一条路径，第 2 段只是把 `StreamAllocator`/层选择接上，不换转发器。
- 信令扩展（沿用 `sigMsg`，加字段不改形状）：
  - C→S `offer` 不再只在入会时一次：开/关摄像头、投屏时客户端 `addTransceiver(sendonly)` 后重协商；
    与服务端主动 offer 的 glare 用现有 `negMu` 串行化 + 客户端「服务端 offer 未应答前不发 offer」规则解决。
  - `Mids` 的值从 `identity` 扩为 `identity/source`（`source ∈ audio|camera|screen|screen-audio`），
    前端据此挂到 `onVideoTrack(part, source, el)`。
  - `peerInfo` 加 `camera`、`sharing` 布尔（`EPart.sharing` 已存在），`roster` 广播时机加「轨增删」。
  - 新增 C→S `keyframe`（identity, source）：观众端解码失败/首帧超时时请求，服务端转成对发布者的 PLI。
- **PLI/FIR 与 NACK 由 `pkg/sfu` 自理**：`DownTrack` 收观众 RTCP 后由 `Receiver` 向上行发 PLI（自带节流），
  buffer 负责 NACK 重传，不必自写 RTCP 读循环。唯一要接的是 Bellows 那条：上行是 Bellows 的 WHIP PC，
  关键帧请求经现有的 `rtc.WithKeyframeRelay`/`KeyframeRelay`（挂在 ctx 上）回到推流端，契约见 CLAUDE.md。
- 编码白名单：opus + H.264（pm=1）+ H.265 + VP9 + AV1，第 1 段只做**单层**转发（VP9/AV1 不带 SVC 时也是单层）。
- 禁言语义：`MuteUserAudio` 契约是禁全部媒体，扩展后要丢弃该参与者全部上行（含视频）并回收其下行 sender。

### 1.3 远端进程与通行证

- `cmd/ember`：环境变量 `EMBER_SHARED_SECRET`、`EMBER_ADDR`（WS 信令）、`EMBER_UDP_PORT`、`EMBER_PUBLIC_IP`，
  外加 Bellows 的 `BELLOWS_*` 端口键；一个进程两个监听（WS 信令、WHIP HTTP）+ 一个 UDP mux（两者复用
  同一 `lite.NewAPI`，端口只放行一个）。端口映射与宣告自愈按 1.1 的硬性要求接线（`PORTMAP_MODE` 同名沿用）。
- 信令通行证：hearth 的 `/providers/{alias}/voice` 对 `ember-remote` 实例做完票据校验后，把
  `{room, identity, name, muted, exp}` 签成 grant 放请求头再反代 WS（复用 `bellows/grant.go` 的签验，
  抽到 `rtc/lite/grant.go` 供两者用）；远端验签后进入现有 `HandleJoin`。一次性入场票机制不变
  （票在 hearth 侧消费）。
- Bellows 在同进程内发布：`rtc.Publisher` 能力接口**已存在**（Bellows 每次发布取当前舞台线实例的 Publisher，
  回执一律走 ctx：`WithKeyframeRelay`/`WithPublishLost`，见 CLAUDE.md），目前只有 `livekitrtc` 一个实现。
  本段新增**进程内实现**：把 Bellows 的 `TrackRemote` 与 `RTPReceiver` 直接交给 `sfu.NewWebRTCReceiver`，
  不再开第二条 PeerConnection——这就是「同进程直递」，省掉一次 DTLS/SRTP 加解密与回环往返。
  远端 Bellows 形态继续用网络 Publisher，两种并存，Bellows 本身不改。`lksdk` 桥接随 LiveKit 退场删除。

### 1.4 前端（`web/src/engine/ember.ts`）

- `setCamera`/`setScreen`：`getUserMedia`/`getDisplayMedia` → `addTransceiver(track, {direction:'sendonly'})` →
  按 prefs 设 `setCodecPreferences`（H.264/H.265/VP9/AV1）与 `sender.setParameters`（maxBitrate、帧率、
  分辨率缩放）→ 发 `offer`。伴音作为独立 `screen-audio` 轨。
- 接收：按 `Mids` 的 `identity/source` 创建 `<video>`/`<audio>` 元素回调房间视图（现有 `EngineCallbacks` 足够）。
- `screenStats`/`remoteVideoStats`/`screenEncoderInfo` 用 `getStats` 实现（LiveKit 引擎里已有同样逻辑，
  抽成共享函数）。
- 房间视图 `combined` 判定不变：两条线都是 ember 但 alias 不同时仍是双连接。

### 1.5 验收

- 三个浏览器进同一房：A 投屏（H.264 单层 8Mbps）+ 开麦，B、C 观看；B 刷新页面重连后画面在 2 秒内恢复
  （PLI 转发生效）；C 弱网（devtools 限速）丢包时 NACK 重传生效（webrtc-internals `nackCount>0`、无马赛克）。
- OBS 经 Bellows 推 HEVC → 观众硬解可见；`{user}-obs` 出现在名册；禁言后视频与音频同时消失。
- 拔掉公网 IP（模拟：改探测器返回值）后新进房观众拿到新候选并连通，无需重启进程。
- 端口映射：舞台进程启动即在网关上出现舞台媒体端口的映射（v4）与 pinhole（v6，有 GUA 时）；退出后撤销；
  `PORTMAP_MODE=off` 时无任何映射；映射建立后 SDP 里出现「映射外部地址:端口」的 srflx 候选。
- LAN 观众与公网观众同时在房，各自走 host / srflx 候选（webrtc-internals 看选中候选对）。
- 单测：转发器（pion 本机回环发布 → 两个订阅 PC 都收到相同序列的 RTP）；PLI 节流；`Mids` 编解码；
  grant 签验复用测试。

### 1.6 工作量

服务端约 1.2k 行（含删 lksdk 桥接），前端约 600 行，1.5–2 周。

## 第 2 段：分层与自适应（LiveKit 真正的技术含量在这里）

- **带宽估计**：注册 TWCC 拦截器（发送端）与 REMB；每个订阅端维护可用带宽估计（先用 REMB/TWCC 的现成
  估计值，不自研拥塞控制）。
- **SVC 层选择**（VP9 / AV1，`L2T2_KEY`）：解析 VP9 payload descriptor 与 AV1 dependency descriptor
  头扩展，按订阅端目标层丢弃高于目标的空间/时间层；**必须做 RTP 序列号/时间戳重写**（丢包后序列连续），
  空间层切换只在关键帧边界生效。这是本段主要复杂度，单独成包 `rtc/ember/svc`，用合成包序列做表驱动单测。
- **H.264 单层 + 分辨率降级**：H.264 无分层，弱网观众的兜底是投屏端按 REMB 自降码率（发送端 `setParameters`），
  服务端不做转码。
- 摄像头：与投屏同路径，只是默认编码与码率不同；`switchCamera` 走 `replaceTrack`。
- 观众端指标：`remoteVideoStats` 反映实际拿到的层（现有接口语义不变）。
- 验收：A 用 VP9 `L2T2_KEY` 投屏，B 满速拿 L1T1 全层，C 限速 2Mbps 自动降到 L0，A 端无感；切换在
  关键帧边界，无花屏；`codec-test` 页分层矩阵与实际一致。
- 工作量：2–3 周，其中 SVC 重写占一半以上。

### 第 2 段实施路线（路线 A 的主体）：模块引用 LiveKit `pkg/sfu` 起手，vendor + 去 protocol 化后置

**为什么不自写**：第 2 段的难点（分层选择 + 包级重写 + 拥塞控制）在 livekit-server 的 `pkg/sfu` 里是打磨多年的
成熟实现，Apache-2.0（`pkg/sfu/NOTICE` 注明部分源自 MIT 的 ion-sfu），可以合法搬入 MIT 项目，只需保留文件
许可证头与 NOTICE。

**Spike 结果（2026-09-03，临时模块以依赖方式引用 `livekit-server v1.13.6`，无任何 `replace`）**：
- `pkg/sfu` 单独 import 对上游 `pion/webrtc/v4 v4.2.18` **编译通过**，因此 Bellows 手里上游 pion 的 `TrackRemote`
  与 `sfu.Receiver` 要的是**同一个 Go 类型**，进程内直递成立，不需要切到 LiveKit 的 pion 分叉。
- `pkg/rtc`（participant/room）对上游 pion只差**一行**：`transport.go` 里的 `se.EnableSped(true)`，分叉私有的发送
  平滑开关。结论：**不 import `pkg/rtc`**，它只作接法参考（只读）；房间/订阅/participant 薄层自己写。
  若哪天急需单二进制里的完整 LiveKit，vendor 一份 `transport.go` 删掉这一行即可，作为备选不进主线。
- `pkg/sfu` 的传递依赖：`livekit/protocol`（22 个包，protobuf 类型与 logger/utils）、`psrpc`、`mediatransportutil`；
  都是类型与工具，不带 redis/nats 等运行时服务。作为过渡接受，第 3 步一次性清掉。

**Spike 结果（2026-09-02，稀疏 checkout 验证）**：
- `pkg/sfu` 约 38k 行非测试 Go，子包：buffer / rtpstats / videolayerselector(+temporallayerselector) / codecmunger /
  streamallocator / bwe(remotebwe, sendsidebwe) / pacer / ccutils / streamtracker / connectionquality / rtpextension
  (dependencydescriptor, abscapturetime, playoutdelay) / packettrailer / audio / datachannel / interceptor / utils。
- LiveKit 主仓库用 `replace` 把 pion/webrtc、ice、dtls 换成自己的 `-warp` fork；**去掉这些 replace 后 `pkg/sfu` 对上游
  pion（v4.2.18）`go build ./pkg/sfu/...` 通过，13 个子包单测全过**。fork 只服务于它的传输层（`pkg/rtc`），SFU 核心
  不依赖 fork 私有 API。hearth 继续用上游 pion，不引 fork。
- 主代码对 livekit-server 内部包的引用只有 `pkg/utils`（时间版本号等工具）与 `pkg/telemetry/prometheus`
  （`forwardstats.go` 一处指标上报）；测试文件另引 `pkg/testutils`（vnet）。

**依赖面清单（非 pion、非标准库，按引用次数）**——这就是要「换成自有标准结构」的全部内容：

| 来源 | 用到的符号 | 处理 |
|---|---|---|
| `livekit/protocol/livekit`（protobuf） | 标识：`TrackID`、`ParticipantID`；枚举：`VideoQuality{OFF,LOW,MEDIUM,HIGH}`、`VideoLayer_Mode`、`TrackSource`、`TrackType`、`ConnectionQuality`；结构：`TrackInfo`、`VideoLayer`、`RTPStats`、`RTPDrift`、`RTCPSenderReportState`、`PlayoutDelay`、`AnalyticsStat/Stream/VideoLayer`；状态快照：`RTPForwarderState`、`RTPMungerState`、`VP8MungerState` | 新建 `server/internal/rtc/media` 包，用**普通 Go 类型**重定义（string 类型 ID、int 枚举、平铺结构体）。`Analytics*` 与 `*State`（多节点迁移用）直接删除对应代码路径 |
| `livekit/protocol/logger` | `Logger` 接口（`Debugw/Infow/Warnw/Errorw/WithValues`）、`Proto`/`ObjectSlice`/`Int64Slice` zap 字段助手 | `media.Logger` 小接口 + `log.Printf` 适配器；zap 字段助手改为直接传值 |
| `livekit/protocol/utils` | `WrapAround`、`RangeMap`、`TimedVersion`、`TimedAggregator`、`GetHeaderExtensionID`、`FindRTXPayloadType`、`DownTrackSpreader`、`CloneProto` | 纯算法，复制进 vendored `sfu/utils`；`CloneProto` 随 protobuf 结构一起消失 |
| `livekit/protocol/utils/mono` | `Now`、`UnixNano`（单调时钟） | 十行，复制 |
| `livekit/protocol/codecs/mime` | `MimeType` 及判定函数 | 复制为 `media/mime` |
| `livekit/mediatransportutil` | `NtpTime`/`ToNtpTime`、`codec.VideoSize/Load/Store/VP8`、`bucket`（RTP 包桶） | 只复制用到的文件进 vendored 树（同为 Apache-2.0） |
| `frostbyte73/core.Fuse`、`go.uber.org/atomic`、`zapcore`、`gammazero/deque` | 熔断器、原子类型、日志编码、双端队列 | Fuse 用 `sync.Once`+chan 替；atomic 换标准库 `sync/atomic` 类型；zapcore 的 `ObjectMarshaler` 实现删除；deque 保留（微型依赖） |
| `livekit-server/pkg/telemetry/prometheus` | `forwardstats.go` 指标 | 删除或换成 hearth 自己的计数钩子 |

**分三步走，每步可编译可测**：
1. **先以模块方式引用，不急着复制（路线 A 起手，2026-09-03 已定）**：`go get github.com/livekit/livekit-server@v1.13.6`
   （钉死这个版本，spike 即在此版本验证）后直接 import `.../pkg/sfu`（它在 `pkg/` 下不是 internal，可引用）。依赖方的 `replace` 对主模块无效，Go 自动用 hearth 的上游 pion
   编译它——spike 已证明可行。代价是依赖图：`pkg/sfu` 经 `livekit/protocol` 与 `pkg/telemetry/prometheus` 连带
   约 60 个外部模块（redis、nats、psrpc、grpc、otel、prometheus…），但 hearth 今天经 lksdk/protocol **已经背着其中大半**，
   净增有限，且第 3 步会一次性清掉。这一步的价值是在不提交 38k 行代码的情况下把胶水写通、验证功能。
   钉死 commit；不删子包（模块引用改不了源码）。
2. **写胶水（路线 A 的主体交付，同时覆盖原第 1 段的转发）**：从 pion `TrackRemote` 建 `Receiver`（`NewWebRTCReceiver`）；
   每个订阅者一个 `DownTrack` 绑到其 `RTPTransceiver`（实现了 `webrtc.TrackLocal`）；每个订阅 PC 挂一个 `StreamAllocator`
   接 TWCC/REMB；`Forwarder` 按订阅端目标层选层。LiveKit 的 `pkg/rtc/mediatrack*.go`、`subscribedtrack.go`、`transport.go`
   是接法参考（只读，不搬，也不 import）。**起手 spike 就是这一步的最小子集**：一条 Bellows `TrackRemote` →
   `Receiver` → 一个 `DownTrack` → 一个浏览器订阅 PC，单层、无分层，跑通即骨架立住；随后再补第 1 段的房间模型、
   信令与前端。验收同第 2 段。
3. **vendor + 去 protocol 化**（改源码必须先复制，这一步才把代码拿进仓库）：`pkg/sfu` + `pkg/utils` 复制到
   `server/internal/rtc/ember/sfu`（保留许可证头与 NOTICE，`sfu/VENDOR.md` 记来源 commit），改 import 路径，删掉不需要的
   子包（`datachannel`、`sfufakes`、`testutils`、`interceptor` 若仅为 pkg/rtc 服务）与 `forwardstats.go` 的 prometheus 上报；
   按上表把外部类型替换为 `rtc/media`，删除 `Analytics*`/状态迁移代码路径；`go mod tidy` 后 `livekit-server`、
   `livekit/protocol`、`mediatransportutil`、`server-sdk-go` 全部从 go.mod 消失。第 2 步的胶水只需改 import 路径。
   机械替换约 30 个类型、5 个日志方法、15 个工具函数，估计 3–5 天；做完才允许进入第 3 段退场。

**维护策略**：冻结在 vendor 时的 LiveKit 提交（记录 commit hash 在 `sfu/VENDOR.md`），不追上游；需要修 bug 时 cherry-pick。
第 3 步之后代码已与上游分叉，本来也无法机械同步。

## 第 3 段：LiveKit 退场

- `livekitrtc` 保留为可注册类型再发一版（给外部部署者迁移窗口），下一版删除包、`lkroom`/`lkingress`/
  `lktoken`、`livekit-client` 前端引擎与依赖。
- aio 镜像：`-livekit`/`-full` 两档退役，`aioinit` 只剩 hearth（或直接不需要 aioinit）；`deploy/` 的
  livekit/ingress/redis 模板与 compose 服务删除；release 工作流去掉 QEMU 步骤。
- 文档：README 架构图、部署段、CLAUDE.md 铁律（`livekit_*` 命名空间、ingress 的坑）清理。
- 工作量：2–3 天。

## LiveKit 能力对照：哪些不做、哪些换个形式做

| LiveKit 能力 | 结论 | 理由 / 替代 |
|---|---|---|
| RTMP 转推大平台 | 不做 | 需要 egress 服务（从未部署过）；且 RTMP 需要 AAC，服务端得转码音频。推流端 OBS 本身就能多路输出，编码器在推流者手里，转推就该在那里做。 |
| 录制 | 不做，留钩子 | 同样依赖 egress；服务端录制要多轨对齐与容器封装。开黑录高光用 OBS 本地录制更好。`Publisher` 接口天然可以 tee 一份 RTP 落盘，将来要做单轨 dump 成本很低。 |
| 多 SFU 级联 | 不做 | 只有规模化/多地域才需要，本项目是十人以内。 |
| TURN 中继 | 不做 TURN，但做 **ICE-TCP** | 服务器公网直达模型下客户端不需要 TURN；真正会遇到的是**客户端网络封 UDP**（公司/校园网、部分移动网络），pion 的 `SetICETCPMux` 一行即可提供 TCP 候选，放进第 1 段 1.1。 |
| 无公网 IP 场景 | 做 **全 ICE 模式**，不做 mesh | 见下节。 |

### 无公网 IP 的少数人场景

场景：没有任何一方有公网 IP（家宽丢公网、朋友家 CGNAT），人少（≤4），想直接打洞。三条路：

1. **WebRTC mesh**（每对参与者一条 PC，浏览器自己打洞）：前端要管 N 个 PC、按对协商，服务端只转发信令；
   是一套与 SFU 并列的第二媒体路径，前端复杂度最高，且带宽与下面第 2 条相同（发布者上传 N-1 份）。不推荐。
2. **SFU 跑在投屏者电脑上 + 全 ICE**：舞台内核本来就要出单文件形态（Windows 单文件版计划）。pion 从 ICE-Lite
   切到**全 ICE**（`SetLite(false)` + STUN 服务器）后，投屏者电脑即使在 NAT 后也能被观众打洞连上；
   带宽上与 mesh 等价（一份上传 × 观众数），但架构不变、前端零改动、观众零安装。需要的配套：
   - hearth 所在公网服务器跑一个 **STUN 应答**（`pion/turn` 的 STUN-only 模式，几乎不占带宽）；
   - Ember 信令加 **trickle**（`candidate` 消息类型），因为全 ICE 的 gathering 不再瞬时完成（已知坑：部分环境
     永不 complete，客户端最多等 1 秒）；
   - 对称 NAT 两端相遇时打洞失败——这是所有方案的共同上限，此时只剩中继。
   **推荐**。作为第 1 段之后的 1.5 段，工作量约 3–4 天。
3. **TURN 中继兜底**：只有当打洞失败时才需要。视频经公网小服务器中继带宽不允许；**语音可以**（64k × 人数）。
   因此 TURN 仅为语音线兜底、跑在 hearth 所在服务器，作为可选项排在最后，不阻塞任何段。

覆盖网络（如 EasyTier）是另一条路：把打洞交给 VPN 层，SFU 模型完全不变，但每个观众都要装客户端；
适合固定的一群人，作为文档里的可选部署方式，不进内核。

## 与其他计划的关系

- `plan-portmap.md`（已实现，取代已删除的 `plan-bellows-upnp.md`）：自动端口映射、向上游级联、v6 pinhole 与
  宣告收归 `lite` 均已上线；远端舞台进程（`cmd/ember`）沿用同一套 `portmap.Mapper` 与 `lite.Announcer`，
  接线方式照抄 `cmd/bellows`。TLS 由 hearth 反代承担，远端不需要证书。
- `plan-store-bun.md`：无交集，可并行。
- 注册制：新实例类型 `ember-remote`、内建 `ember` 补 stage 能力，走现有 `providerTypeFields`/`instantiateProvider`
  两个扩展点。

## 风险与取舍

- `pkg/sfu` 不是对外承诺稳定的 API：钉死 `v1.13.6`，升级视为一次有意的迁移；`livekit/protocol` 等类型在第 3 步前
  留在依赖树里是**有意的过渡**，不要在胶水层提前做类型替换，避免做两遍。
- SVC 重写是最大的技术风险：若第 2 段推进不顺，保底方案是 simulcast（投屏端多编码流、服务端整流切换，
  无需包级重写）——代价是投屏端 CPU，对硬编场景可接受。第 1 段不依赖此决策。
- HEVC 在浏览器侧的可用性（Chrome 已默认开、Safari 平台硬编）是既有事实，服务端直通不引入新约束。
- 远端进程的公网可达仍需要端口放行（与今天 LiveKit 的端口一样，复用即可）；UPnP 只是省手动步骤。
- 语音线与舞台线同为 ember 但分属两个进程，禁言/踢出要对两个实例都执行——`setGag`/`evict` 已按双线各调一次，
  不需要改。
