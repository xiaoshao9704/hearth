# 计划：内核收敛（Ember / Bellows / livekit-ingress 退场，进程内 LiveKit 成为唯一内核）

状态：**六步全部实施完成（2026-09-04）**，发布说明在 `docs/release-notes-v0.9.0.md`；待办：第六节两项实测（弱网语音 A/B、常驻内存）回填发布说明、`v0.9.0` tag（由维护者打）。下一个大版本（v0.9.0）的第一步；`plan-onebox.md` 与 `plan-roles-guests.md` 在它之后。
取代 `plan-stage-kernel.md`（路线 A，Ember 补视频）与 `plan-pionwhip.md`/`plan-bellows-grant.md` 的后续；三份文档保留作历史，状态行改为「已退役」。

## 动机与边界

`plan-livekit-embed.md` 路线 B 落地后，进程内补丁版 LiveKit（`lkembed`）已经覆盖了自研内核的全部职责：语音、投屏、
OBS 推流（自带 `/whip/v1`，HEVC/AV1 直通已真机验证），且同样接在 `portmap` + `lite.Announcer` 的打洞与宣告基建上。
剩下的 Ember（语音）、Bellows（推流网关）、livekit-ingress 适配是三条平行的第二实现，每条都要维护入场路径、配置键、
端口映射、前端引擎与测试。收敛之后：**一个内核类型、两种形态（进程内 / 远端 `cmd/stage`）、外加官方 LiveKit 作为可注册的外部实例。**

保留的：
- `rtc.Provider` / `rtc.StageProvider` 中性抽象与注册制（实例 alias 选择器）。抽象不因只剩一个实现而拆掉——
  远端 `cmd/stage` 与官方 LiveKit 都是同一类型的不同实例，注册制正是为它们服务的。
- 双线插槽（`voice_provider` / `stage_provider`）。两者同选 `lkembed` 即单连接（`combined`），这是新默认；
  舞台线换成远端实例仍是「语音留在进程内、投屏去上行充足的机器」的物理隔离形态，`plan-livekit-embed.md` 第 6 步已交付。
- `lite.Announcer`/`candidate`（宣告与 srflx 候选）、`portmap`、`lktoken`/`lkroom`。

退场的：
- `rtc/ember`（语音 SFU）与 ember 信令入口 `/providers/ember/voice`、一次性入场票（`admission.go` 的 voiceTicket）。
- `rtc/bellows`、`cmd/bellows`、`bellows-remote` 实例类型、`rtc.Publisher`/`KeyframeRelay`/`PublishLost` 这套「把轨发进舞台内核」的桥接约定。
- `livekit-ingress` 实例类型、`lkingress` 包、`livekitrtc/ingress.go`、`rtc.IngestProvider` 的 `EnsureEndpoint/BindRoom/DeleteEndpoint` 三个端点方法与 `ingest_endpoints` 表的读写代码（表本身留一个版本，下个版本迁移删表）。
- `lite.Transport`（pion UDP mux 与 MediaEngine，只有 ember/bellows 用）。
- 前端 `engine/ember.ts`。
- 发布产物 `bellows-linux-*` 与镜像过渡别名 `-livekit`/`-full`（上一版已声明只保留一个版本）。

不做的：
- 不动 `rtc.Identity`/`rtc.Meta`/`rtc.MatchesUser`、`admitUser`、禁言=禁全部发布的契约。
- 不动补丁版 LiveKit 本身（`livekitembed` 与 fork 的补丁集）。
- 不做 TURN、不做 ICE-TCP 默认开启（`cmd/stage` 的 `STAGE_TCP_PORT` 保持默认关）。

## 一、目标形态

| 槽位 | 可选实例 | 默认 |
|---|---|---|
| `voice_provider` | 内建 `lkembed`；已注册的 `livekit` 实例（远端 `cmd/stage` 或官方 LiveKit） | `lkembed` |
| `stage_provider` | `none`；内建 `lkembed`；已注册的 `livekit` 实例 | `lkembed` |
| 推流入口 | **不再是独立选择器**：OBS 的 WHIP 一律进当前舞台实例自带的 `/whip/v1`（`/providers/{alias}/w/{channel}` 路径不变，alias 必须是当前舞台实例，否则 404） | — |

实例类型只剩两个：`livekit-embedded`（内建、alias 固定 `lkembed`，不接受 DB 注册）与 `livekit`（DB 注册或 `LIVEKIT_API_URL` 合成锁定实例）。
端口映射的 wants 收成两条：HTTP 与 `lkembed_udp_port`（StrictPort，理由见 `dyncfg.go` 现有注释）。

