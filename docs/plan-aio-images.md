# 计划：三档自包含镜像（方案 A）与后续演进

状态：**已退役（2026-09-04）**。内嵌 LiveKit 改为进程内嵌入补丁式 fork（`plan-livekit-embed.md`，内建实例 `lkembed`，
`stage_provider` 选中即热启动，无需外部子进程），本文档描述的「拉外部 livekit/redis/ingress 子进程」形态（`Dockerfile.aio`、
`server/cmd/aioinit`）已删除，`-livekit`/`-full` 镜像 tag 只在退役后的第一个版本里保留为主镜像的别名。本文档保留作历史记录。

以下为原文，已落地（2026-09-01）时的执行记录。执行中与原文的偏差：

- `Dockerfile.aio.dockerignore` 的 `!out/aioinit-linux-*` 行在落盘时已补齐，第 1 步无需执行。
- writeIfAbsent 语义改为 **env 权威**：`livekit.yaml`/`ingress.yaml` 每次重启按环境变量重生成（手改不保留），密钥仍只持久化 `keys.env`。
- 端口环境变量：`LIVEKIT_PORT`/`LIVEKIT_TCP_PORT`/`LIVEKIT_UDP_PORT`、`INGRESS_WHIP_PORT`/`INGRESS_UDP_PORT`/`INGRESS_TCP_PORT`；
  另加 `LIVEKIT_STUN_SERVERS`（默认 STUN 不可达时 livekit 启动即死，国内必配）与 `REDIS_ADDR`（填了用外部 redis，空则内嵌）。
- 基础镜像整体可经 build-arg 替换：`LIVEKIT_IMAGE`/`INGRESS_IMAGE`/`DISTROLESS_IMAGE`（版本钉死在默认值里）。
- release.yml：`-full` 档有 apt 层，arm64 构建需 setup-qemu（分钟级，可接受）；其余纯装配不变。
- 本地验证（arm64 开发机）两档全通过；livekit 外网 IP 探测依赖可用 STUN，媒体面留待 Linux 宿主复验。

## 背景与目标

把部署形态从「多容器 compose + 模板生成」扩展为按需选档的自包含镜像，
一条 `docker run` 到位。内嵌服务用**官方构建产物**（不自行编译 LiveKit），
进程编排用自研纯 Go init（保持 distroless 兼容，不引入 supervisord/s6）。

| tag | 内容 | base | 预期体积 | 默认开关 |
|---|---|---|---|---|
| `hearth:X` | 纯 hearth（pion 语音 + 聊天 + 管理） | distroless/static | ~36MB | 现状，不变 |
| `hearth:X-livekit` | + livekit-server（投屏/摄像头/弱网 SVC） | distroless/static | ~110MB | `EMBED_LIVEKIT=1` |
| `hearth:X-full` | + redis + ingress（OBS WHIP 推流） | livekit/ingress 官方镜像（Ubuntu + GStreamer） | ~600MB | + `EMBED_INGRESS=1` |

原则：
- 开关可覆盖（如 `-full` 档 `EMBED_INGRESS=0` 临时关推流）；`EMBED_INGRESS=1` 必须伴随 `EMBED_LIVEKIT=1`。
- 密钥/配置首启生成、落 `/data/aio/` 持久化（跨重启稳定）；用户手改的配置不被覆盖（writeIfAbsent）。
- 密钥经环境变量喂给 hearth，走「环境变量固定」语义，管理后台只读。
- 架构层零改动：combined 单线（两线 livekit）由动态配置默认值天然承接，后台仍可切 pion 双线。

## 已完成（未提交）

- `server/cmd/aioinit/main.go`：纯 Go PID1 编排器——EMBED_* 开关、密钥/`livekit.yaml`/`ingress.yaml` 生成、
  子进程退避重启（1s→30s，运行超 1 分钟重置）、SIGTERM 广播 + 10s 强杀、子进程日志加 `[name]` 前缀。
- `Dockerfile.aio`：`livekit` 与 `full` 两个 target；livekit-server 二进制自官方镜像 COPY
  （已验证为静态链接，可进 distroless）；`full` 基于 `livekit/ingress`（Ubuntu 24.04，
  ingress 二进制在 `/bin/ingress`），apt 安装 redis-server。
  版本钉死：livekit v1.13.6 / ingress v1.5.0（ARG 可覆盖）。
