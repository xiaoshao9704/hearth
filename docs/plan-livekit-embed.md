# 计划：进程内嵌入 LiveKit（补丁式 fork，单二进制自打洞自宣告）

状态：**路线 B，主线，待实施。2026-09-03。** 取代 `plan-stage-kernel.md` 的路线 A 成为舞台线的主路线；路线 A 降为备选，
其文档保留。本计划自包含，实施会话读完即可开工，不需要本会话的其他上下文。

## 目标与判据

把舞台线（投屏/摄像头/OBS 推流）收进 hearth 自己的二进制，同时**完整保留 LiveKit 的 SFU、信令与前端 SDK**：

- **单二进制**：hearth 进程内拉起 LiveKit 服务端，没有第二个进程、没有 redis、没有 ingress、没有 aioinit 拉子进程。
  Windows 单文件同样成立（已验证能编译）。
- **自打洞**：LiveKit 的媒体端口进 `portmap.Mapper` 的 wants，v4 映射、向上游级联、v6 pinhole 全部自动。
- **自宣告不重启**：LiveKit 的候选宣告改为每建新 PC 时向 hearth 的 `lite.Announcer` 取当前外部地址，公网 IP 或映射变化
  只影响新会话，在途会话不动，watchdog 退役。
- **进程内推流**：Bellows 经现有 `rtc.Publisher`（`livekitrtc`）向回环的 LiveKit 发布，OBS 推流不出进程。
- **零重写**：不碰协商、订阅、层选择、拥塞控制、重连，也不碰 livekit-client。LiveKit 多年修掉的 bug 原样继承。

**不做**：改 LiveKit 的架构、把它的控制面换成 hearth 的（那是路线 A）、Bellows 与 SFU 的进程内直递（保留回环 PC，
同机代价可忽略；将来想要再在 fork 里加内部发布入口）。

判据（用户 2026-09-03 拍板）：「不担心做完成本，只担心跑完出现很多 LiveKit 早就解决的 bug」+「一个容器/一个文件自己解决所有问题」。
路线 A 只剩「一套前端引擎、控制面完全自有、Bellows 直递」三样，不是硬目标，因此让位。

## 已核实的事实（2026-09-03 spike，临时模块，直接采信）

| 事实 | 依据 |
| --- | --- |
| 整个 `livekit-server v1.13.6` 只删 `pkg/rtc/transport.go:387` 的 `se.EnableSped(true)` 一行，**对上游 pion 编译通过** | 临时模块 `replace` 到删行后的本地副本，`go build ./...`（import `pkg/service` + `pkg/config`）通过 |
| 三平台都过 | `GOOS=linux` 与 `GOOS=windows` 交叉编译均通过 |
| 解析到的 pion 全是上游 | `pion/webrtc/v4 v4.2.18`、`pion/ice/v4 v4.4.0`、`pion/dtls/v3 v3.1.5`，无任何分叉；与 hearth 的 4.2.19 / 4.4.0 / 3.1.5 兼容 |
| Go 版本 | livekit-server 要 go 1.26，hearth 是 go 1.27 |
| `livekit/protocol`、`server-sdk-go` | hearth **已经**依赖（`livekitrtc` 用），嵌入只新增 `livekit-server` 本身 |
| `EnableSped` 是 LiveKit pion 分叉私有的「warp」建连加速开关，**不是**发送平滑 | 见下节「分叉 diff 清单」；在 livekit-server 里它受 `TransportParams.EnableWarp` 门控（`transport.go:386-389`，同一守卫内还有上游已有的 `EnableSctpZeroChecksum`/`EnableSctpSnap`），默认不开 |
| 嵌入的规范写法 | 就是 `cmd/server/main.go` 与 `test/integration_helpers.go` 的三步：`config.NewConfig(yaml, true, nil, nil)`（`c` 可为 nil，从 `DefaultConfig` 起叠加 YAML 字符串）→ `routing.NewLocalNode(conf)` → `service.InitializeServer(conf, node)` → `Start()` / `Stop(force)` |
| 无 redis | `pkg/service/wire.go:183`：`!conf.Redis.IsConfigured()` 即走本地 router |
| HTTP/信令 | `Start()` 自建 `http.NewServeMux()`，注册 Room/Egress/Ingress/SIP/AgentDispatch 的 twirp 与 `rtcService.SetupRoutes(mux)`，监听 `conf.Port`；`RTCService.SetupRoutes` 是导出的。**本计划让它监听回环、hearth 反代**，不挂外部 mux（`Start()` 是整体的，挂外部 mux 省不掉它自己的监听） |
| 候选宣告的「钉死」在哪 | `mediatransportutil/pkg/rtcconfig.NewWebRTCConfig`：启动时只在 `node_ip` 非空且（`use_external_ip` 或非自动生成）时把 NAT1To1 规则烧进 `SettingEngine`；两者都不设（DefaultConfig：`UseExternalIP=false`、`STUNServers=[]`）则**没有任何启动规则**。每建一个 transport 时 `transport.go:357` 做 `se := params.Config.SettingEngine`（**值拷贝，本来就是每 PC 一份**）。`transport.go:427-446` 那段只是给不支持 prflx-over-relay 的老客户端的特例，读的是启动时的 `NAT1To1IPs`（我们为空，跳过） |
| `SetNAT1To1AddressRewriteRules(se, ips, includeInternal)` 的 bool | `includeInternal=true` → pion `ICEAddressRewriteAppend`（保留 host 候选、追加外部地址为 host 型候选）；false → 替换。**我们用 true**。`ips` 可以是裸 IP（catch-all）或 `外部/本机` 对 |