去掉推流选择器的理由：剩下的每种实例都自带 WHIP 入口，「推流进哪个实例」与「舞台在哪个实例」不可能不同——推进别的实例观众看不到。
保留一个只有一个合理值的选择器只会制造错配。

## 二、改动清单（按包）

### server/internal/rtc

- `rtc.go`：删 `Publisher`、`WithKeyframeRelay`/`KeyframeRelay`、`WithPublishLost`/`PublishLost`；`IngestProvider` 收成只剩 `ServeWHIP`（或按现状叫法）一个方法；接口注释同步。
- 删目录 `ember/`、`bellows/`；`lite/` 删 `transport.go` 及其测试，保留 `announcer.go`、`candidate.go`、`lite.go` 里 Announcer 用到的部分。
- `livekitrtc/`：删 `ingress.go`、`publisher.go`；`whip.go` 保留（lkembed 与 livekit 实例共用的换票反代）。
- `livekitembed/`：不动。

### server/internal/api

- `api.go`：删 ember/bellows 构造与 `kernelKeys` 里两者的配置键；`joinToken` 删 `Engine == "ember"` 分支（凭证 `engine` 字段固定 `livekit`，字段保留给前端注册表）；`combined` 逻辑不变。
- `voice.go`：整文件删除；`admission.go`：删 voiceTicket 段，`admitIngest` 只剩「按 alias 反代到该实例 WHIP」一条路径。
- `ingest.go`：删 bellows 进程内直通与 bellows-remote 通行证反代、livekit-ingress 端点绑定；令牌重置/改标签不再删端点。
- `providers.go`：内建实例只剩 `lkembed`；类型表删 `livekit-ingress`/`bellows-remote`；env 合成只认 `LIVEKIT_API_URL`；`ingestInstance` 改为返回舞台实例。
- `dyncfg.go`：选择器删 `ingest_provider`，`voice_provider`/`stage_provider` 默认 `lkembed`，Hint 重写；`PortWants` 删 ember/bellows 两条；`warnLegacyConfig` 换成本版本的告警集（见第三节），`pion_*` 那段一并删除（已过一个版本）。
- `lkembed.go`：不动（已经同时承担 Stage 面与 WHIP 面）。
- 测试：`admin_providers_test.go` 的内建实例顺序断言、`ingest_test.go`、`proxy_test.go`、`selector_env_test.go` 按新实例表重写；删 ember/bellows 专属测试。

### server/internal/store

- `ingest.go` 里 `ingest_endpoints` 的读写方法删除；表留到下个版本再出迁移删表（`00003` 若被 `plan-roles-guests.md` 占用则顺延编号）。
- `ProviderRecord` 与 `providers` 表不动。

### server/cmd

- 删 `cmd/bellows`；`cmd/stage` 不动（它就是远端形态）；`cmd/server/main.go` 删 ember/bellows 相关注释与 wants。
- 删 `lkingress` 包。

### web

- 删 `engine/ember.ts`；`engine/index.ts` 注册表只剩 `livekit`（保留动态导入形态，`createEngine` 签名不变）。
- `room.tsx` 不需要改：两线/`combined` 逻辑、`onAudioBlocked`、进出提示都是引擎中立的。
- 管理后台：服务实例页去掉两个类型；「推流入口」选择器移除，推流 pane 的说明改为「推流进当前舞台内核」；`ingest` 令牌页不变（令牌与频道 URL 的模型不变）。

### 发布与文档

- `release.yml`：删 `bellows-linux-*` 编译、上传与 Release 资产三处；删 `meta-livekit-alias`/`meta-full-alias` 两段与 tags 拼接。
- `Dockerfile`/`Dockerfile.release`：`EXPOSE`/注释里去掉 47700/47710；README 快速开始改为 `-p 8080:8080 -p 47720:47720/udp` 一种形态，架构图与三张表按第一节重画；「Bellows 远端形态」章节删除，「远端舞台机器」章节保留。
- `CLAUDE.md` 架构铁律整段重写：删 ember/bellows/pion/ingress 的全部条目，加「唯一内核类型 + 两种形态」「推流入口 = 舞台实例」两条；「已知的坑」删 ember 锁与 ingress bypass 两条。
- `docs/plan-stage-kernel.md`、`plan-pionwhip.md`、`plan-bellows-grant.md`、`plan-aio-images.md` 状态行改「已退役（v0.9.0 内核收敛）」，正文不改。

## 三、迁移与兼容（只保留一个版本，比照 `pion_*` 先例）

api 层游标迁移 v3，启动时一次性执行：