- `Dockerfile.aio.dockerignore`：已建，**缺一行 `!out/aioinit-linux-*`**（构建即卡在此，下一步第一件事）。

## 待执行步骤

1. **修 dockerignore**：`Dockerfile.aio.dockerignore` 补 `!out/aioinit-linux-*`，注释改名 aio。
2. **本地构建验证（arm64，本机）**
   - `-livekit` 档：build → run（映射 8080 + volume）→ 验证：三点
     a) 容器日志见 `[aioinit] livekit 已启动` 与 hearth 监听；
     b) `curl :8080` 200、登录建号、`/api/token` 返回 livekit 凭证（combined）；
     c) `/lk` 代理 → 内嵌 livekit twirp 可达（管理后台服务状态 voice/stage ok）。
     媒体层不在本机验证（Docker Desktop 的 UDP 端口转发损坏是已知问题），留待 Linux 宿主/生产。
   - `-full` 档：build（首次含 apt 层较慢）→ run → 额外验证：
     a) redis/ingress 均被拉起且无崩溃循环；
     b) hearth 后台「推流入口」显示已启用（INGRESS_UPSTREAM_URL=127.0.0.1:7888）；
     c) 设置页能创建 WHIP 端点（CreateIngress twirp 走通）。
   - 验证 nonroot 写 `/data/aio` 权限（distroless 档 USER 65532 + volume）。
3. **release.yml**：
   - build job：aioinit 双架构交叉编译（与 hearth 同 loop）。
   - image job：新增两个 build-push（`-f Dockerfile.aio --target livekit|full`），
     tag 形如 `ghcr.io/...:X-livekit` / `X-full`，multi-arch（两个官方 base 均有 arm64）。
4. **文档**：README 部署段改为三档表（快速开始各给一条 docker run）；
   CLAUDE.md 增补 aio 编排约定（aioinit 职责、/data/aio 布局、EMBED_* 语义、验证要求）。
5. **同步 main**：以上作为一个提交推送。发版（打 tag 出三档镜像）另行确认。

## 风险与回退

- livekit/ingress 官方镜像 tag 漂移：ARG 钉版本，升级=改一行 + 重验证。
- `-full` 档 Ubuntu base 的 CVE 面大：接受（与直接用官方 ingress 镜像等同）。
- aioinit 是 PID1 但不做孤儿进程收割：livekit/redis/ingress 均不产生脱管孙进程，接受；若未来出现僵尸进程问题再补 reaper。
- 回退：三档互不影响，标准档链路完全不变；aio 档出问题不影响现有部署。

## 后续演进（本计划不含，按序排队）

1. **方案 B：自研纯 Go WHIP ingest（`pionwhip`）**——WHIP 直通网关：pion 收 RTP → livekit server-sdk-go
   以 bot 参与者发布进房（我们的用法本就是 bypass transcoding，GStreamer 从未被用到）。
   做成 `rtc.IngestProvider` 第二实现（`ingest_provider` 枚举加项）。完成后并入 `-livekit` 档，
   **`-full` 档退役**（600MB → 0 增量）。工程要点：PLI/RTCP 桥接、lksdk PublishTrack(TrackLocalStaticRTP)。
2. **Windows 单文件版 v1**：`hearth.exe`（pion 语音 + 内嵌前端 + 自签 HTTPS——证书警告点过即为
   secure context，访客零安装）。首启体验：生成管理员、自动开浏览器、防火墙提示。
3. **Windows exe + 投屏**：依赖 pion 舞台内核（局域网场景纯直通转发即可，不需要 SVC 选层）
   或 LiveKit Windows 交叉编译试金石（官方不支持，需真机验证 UDP/网卡枚举）。
4. **EasyTier 组网**：无公网 IP 的用户组网开黑。前置：自签 HTTPS（同 2）；集成形态倾向伴生进程 +
   pion 通告虚拟网 IP。
5. AI 会话总结：挂起（定位不符 + ASR 算力/隐私成本）；pion 内核旁路音频的技术入口保留。