## 分叉 diff 清单（2026-09-03，逐文件对比，直接采信）

livekit-server 用 `replace` 把三个 pion 模块换成自家 `-warp.1` 分叉，基线版本精确：
`webrtc/v4 v4.2.18`、`ice/v4 v4.4.0`、`dtls/v3 v3.1.5`，与 hearth 当前使用的版本一致或相邻。三个分叉对基线的非测试改动合计约 465 行：

| 分叉 | 改动 | 内容 |
| --- | --- | --- |
| `livekit/webrtc-pion` | 4 个文件，约 105 行 | `ICETransport.SetDtlsCallback / Piggyback / ReportDtlsPacket`、`SettingEngine.EnableSped`：让 DTLS 握手包捎在 ICE 连通性检查里发，省建连一个往返（这就是 warp） |
| `livekit/ice` | 3 个文件（新增 `piggyback.go`），约 74 行 | ICE agent 侧的捎带发送与接收 |
| `livekit/dtls` | 5 个文件，约 283 行 | `InjectInboundPacket`、`WithOutboundHandshakePacketInterceptor`、`WithInboundHandshakePacketNotifier`：握手包拦截与注入的管道 |

除 warp 管道外只有两处行为差异，用上游 pion 时会回到上游语义：

1. `webrtc-pion` 把 DTLS 的 `WithInsecureSkipVerify` 硬编码为 true（上游按 SettingEngine 配置，默认值也是 true）——LiveKit 用法下无实际差别。
2. `livekit/ice` **删掉了** 上游在 `ConnectionStateFailed` 时清空 checklist / pairs / pendingBindingRequests 的那段状态重置。
   用上游即恢复该重置，这是所有非 LiveKit 的 pion 使用者的标准行为，判断为低风险；但它是唯一的实质性语义差异，第 4 步验收要加一条
   「ICE 失败后重连正常」。

**结论：丢掉分叉不丢任何 bug 修复，只丢 warp 建连加速（本来就默认关）和一处 LiveKit 自己偏离上游的失败态处理。**
这就是「跑完之后出现 LiveKit 早就解决的 bug」这条担心在 pion 层的答案：没有这样的 bug 藏在分叉里。

## 架构

```
浏览器 ──舞台信令 /providers/{alias}/rtc/*（hearth 反代）──▶ 回环 127.0.0.1:<lkembed_port> 的进程内 LiveKit
                                    ▲ 媒体直连：LAN host + 映射/STUN 外部地址（LiveKit 按补丁二从 lite.Announcer 取）
OBS ──WHIP /providers/{alias}/w──▶ 进程内 Bellows ──现有 livekitrtc Publisher（回环 PC）──▶ 同一个进程内 LiveKit
portmap.Mapper：把 lkembed 的 UDP（与可选 TCP）端口映射到网关；v6 pinhole 同步；OnChange 触发宣告刷新
```

- **注册制**：新增内建实例类型 `livekit-embedded`，alias `lkembed`（不能用 `livekit`，那是 `LIVEKIT_API_URL` 合成的 env 锁定实例）。
  实例对象**复用现有 `livekitrtc.New(cfg)`**：给它一个 cfg getter，`livekit_api_url` 固定为回环地址、`livekit_api_key/secret`
  取自 `lkembed_*` 全局键。这样舞台槽位、令牌签发、`/rtc/*` 反代、Bellows 的 Publisher 全部零改动。
