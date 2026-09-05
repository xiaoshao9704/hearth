# 计划：浏览器 ICE 配置由服务端下发，连通性三层兜底

状态：**设计定稿（2026-09-05），待实施。** 第一阶段（STUN 并列 + 服务端下发 + 补丁四 + 埋点收编）为本期范围；第二阶段（TURN over TLS 443）设计已定、实施待排。
本计划自包含，实施会话读完即可开工，不需要本会话的其他上下文。

## 背景与判据

2026-09-05 的语音故障排查得到三条硬结论，本计划直接建立在它们之上：

1. 在做策略路由/分流的家庭网络里，浏览器到云服务器媒体端口的 **UDP 会被中间设备接管、回程不通**；蜂窝网络 UDP 直连正常。同一台设备换网络即换路径。
2. 最终让语音可用的是 **ICE-TCP 回落**（云侧安全组同时放行媒体端口的 udp 与 tcp）。桌面 Chrome 实测 `selected_server tcp/host <公网IP>:47700`、561 ms 建连；蜂窝 `udp` 322 ms。
3. 浏览器侧 **STUN 不是必需品**：客户端 `iceServers: []` 的对照版本在两种网络下都能通。srflx 候选在 hearth 这种 client-server 结构里不提供额外可达性——客户端永远是主动方，服务端从收到的包按 peer-reflexive 学到客户端地址。

同时暴露了两个设计缺陷：

- 前端把 STUN **硬编码**在 `web/src/engine/livekit.ts`（`rtcConfig: { iceServers: [stun.miwifi.com] }`），部署者改不了。任何单一 STUN 都是地域性的：国内可达的对海外玩家超时，反之亦然。
- 之所以要硬编码，是因为 LiveKit 服务端下发 `ice_servers` 时（fork `pkg/service/roommanager.go` 的 `iceServersForParticipant`）**没有任何 STUN/TURN 配置就回落到内置的 google/twilio 默认列表**，"显式不要 STUN"在服务端无法表达。

判据：**连通性不依赖 STUN 成功与否**。STUN 从"必须选对"降级为"选错也无所谓"；兜底靠 ICE-TCP（已有）与 TURN/TLS 443（第二阶段）。

## 已核实的事实

| 事实 | 依据 |
| --- | --- |
| 服务端下发顺序：内置 TURN（`turn.enabled`）→ 外部 `turn_servers` → `stun_servers` → 三者皆无时 `DefaultStunServers` | fork `pkg/service/roommanager.go` `iceServersForParticipant`，末尾 `if !hasSTUN { append(DefaultStunServers) }` |
| `stun_servers` 在服务端**一键两用**：既下发给客户端，也是服务端自己 srflx 探测的来源；hearth 现在写 `stun_servers: []` 并靠显式 `node_ip: 127.0.0.1` 跳过服务端探测（v0.9.1） | `livekitembed.buildYAML` 及其注释 |
| 内置 TURN 的 turns URL 端口**写死 443**：`turns:%s:443?transport=tcp`，域名取 `turn.domain`；`external_tls: true` 时 LiveKit 在 `tls_port` 上收**明文** TURN/TCP，由外层终止 TLS | 同文件 TURN 分支；`pkg/config/config.go` `TURNConfig.ExternalTLS` |
| 浏览器对多个 STUN 并行探测、先返回者先成候选；STUN 超时不阻塞 trickle ICE | WebRTC 标准行为；本次对照实验（零 STUN 亦可建连）反证 |
| 前端诊断链路：`engine.onDiagnostic` → `room.tsx` `diag()` → `api.reportClientLog` → `POST /api/client-log` → 服务端 `log.Printf("前端诊断: ...")`；服务端已有鉴权、120 条/分钟/用户限流、8 KB body 上限、token/URL 脱敏与两个测试 | `server/internal/api/clientlog.go`、`clientlog_test.go`、`web/src/api.ts:247` |
| 引擎侧 ICE 采集现为 **400 ms 轮询 `getStats`**，每个 local/remote candidate、每个 candidate-pair 各上报一条（`iceProbeSeen` 去重）；`state` 字段服务端截断 80 字符 | `web/src/engine/livekit.ts` `startIceProbe/reportTransportStats`；`clientlog.go` `redactClientLogText(in.State, 80)` |
| iOS/WebKit 的 RTCStats 隐藏候选地址字段（日志里全是 `unknown`），桌面 Chrome 完整 | 本次真机日志对比 |
| 截至 2026-09-05，前端 ~98 行诊断代码与 `clientlog.go`/`clientlog_test.go` **均未提交** | `git status` |

