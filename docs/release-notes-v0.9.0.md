# Hearth v0.9.0 发布说明

本版本的发布范围固定为三项：**内核收敛、单文件三系统、分层权限**。

- 自研的 Ember（语音）、Bellows（推流网关）与 livekit-ingress 适配整体退场，进程内补丁版 LiveKit（`lkembed`）成为唯一内核类型。两种形态：进程内（默认、零配置）与远端 `cmd/stage`；官方 LiveKit 仍可注册为外部实例。语音、投屏、摄像头、OBS 推流（HEVC/AV1 直通）全部走同一个内核。
- 前端资源编进服务端二进制，Linux、macOS、Windows 六个目标均提供可直接运行的单文件产物；数据目录按显式参数、便携目录、系统目录的顺序选择。
- 系统权限改为 `guest < user < power < admin < super` 的严格阶梯，频道权限改为 `member < moderator < owner`，所有判定统一收口到 `perm` 包。

CertMagic、libdns、DNS-01 与 IP 证书不在 v0.9.0 发布线上，留待真实环境验收后单独发布。

## ⚠️ 升级必读：默认语音端口变化

**默认语音媒体端口从 `47700/udp` 变为 `47720/udp`**（与投屏同一个端口，内核统一后只有这一条媒体通道）。

- 只放行了 47700 的旧部署，升级后**语音会断**：请把防火墙/安全组/compose 的端口放行改为 `47720/udp`。
- 使用 `portmap` 自动端口映射的部署不受影响（wants 已改为 47720，升级后自动重新映射）。
- 容器仍发布 47700/47710 不会报错，只是空放行，可以收掉。快速开始形态统一为 `-p 8080:8080 -p 47720:47720/udp`。

## 行为差异

- **纯语音部署也会跑 LiveKit**：进程内多一个 SFU 的常驻开销（内存约数十 MB 量级，具体数值**待实测**后补充），换来投屏/摄像头开箱即用（`stage_provider` 默认 `lkembed`，不再需要进管理后台开启）。
- **OBS 推流 URL**：路径形态不变，但原来用 Bellows 的部署要把 alias 段从 `bellows` 改成 `lkembed`：`/providers/lkembed/w/{频道}`。推流令牌不变。推流不再有独立的 `ingest_provider` 选择器——OBS 的 WHIP 一律进当前舞台实例自带的入口，URL alias 必须是当前舞台实例，否则 404。
- **HEVC/AV1 继续直通**：LiveKit 自带 WHIP 对 offer 编码原样保留，行为与上一版一致。
- **远端 Bellows 用户**：远端机器改为跑单个 `cmd/stage` 容器（替代原来的 livekit + bellows 两个容器，部署说明见 README「远端舞台机器」），hearth 侧把 `stage_provider` 指向该实例即可。

## 单文件三系统

- 发布产物覆盖 Linux、macOS、Windows 的 amd64/arm64 六个目标，均为 `CGO_ENABLED=0` 的单文件二进制。
- 前端构建产物经 `go:embed` 编进服务端；开发环境仍可用 `STATIC_DIR` 回落。
- 裸机数据目录优先级为 `--data` / `HEARTH_DATA`、可执行文件旁的 `data/`、系统用户目录；`DB_PATH` 默认落在所选数据目录。
- `hearth service install|uninstall|start|stop|status` 提供 macOS LaunchAgent、Linux systemd 与 Windows SCM 后台常驻；服务日志写入数据目录并自动轮转。

## 分层权限

- 系统角色以 `users.role` 为权威；只有更高档角色可以管理更低档角色，`super` 全站恰好一个，只能用 `hearth promote <用户名>` 转移。
- `power` 及以上可创建频道和发注册邀请；管理员可以配置注册默认档并管理角色。
- 频道归属以 `channel_members.role` 为权威，支持频道主、频道管理员、成员三档；系统 `admin+` 在所有频道隐含频道主权限。
- 旧 `is_admin` 与 `channels.created_by` 在本版只作兼容读取，下个版本再删列。

## 升级自动迁移（启动时一次性执行）

schema 迁移 `00003_roles` 先补齐角色字段，随后按不可复用的语义游标依次执行：

- **v5 分层权限**：旧管理员映射为 `admin`，其中最早一位提升为 `super`；已有频道主提升为 `power`，并写入频道 `owner` 角色行。
- **v6 内核收敛**：启动日志会出现 `迁移 v6:` 系列行，自动完成以下清理：

  - 选择器改写：`voice_provider` 为 `ember`/`pion`/`bellows`/失效 alias 的改为 `lkembed`；`stage_provider` 指向已删类型实例的改为 `lkembed`（显式 `none` 保持不变）。
  - 删除 `ingest_provider` 选择器与全部 `cfg_ember_*`/`cfg_bellows_*`/`cfg_pion_*` 配置键。
  - 删除 `livekit-ingress`、`bellows-remote` 类型的已注册实例（逐条打日志）。
  - 清空 `ingest_endpoints` 表（表结构保留，下个版本删除）。

## 部署侧需要手动清理的

以下环境变量**已不再读取**，启动时会各打一行告警，请从部署侧删除：
`EMBER_UDP_PORT`、`EMBER_PUBLIC_IP`、`EMBER_STUN_SERVERS`、`BELLOWS_*`（全部）、`INGRESS_UPSTREAM_URL`、`BELLOWS_REMOTE_URL`、`BELLOWS_SINK`。

显式公网 IP / STUN 覆盖能力由 `lkembed_public_ip` / `lkembed_stun_servers`（或对应 env）承接。

## 发布产物变化

- 不再发布 `bellows-linux-amd64/arm64` 二进制（`cmd/bellows` 已删除，远端形态用 `cmd/stage`）。
- 镜像过渡别名 `-livekit` / `-full` 按上一版声明到期移除，只剩主镜像与 `hearth-stage`。

## 待实测（发布前补充）

以下两项验收需要真实网络与硬件环境，本版发布时暂无数据，**待实测**后回填：

- **弱网语音 A/B**：同一台机器在弱网（丢包/抖动）下分别用 Ember（旧版）与 LiveKit（本版）各听 10 分钟，对比主观语音质量。LiveKit 具备更完整的丢包处理（NACK/RED/DTX），理论上不劣于 Ember，但需实测确认无回归。
- **常驻内存基线**：arm64 小主机上纯语音部署的常驻占用对比（旧版 Ember vs 本版进程内 LiveKit），若明显抬高则进一步收拢 LiveKit 的无关功能（日志级别等）。

## 退场清单（对开发者）

- 删除：`rtc/ember`、`rtc/bellows`、`cmd/bellows`、`lkingress`、`livekitrtc/ingress.go`、`livekitrtc/publisher.go`、`lite.Transport`、前端 `engine/ember.ts`。
- 接口瘦身：`rtc.Publisher`/`KeyframeRelay`/`PublishLost` 桥接约定删除；`rtc.IngestProvider` 的端点三方法（`EnsureEndpoint`/`BindRoom`/`DeleteEndpoint`）删除，推流面只剩 WHIP 服务能力。
- 实例类型只剩 `livekit-embedded`（内建，alias 固定 `lkembed`）与 `livekit`（DB 注册或 `LIVEKIT_API_URL` env 合成）。
- 下个版本删除：本版的遗留告警集与 `ingest_endpoints` 表。