1. `cfg_voice_provider` 为空/`ember`/`pion`/`bellows`/任何不存在的 alias → `lkembed`；`cfg_stage_provider` 为空 → `lkembed`，显式 `none` 保持 `none`，指向已删类型实例的 alias → `lkembed`。
2. `cfg_ingest_provider` 删除。
3. `providers` 表中 `type in (livekit-ingress, bellows-remote)` 的行删除，逐条打日志（alias、类型）。
4. `cfg_ember_*`、`cfg_bellows_*`、`cfg_pion_*` 全部删除。
5. `ingest_endpoints` 表内容清空（不删表）。

启动告警（`warnLegacyConfig` 本版本内容）：
- 环境变量 `EMBER_UDP_PORT`/`EMBER_PUBLIC_IP`/`EMBER_STUN_SERVERS`/`BELLOWS_*`/`INGRESS_UPSTREAM_URL`/`BELLOWS_REMOTE_URL`/`BELLOWS_SINK` 存在 → 各打一行「已不再读取，语音/推流已并入进程内 LiveKit（lkembed），请从部署侧删除」。
- 容器仍发布着 47700/47710 端口不会报错，只是空放行；README 提示可以收掉。

下个版本删除这组告警与 `ingest_endpoints` 表。

## 四、行为差异（要写进发布说明）

- **默认端口变化**：语音媒体从 `47700/udp` 变为 `47720/udp`（与投屏同一个端口）。旧部署升级后若只放行了 47700，语音会断——迁移日志与 README 都要醒目提示；`portmap` 自动映射的部署不受影响。
- **纯语音部署也跑 LiveKit**：进程内多一个 SFU 的常驻开销（内存约数十 MB 量级，具体数值在验收时实测写入发布说明），换来投屏默认可用。
- **OBS 推流 URL**：路径形态不变（`/providers/lkembed/w/{频道}`），但原来选 `bellows` 的部署 alias 段要从 `bellows` 改成 `lkembed`；令牌不变。
- **HEVC/AV1**：继续直通（LiveKit 自带 WHIP 对 offer 编码原样保留）。
- **远端 Bellows 用户**：迁移到 `cmd/stage`（`plan-livekit-embed.md` 第 6 步的部署说明），hearth 侧把舞台实例指向它即可。

## 五、分步与验收

按顺序做，每步 `go build ./... && go vet ./... && go test ./...`、`npx tsc --noEmit && npm run build` 通过再进下一步：

1. **默认值与迁移先行**（代码仍在）：选择器默认改 `lkembed`、游标 v3、告警集；验收：干净库启动后 `go run ./cmd/server` 直接能进房说话与投屏，不进管理后台改任何东西；旧库（`voice=ember, ingest=bellows`）启动后日志出现迁移行且选择器落成 `lkembed`。
2. **删推流选择器与 ingest 分支**：`admitIngest` 只剩 WHIP 反代；验收：OBS 走 `/providers/lkembed/w/{频道}` 推 HEVC 观众可见；`/providers/bellows/w/...` 404。
3. **删 ember**：`voice.go`、voiceTicket、`rtc/ember`、前端 `engine/ember.ts`；验收：`/providers/ember/voice` 404；前端 `livekit` chunk 仍是动态加载。
4. **删 bellows / ingress / lite.Transport / Publisher 约定**；验收：`grep -rn "bellows\|ember\|ingress" server web --include=*.go --include=*.ts --include=*.tsx` 只剩历史文档引用与 `livekitrtc/whip.go` 里的 LiveKit 自带 ingress 术语。
5. **CI / Docker / README / CLAUDE.md**；验收：`release.yml` 手动触发一次成功，产物里没有 bellows，镜像只有主 tag。
6. 发布说明按第四节写，打 `v0.9.0`。

## 六、风险

- **语音质量回归**：Ember 是纯音频、零拦截器的最小路径；LiveKit 有 NACK/RED/DTX 等更完整的丢包处理，理论上更好，但要在弱网下 A/B 一次（同一台机器切两个内核各听 10 分钟），结果写进发布说明。
- **CPU/内存基线抬高**：纯语音小机器（arm64 小主机）上实测常驻占用，若明显高于 Ember，把 LiveKit 的 `room.auto_create`、`rtc.use_ice_lite`、日志级别等无关功能再收一遍。
- **删代码的连带**：`rtc.go` 接口瘦身会波及 `cmd/stage` 与 `livekitrtc` 的编译，先改接口再删实现，靠编译器把引用清干净，不靠 grep。
- **测试基线**：`admin_providers_test` 等对内建实例顺序有硬断言，重写时以「lkembed 唯一内建」为准，不要为兼容旧断言留假实例。