## 设计：三层

### 第一层：STUN 多地域并列，服务端可配，前端不写死（本期）

**语义**

- 新增管理后台键 `lkembed_client_stun_servers`（Group `stage`，Env `LKEMBED_CLIENT_STUN_SERVERS`）：逗号分隔，**发给浏览器**的 STUN 列表。与既有 `lkembed_stun_servers`（服务端自己探测公网映射用）严格分开——两者语义不同，不得复用一个键。
- 默认值 `stun.miwifi.com:3478,stun.l.google.com:19302`：国内/海外各有一个能命中的；浏览器并行探测、谁先回用谁。
- 显式 `none` = 不下发任何 STUN（对照实验/纯内网部署用）。沿用仓库里选择器 `none` 的惯例。
- 留空 = 默认值。改动重启生效（与 `lkembed_log_level` 同款 Hint）。
- `cmd/stage` 对应 `STAGE_CLIENT_STUN_SERVERS`，默认同一常量。

**补丁四（fork `v1.13.6-hearth.4`）：显式客户端 STUN 列表，不回落默认**

编号说明：fork 的 `hearth` 分支上此前已有一个未收进补丁目录的提交（darwin 无 cgo 构建退化 + protocol replace），本期一并补成 0003 让 `server/livekit-patches/` 重新成为权威副本；`v1.13.6-hearth.3` 这个 tag 名已被一个废弃实验（补丁二 host 候选翻成 external-only）占用，不采用、不删除，新补丁顺延为 0004 / `hearth.4`。

`pkg/config/config.go` 的 `RTCConfig` 加非 YAML 字段（与补丁二的 `ExternalIPs` 同款风格）：

```go
// hearth patch 4: explicit STUN list to advertise to clients. nil keeps upstream behaviour
// (fall back to DefaultStunServers when nothing else is configured); non-nil is authoritative,
// and an empty slice means "advertise no STUN at all". Not a YAML field.
ClientSTUNServers *[]string `yaml:"-"`
```

`pkg/service/roommanager.go` `iceServersForParticipant`，放在 `if len(rtcConf.STUNServers) > 0` 之前：

```go
if cs := rtcConf.ClientSTUNServers; cs != nil {
	if len(*cs) > 0 {
		iceServers = append(iceServers, iceServerForStunServers(*cs))
	}
	return iceServers // explicit: never fall back to DefaultStunServers
}
```

TURN 分支在其之前已 `append`，不受影响——第二阶段的 TURN 与本字段正交。约 10 行。补丁文件 `server/livekit-patches/0004-hearth-patch-4-client-stun-servers-explicit.patch`（另有 `0003-*-telemetry-darwin-nocgo-protocol-fork.patch` 补齐既有提交），README 表格加两行，fork 打 `v1.13.6-hearth.4`，`server/go.mod` 的 `replace` 跟进。

**hearth 侧接线**

- `livekitembed.Options` 加 `ClientSTUNServers string`（原样逗号分隔；`none` → 显式空）；导出常量 `DefaultClientSTUNServers`。`Start` 里在 `conf.RTC.ExternalIPs = o.ExternalIPs` 同处挂 `conf.RTC.ClientSTUNServers`（必须在 `NewLocalNode`/`InitializeServer` 之前，理由同补丁二）。`buildYAML` 的 `stun_servers: []` 保持不动——服务端自己的探测仍由 `node_ip` 绕过。
- `server/internal/api/lkembed.go` `EnsureStageKernel` 传 `a.dynVal(ctx, "lkembed_client_stun_servers")`；键声明加在 `livekitembed.go` 的 dyn 键表里、紧邻 `lkembed_stun_servers`，Hint 写清"发给浏览器的列表，与上一项探测用途不同；`none` = 不下发"。
- `cmd/stage/main.go` 加 `STAGE_CLIENT_STUN_SERVERS`，头部 env 注释同步。

**前端**

`web/src/engine/livekit.ts` 的 `room.connect(url, token, { rtcConfig: ... })` 改回 `room.connect(url, token)`，删掉硬编码与"诊断期间覆盖"注释。ICE 服务器完全回归服务端 `JoinResponse.ice_servers`。