- **语音线不变**：仍是 Ember。两条线分属两个实例，前端仍是双连接（`combined` 判定不变）。
- **远端形态**：家里局域网机器跑 `cmd/stage` = 同一个嵌入包 + Bellows + Mapper + 周期刷新，替代今天树莓派上 livekit 与 bellows
  两个容器。hearth 侧继续用 env 锁定的 `livekit` 实例指向它的 API 地址（tailscale），推流入口用 `bellows-remote`——**hearth 侧接线与今天完全一样**。
  LiveKit 的信令票据就是 hearth 签的 JWT，不需要 grant。
- **回退**：外部 LiveKit 实例（`livekit` env 锁定 / DB 注册）继续可选，选择器一切即回退。

## Fork：补丁序列，不改形

- 仓库：fork `livekit/livekit-server`，分支 `hearth-patches`，基于 tag `v1.13.6`，**保持模块路径不变**（fork 天然如此，`replace`
  要求目标 go.mod 的 module 与原路径一致）。每次跟上游只做 `rebase` 到新 tag，补丁不超过两个文件。
- hearth `server/go.mod`：`require github.com/livekit/livekit-server v1.13.6` +
  `replace github.com/livekit/livekit-server => github.com/<fork>/livekit-server <fork 上的 tag，如 v1.13.6-hearth.1>`。
- **补丁 1（一行）**：删 `pkg/rtc/transport.go:387` 的 `se.EnableSped(true)`，守卫 `if params.EnableWarp { ... }` 与其中的
  `EnableSctpSnap` 保留（上游 pion 有）。配置里 warp 保持默认关；即便有人打开，也只是少了 sped 这一半，不报错。
- **补丁 2（21 行，已写好并验证）**：外部地址来源改为可注入回调。
  - `config.RTCConfig` 加 `ExternalIPs func() []string \`yaml:"-"\``（宿主在 `config.NewConfig` 之后、`InitializeServer` 之前赋值）
    → `rtc.NewWebRTCConfig` 透传到 `rtc.WebRTCConfig.ExternalIPs`
    → `transport.go` 在 `se := params.Config.SettingEngine` 拷贝之后：回调非空且返回非空时 `SetNAT1To1AddressRewriteRules(&se, ips, true)`。
    `SetICEAddressRewriteRules` 是整体替换，所以它覆盖启动规则；`se` 是每 PC 一份拷贝，所以**天然就是「每建新 PC 取一次当前值」**，
    不需要缓存失效机制，也不影响其他 transport。
  - bool 已确认为 `includeInternal`，用 `true`：保留本机 host 候选（LAN 直连）并追加外部地址（公网观众）。不需要改成 srflx；
    LiveKit 一贯用 host 型追加（`advertise_internal_ip` 语义），与 hearth 在 P1 之前的做法相同。
  - hearth 侧 YAML：`rtc.use_external_ip: false`、不设 `node_ip`、`rtc.stun_servers: []`（DefaultConfig 本就如此，显式写上防上游改默认）——
    探测与打洞交给 hearth，避免 LiveKit 自己去 gather STUN（国内 Google STUN 不可达会拖慢每个 PC）。
  - **两个补丁的权威副本在 `server/livekit-patches/`**（`git format-patch` 产物 + 重建 fork 的步骤），fork 上的 tag 为 `v1.13.6-hearth.1`。
    已验证：贴上两个补丁的整个服务端对上游 pion 在 darwin / linux / windows 编译通过。
- 许可证：Apache-2.0，fork 与嵌入合法，保留 LICENSE/NOTICE。

## hearth 侧接线

新包 `server/internal/rtc/livekitembed`（进程内拉起/停止 + 配置拼装，**不实现任何 rtc 接口**，接口全部由 `livekitrtc` 承担）：

```go
type Options struct {
    HTTPPort   int              // 回环监听端口（lkembed_port）
    UDPPort    int              // 媒体 UDP 单端口（lkembed_udp_port）
    TCPPort    int              // ICE-TCP 端口，0 = 关（lkembed_tcp_port）
    APIKey, APISecret string    // lkembed_api_key / lkembed_api_secret
    ExternalIPs func() []string // 补丁二的回调：来自 lite.Announcer（映射外部 IP 优先，其次 STUN 公网 IP）
    LogSink    func(string, ...any)
}
func Start(ctx context.Context, o Options) (*Server, error) // Start 失败原样返回（端口占用等），不阻塞 hearth 启动
func (s *Server) Stop()                                       // service.LivekitServer.Stop(true)
```

