# 计划：宣告地址可刷新（健康检查触发探测，替代 watchdog 重启）

状态：已执行（announce-refresh 分支）。2026-09-02。
2026-09-03：healthz 触发刷新已改为进程内周期刷新（`/healthz` 精简为纯探活，不再接受 `refresh=1`、不再回显宣告），
见 `docs/plan-portmap.md`；下文正文保留原样，涉及健康检查触发的部分以此条为准。

基线：main 已含注入式 `announceRules`（PR #1 + #2，per-NIC 纯 STUN 探测、External 去重；HTTP 兜底已去除——STUN 全挂返回 nil，candidate 按网卡地址原样宣告）。

## 动机

宣告地址的探测在 `ensureAPI` 里只跑一次并缓存到进程结束：公网 IP 变化后新会话拿到的仍是旧候选，
只能靠重启进程（LiveKit 时代的 watchdog）。目标：进程内可刷新，刷新由容器健康检查周期触发
（也可手动触发），不重启、不丢现有会话。

## 设计

### 1. 规则可换：`webrtc.API` 不再整体缓存

pion 的地址改写规则挂在 `SettingEngine` 上，`webrtc.API` 建好后不可改。改成：
- `lite.Transport`（新类型）持有**长期资源**：UDP mux（socket）、`MediaEngine`、拦截器注册表；
- `Transport.NewAPI(rules)` 每次按当前规则组装一个 `webrtc.API`（纯结构体组装，无 socket、无网络，可以每建一个
  PeerConnection 组装一次）；
- 内核（ember/bellows）的 `ensureAPI` 改为 `ensureTransport` 只建一次，建 PC 时 `transport.NewAPI(announcer.Rules())`。
  现有会话的 PC 不受影响（候选早已交换完）。

### 2. 探测器：`lite.Announcer`

```go
type Announcer struct {
    publicIP, stunServers string   // 配置来源（ConfigFunc 逐次读，后台改配置即时生效）
    mu       sync.Mutex
    rules    []webrtc.ICEAddressRewriteRule
    probedAt time.Time
    probing  bool                  // 同一时刻只跑一次探测，其余调用者等它
}
func (a *Announcer) Rules(ctx) []Rule            // 缓存过期（TTL 默认 5 分钟）则同步刷新后返回
func (a *Announcer) Refresh(ctx, force bool) (changed bool, rules []Rule, err error)
                                                  // force=false 时距上次探测 < MinInterval（30s）直接返回缓存
func (a *Announcer) Snapshot() (rules, probedAt)   // 只读，给健康检查回显
```
- `Refresh` 复用现有注入式 `announceRules`，单测用假探测覆盖各分支（显式覆盖 / append / 全失败；
  已在 lite 层覆盖，Announcer 侧重 TTL/最小间隔/并发单飞/变化检测）。
- 规则变化时打一条日志（旧 → 新），不变则静默。
- 触发点三个：新建 PC 时缓存过期、健康检查端点、ICE 连续失败 N 次（可选，第二步再做）。

### 3. 刷新出口：健康检查端点

- Bellows 已有 `/healthz`；hearth 增加 `GET /healthz`（不鉴权）。两者行为一致：
  - 返回 200 + JSON `{"ok":true,"announce":{"rules":[…],"probed_at":…}}`；
  - 带 `?refresh=1` 时先调 `Announcer.Refresh(ctx, false)`（受 MinInterval 保护，外网刷不动）；
  - **只有回环地址来源才接受 `refresh`**（`RemoteAddr` 为 127.0.0.1/::1）：健康检查在容器内发起、天然回环，
    经反代进来的外部请求即使带参数也只回显不探测。两层保护叠加，端点可以不鉴权。
- 健康状态只表示"进程活着"：探测失败、映射为空都**不**返回非 200——否则 autoheal 类工具会把"公网探测服务
  暂时不可达"当作故障反复重启，比不刷新更糟。

### 4. 健康检查命令（distroless 无 shell/curl）

- `hearth healthcheck` / `bellows healthcheck` 子命令：GET `http://127.0.0.1:<port>/healthz?refresh=1`，5s 超时，
  HTTP 失败或非 200 退出码 1。端口从各自的 `ADDR`/`BELLOWS_ADDR` 环境变量推导。
- `Dockerfile.release`：`HEALTHCHECK --interval=60s --timeout=10s --start-period=20s CMD ["/app/hearth","healthcheck"]`；
  bellows 的 compose 示例用 `healthcheck: test: ["CMD","/app/bellows","healthcheck"]`（exec 形式，无需 shell）。
  aio 镜像同样加（PID1 是 aioinit，健康检查命令仍直接跑 hearth 二进制）。
- 间隔即刷新周期，由部署方按需调；默认 60s，探测最坏耗时（STUN 超时 + HTTP 兜底）远小于间隔。

### 5. 现有会话的恢复

规则刷新只影响新 PC。公网 IP 真的变了，旧会话的媒体路径已断，ICE 会在几十秒内判失败：浏览器端有自动重连
（回到签发路径拿新凭证），OBS 有自动重连（重新 POST）。所以不做"主动踢掉旧会话"——它们自己会回来。

### 6. 顺带修正（PR #1 评审项）

已在 PR #2 完成：国内可达默认 STUN、服务器并发探测、External 去重、端口假设注释。
本分支额外决策：去掉 HTTP 回显兜底，纯 STUN（全挂时按网卡地址原样宣告）。

## 测试

- `Announcer`：TTL 过期刷新、MinInterval 内不重复探测、并发调用只探测一次、规则变化返回 `changed=true`；
  假探测覆盖四个分支。
- `/healthz`：回环带 `refresh` 触发探测；非回环带 `refresh` 只回显；探测失败仍 200。
- `healthcheck` 子命令：端口推导、超时、退出码。
- 两进程冒烟：起 hearth + bellows，改假探测返回值后调一次健康检查，新建会话的 SDP 候选变为新地址，旧会话不受影响。

## 验收

- 容器 `docker inspect` 显示 healthy；`docker logs` 每次映射变化各一条日志，不变时无输出。
- 模拟公网 IP 变化（假探测 / 改路由器映射）后 60s 内新推流/新进房拿到新候选，进程未重启。
- 树莓派上的 `ip-watchdog` 可以下线（LiveKit 仍在则保留其重启，只对 Bellows 不再需要）。

## 工作量

约 250 行 Go（`lite.Transport`/`Announcer`、两处 `ensureAPI` 改造、`/healthz`、子命令）+ Dockerfile/compose 两行，1–2 天。