### 第二层：ICE-TCP（已有，本期只补文档）

`lkembed_tcp_port` / `STAGE_TCP_PORT` 已实现。本期把 `lkembed_tcp_port` 的 Hint 从"UDP 全被封的网络里才需要"改为反映实测：**家庭网络做策略路由/分流时 UDP 常被接管，建议与 UDP 同号开启，云侧安全组 udp/tcp 双放行**。README 部署段同步一句。

### 第三层：TURN over TLS 443（设计定稿，实施待排）

**为什么在 hearth 里几乎免费**：SFU 架构下服务器就是媒体终点，TURN relay 到"自己"不产生额外带宽与机器。TURN 为浏览器分配的 relay 地址与 SFU 的 ICE agent 在同一进程，relay 流量走本机回环，**对外只需 443 一个口**，`relay_range` 不必开放。relay 候选优先级最低，只在 UDP 与 ICE-TCP 都失败时被选中，不影响正常路径延迟。

**反代形态（当前线上：hearth 跑在 nginx 后面、nginx 管 TLS）——能做，且 hearth 零 TLS 职责**：

- TURN 用独立子域（如 `turn.example.com`），证书由 nginx 现有的泛域名/SAN 证书覆盖。
- nginx `stream` 块监听 443，`ssl_preread` 按 **SNI** 分流原始 TLS 流：主域名 → nginx `http` 块的内部 TLS 监听（如 `127.0.0.1:8443`）；TURN 子域 → 在 `stream` 里终止 TLS 后转**明文** TCP 到 LiveKit 的 `tls_port`。
  按 ALPN 分流不可靠：浏览器对 turns 连接不一定带 ALPN，所以只能靠 SNI，这也是必须用独立子域的原因。
- LiveKit 侧：`turn.enabled: true`、`turn.domain: turn.example.com`、`turn.tls_port: <内部端口>`、`turn.external_tls: true`、不配 `udp_port`（TURN/UDP 在 UDP 通的网络里没意义，那时直连早已成功）、`bind_addresses` 含回环/容器地址。下发给浏览器的即 `turns:turn.example.com:443?transport=tcp`，凭证由 LiveKit 按参与者短时签发。
- hearth 侧新增 `lkembed_turn_domain` / `lkembed_turn_port`（Group `stage`），两者皆非空才开启；`cmd/stage` 对应 `STAGE_TURN_DOMAIN` / `STAGE_TURN_PORT`。
- 容器部署要把 `tls_port` 映射出容器给 nginx 连；relay 端口不映射。

**单文件裸机形态（hearth 自带 CertMagic 终止 TLS）**：hearth 自己在 443 上按 SNI 分流，或把证书路径喂给 LiveKit（`cert_file/key_file`，续期后需重载）。留到 TURN 实施时一并决定，不在本计划展开。

### 埋点收编：保留，改"轮询全量"为"结果驱动"

**结论：保留并提交为常驻能力。** 本次排障里，`selected_server` 那一行日志是唯一让"通了/没通"变得可归因的东西——没有它，两种网络各自走了 UDP 还是 TCP 根本无从知晓。重写它的成本远高于收编。

但诊断期形态不能常驻：400 ms 轮询、每候选每 pair 一条、全量 info 级别，正常通话一次进房就是二三十行。改为：

- **轮询保留但只收集、不上报**。轮询存在的理由成立：SDK 在 15 s 超时后立即关闭 PeerConnection，不提前抓就拿不到失败候选。
- **连接成功**：只报一条 `sdk_selected_server`（`pub`/`sub` 各一，`protocol/candidateType addr:port`）；建连耗时已在 `connect_ok` 的 `elapsed_ms` 里。
- **连接失败**（`connect` 抛错、含超时）：报一条 `sdk_ice_failed`，内容为最后一次快照的全部 candidate-pair（`state[ nominated] local -> remote`，每行一条、换行分隔，最多 24 条、超出截断并标注），外加 local/remote 候选各自的去重列表。
- 服务端 `clientLogRequest` 加 `Detail string` 字段承载快照，`redactClientLogText(in.Detail, 2000)`；`state` 维持 80。body 8 KB 上限不变，快照按上述上限在前端裁好。
- `room.tsx` 层的生命周期事件（`credentials_*`、`connect_*`、`visibility_changed`、`browser_online/offline`、`page_hide`、`window_error`、`unhandled_rejection`、`room_open/close`）与引擎层状态事件（`sdk_connection_state`、`sdk_signal_connected`、`sdk_reconnecting/reconnected`、`sdk_ended`）**原样保留**：每个都是一次性事件，成本可忽略。
- 隐私边界：上报的地址是用户自己的公网出口与服务器地址，只进服务端进程日志（与 HTTP access log 同级），不落库、不展示、不进仓库。`clientlog.go` 现有的 token/URL/密钥脱敏保留。
- iOS 地址为 `unknown` 的限制写进 `reportTransportStats` 的注释（已有）与本文档，排障时优先用桌面 Chrome。