- 配置拼装：`config.NewConfig(yaml, true, nil, nil)`，YAML 只放我们关心的键：`port`、`bind_addresses: ["127.0.0.1"]`、`keys`、
  `rtc.udp_port`、`rtc.tcp_port`、`rtc.use_external_ip: false`、`room.auto_create: true`、`logging.level`，其余用 `DefaultConfig`。
  `redis` 不配即本地 router。把 `webRTCConfig.ExternalIPs` 回调塞进去的位置按补丁二定（可能要在 `InitializeServer` 前后各挂一次，
  以 fork 里的实际结构为准）。
- **全局配置键**（走 `rtc.ConfigKey`，Group 用管理后台已有的分组，与 `ember_*` 同列）：
  `lkembed_port`（默认 47730，回环）、`lkembed_udp_port`（默认 47720）、`lkembed_tcp_port`（默认 0）、
  `lkembed_api_key`/`lkembed_api_secret`（`Secret: true`；为空时首次启动自动生成并落库 settings，之后不变——与 aio 的 `keys.env` 同一思路，
  但持久化在 DB 里，用户挂卷即备份）。端口改动重启生效（与 ember 一致）。
- **注册表**：`providers.go` 内建列表加 `{Alias: "lkembed", Type: "livekit-embedded", Builtin: true, Stage: livekitrtc.New(embedCfg)}`；
  `embedCfg` 把 `livekit_api_url` 映射为 `http://127.0.0.1:<lkembed_port>`、`livekit_api_key/secret` 映射到 `lkembed_*`。
  选择器 `stage_provider` 的合法性校验按「实例存在 + 能力匹配」照旧通过。**只有 `stage_provider == lkembed` 时才 `Start`**，
  否则不起进程内 LiveKit（纯语音部署零额外开销）；选择器切换到/离开 `lkembed` 时启动/停止（先做「改了重启生效」也可，后续再热切）。
- **宣告回调**：`ExternalIPs` 返回 `announcer.Snapshot()` 里的外部 **IP**（映射结果取 IP 部分排最前，其次 STUN 公网 IP，去重）。
  这里复用 Ember 那一个 `Announcer` 即可（同一台机器公网地址只有一个），不新建第二个探测器。
- **端口映射**：`api.PortWants` 在 `stage_provider == lkembed` 时追加 `{Proto: "udp", Port: lkembed_udp_port, Desc: "hearth stage",
  StrictPort: true}`（TCP 端口开着时同样追加）。**`StrictPort` 必须为 true**：LiveKit 的地址改写只改 IP 不改端口（与 pion 同源限制），
  网关若改派了外部端口，宣告出去的端口就是错的，宁可判定失败给 `port_conflict` 诊断也不要假成功。v6 pinhole 随 wants 自动覆盖。
  `Mapper.OnChange` 已接 `a.RefreshAnnounce`，回调读的是刷新后的快照，无需额外接线。
- **Bellows**：`stagePublisherSink` 取到 `lkembed` 实例的 Publisher（`livekitrtc` 已实现）即向回环 LiveKit 发布，零改动。
- **`cmd/stage`**（远端形态，替代树莓派上的两个容器）：`livekitembed.Start` + `bellows.NewRemote` + `portmap.New` + 周期刷新，
  接线逐行照抄 `cmd/bellows/main.go`（Mapper、OnChange、ticker、优雅退出 `Close(新 ctx)`），环境变量 `STAGE_*` 对应上面五个键，
  `PORTMAP_MODE` 同名沿用。LiveKit 在这里**监听非回环地址**（hearth 经 tailscale 访问其 API），`bind_addresses` 由 env 给。
  `cmd/bellows` 保留一段时间后并入 `cmd/stage`。

## 实施顺序（每步单独可验）

1. **fork 与依赖**：建 fork 分支与两个补丁，打 tag；hearth `go.mod` 加 `require` + `replace`；`cd server && go build ./... && go vet ./...`
   与 `GOOS=linux`/`GOOS=windows` 交叉编译通过（spike 已证明，这里只是接线）。`go mod tidy` 后确认 pion 仍是上游版本、无 `-warp` 类分叉。
2. **`livekitembed` 包**：`Start`/`Stop` 与配置拼装；单测：回环起一个实例，用 `server-sdk-go` 进房发一条音轨、另一个客户端订阅收到 RTP；
   `Stop` 后端口释放。**不接注册表也能跑**，先把这层验干净。
3. **注册表与配置键**：`lkembed` 内建实例、五个全局键、`stage_provider` 选中即启动；管理后台能看到并能选；本地 `go run ./cmd/server`
   选 `lkembed` 后浏览器进房投屏可见（这一步不需要路由器）。前端 `npx tsc --noEmit && npm run build` 若动了分组标签。
4. **宣告与打洞**：补丁二回调接 `Announcer`；`PortWants` 加 StrictPort 的 UDP want。真机验收（**环境信息不入库**）：
   网关租约表出现舞台 UDP 端口的同端口映射（v4）与 pinhole（v6）；LiveKit 给浏览器的候选里出现映射外部 IP 的 srflx，
   **且 LAN host 候选仍在**（这条验补丁二的 bool 语义）；公网 IP 变化模拟（改探测返回值）后**新**会话拿到新地址、进程不重启、在途会话不断。
5. **推流**：OBS/ffmpeg（见既有 WHIP 验收配方）经进程内 Bellows 推 HEVC，观众可见，`{user}-obs` 入名册，禁言后消失。
6. **`cmd/stage`** 远端形态；树莓派 compose 从 livekit + bellows 两个服务换成一个 `stage`（备份旧 compose）；bj 侧不动。
7. **收尾**：aio 的 `EMBED_LIVEKIT` 路径退役（`aioinit` 不再拉 livekit/redis）；README 架构图与部署段；CLAUDE.md 更新
   （内建实例多一个 `lkembed`、aio 不再拉子进程、`livekit_*` 命名空间说明）；`plan-stage-kernel.md` 状态行指向本计划。

第 1 到 3 步可以在一个 worktree 里连续做，第 4 步起需要真机。

## 验收总表

- `cd server && go build ./... && go vet ./... && go test ./...`；`GOOS=linux`/`GOOS=windows` 交叉编译。
- 纯语音部署（`stage_provider=none`）：启动无任何 LiveKit 相关日志与端口，行为与今天完全一致。
- `stage_provider=lkembed`：单进程内投屏/摄像头/OBS 三路都通；无 redis、无 ingress、无 watchdog。
- 打洞与宣告：见第 4 步；`PORTMAP_MODE=off` 时无映射且 LiveKit 只宣告 host + STUN 公网 IP。
- 回退：选择器切回外部 `livekit` 实例，前端零改动即恢复。

## 风险与取舍

- **依赖树膨胀**：`livekit-server` 的传递依赖（psrpc、grpc、otel、prometheus、twirp 等）全部进 go.mod，二进制大几十 MB、构建变慢。
  hearth 自己的代码仍只依赖中性 `rtc` 接口，脏只脏在 `livekitembed` 包、`providers.go` 一行和 go.mod。接受。
- **两套控制面**：hearth 的入场判定/禁言/封禁经现有 `livekitrtc` 桥接到 LiveKit 的 JWT 与 RoomService，今天远端形态就是这么跑的，
  只是搬进同一进程，没有功能损失，也没有新代码。
- **fork 维护**：两个补丁、两个文件，按上游 tag rebase；升级是一次有意的动作（同时升 `require`/`replace`），不追 edge。
  升级后重跑第 1、4 步验收。
- **补丁二的 bool 语义**是本计划唯一的未知数（见实施检查点），第 4 步的「LAN host 候选仍在」专门验它。
- **弃用分叉的代价已量化**（见「分叉 diff 清单」）：只失去 warp 建连加速（默认关）与一处 ICE 失败态重置的差异，无 bug 修复损失。
  升级 LiveKit 版本时重跑一次同样的 diff，确认分叉没有长出新的非 warp 改动。
- **Windows**：只验证了编译，运行时（UDP mux、防火墙弹窗）第一次跑 `cmd/stage` 时验；`portmap` 的 `host_firewall` 诊断仍是预留。
- **端口改动需重启**：与 ember 一致；热切选择器可后置。
- **性能**：Bellows 到 LiveKit 多一条回环 PC（一次 DTLS/SRTP），同机可忽略；真要省，将来在 fork 里加内部发布入口，不在本计划内。

## 与路线 A 的关系

路线 A（`plan-stage-kernel.md`：模块引用 `pkg/sfu`、自写房间与前端视频）降为备选，文档保留不删。
两条路解决同一组功能问题，分歧只在形态：A 换来一套前端引擎与完全自有的控制面，代价是重写协商、订阅、前端并承担 bug 尾巴；
B 放弃这三样，换来零重写与 LiveKit 的全部成熟度。若将来「一套引擎」成为硬目标，A 的起手 spike（Bellows 轨直递 `sfu.Receiver`）仍可复用。