## 改动清单

| 位置 | 改动 |
| --- | --- |
| fork `pkg/config/config.go`、`pkg/service/roommanager.go` | 补丁四（约 10 行）；打 `v1.13.6-hearth.4` |
| `server/livekit-patches/0003-*.patch`、`0004-*.patch`、`README.md` | 补丁权威副本（0003 补齐既有提交，0004 新增）；表格加两行 |
| `server/go.mod` | `replace ... v1.13.6-hearth.4` |
| `server/internal/rtc/livekitembed/livekitembed.go` | `Options.ClientSTUNServers`、`DefaultClientSTUNServers`、`Start` 挂 `conf.RTC.ClientSTUNServers`、dyn 键 `lkembed_client_stun_servers`、`lkembed_tcp_port` Hint |
| `server/internal/rtc/livekitembed/livekitembed_test.go` | `none`/空/列表三种取值挂到 `conf.RTC` 的断言 |
| `server/internal/api/lkembed.go` | `EnsureStageKernel` 传新键 |
| `server/cmd/stage/main.go` | `STAGE_CLIENT_STUN_SERVERS` + 头部注释 |
| `server/internal/api/clientlog.go`、`clientlog_test.go` | `Detail` 字段与上限；测试补 `Detail` 脱敏/截断；**提交** |
| `web/src/engine/livekit.ts` | 删 `rtcConfig` 硬编码；ICE 采集改结果驱动；**提交** |
| `web/src/api.ts`、`web/src/views/room.tsx` | `ClientLogEntry` 加 `detail`；**提交** |
| `README.md`、`docs/plan-livekit-embed.md` | 部署段：媒体端口 udp/tcp 双放行；浏览器 STUN 由服务端下发 |

## 验收标准

1. `cd server && go build ./... && go vet ./... && go test ./internal/api/... ./internal/rtc/...`；`cd web && npx tsc --noEmit && npm run build` 全过。
2. 前端源码中不再出现任何 STUN 地址字面量（`grep -rn "stun:" web/src` 为空）。
3. fork 单测：`ClientSTUNServers` 为 nil 时行为与上游一致（回落默认）；为空切片时 `iceServersForParticipant` 返回不含任何 `stun:` URL；为列表时按列表下发。
4. 真机（桌面 Chrome，地址字段完整）三组对照，其余条件全部锁定：
   - 默认并列列表：`selected_server` 与建连耗时不劣于本次基线（家庭分流网络 tcp ≈ 561 ms，蜂窝 udp ≈ 322 ms）。
   - `none`：仍能建连（复现本次对照实验结论）。
   - 单填一个本地不可达的 STUN：仍能建连，耗时增幅可解释（STUN 超时不阻塞 trickle）。
5. 一次成功进房的诊断日志 ≤ 4 行 SDK 事件（`connection_state`×2、`signal_connected`、`selected_server`）；人为制造失败（如临时把 `lkembed_tcp_port` 设 0 并在分流网络进房）时，日志里出现一条含完整 pair 快照的 `sdk_ice_failed`。
6. `cmd/stage` 与 `lkembed` 两种形态各跑一遍第 4 条的默认组。

## 风险与不做

- fork 再发一版：按 `livekit-patches/README.md` 的"跟上游"流程，四个补丁，冲突概率低。
- 改动重启生效：与现有 `lkembed_*` 键一致，不做热更新。
- **不做**：TURN 的实施（第三层，单独排期）；改 `lkembed_stun_servers` 的语义；给客户端下发 TURN 以外的任何 relay；hearth 自管 TLS 形态下的 443 分流。
- 不把本次排障中出现的任何具体网络环境（路由器型号/分流软件/出口地址）写进代码注释与提交信息。
