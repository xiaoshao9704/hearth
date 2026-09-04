# Hearth 开发规范

## 语言与风格

- 代码注释、提交信息正文、面向用户的错误文案用中文；标识符、日志键、技术术语保留英文。
- 注释只写代码本身说不清的约束与取舍，不复述代码、不写"本次改动说明"。
- 最小改动原则：不顺手重构、不加未被要求的抽象/配置项；单次使用的代码直接写。

## 隐私铁律（仓库内一切可提交内容）

本仓库公开。文档、代码注释、提交信息、测试数据、示例配置、计划文档里**不得出现维护者的个人部署与生活信息**：

- 不写具体硬件与网络环境：设备型号/品牌（Mac mini、树莓派、显卡型号）、带宽数字、运营商、家宽/宿舍/公司等位置属性、DDNS/家庭路由器。
- 不写社交关系与使用者画像：朋友、观众是谁、谁在推流、开发机是什么。
- 不写真实标识：域名、IP、邮箱、本机路径、真实密钥；示例一律用 `example.com`、`<占位>`、`change-me`。
- 用中性表述替代：「上行有限的服务器」「LiveKit 同一局域网的机器」「arm64 小主机」「实际观众设备」「另一台机器」。
- 地区性的通用部署建议（如国内 STUN 不可达）可以保留；产品面向的场景词（开黑、游戏画面）可以保留。
- 提交信息同样受约束——历史一旦推送就难以抹除，写之前先自查一遍。

## 架构铁律

### rtc 内核插件模型（server/internal/rtc/）

- `rtc.Provider` / `rtc.StageProvider` / `rtc.IngestProvider` 接口保持**中性命名**，不得泄漏任何具体实现（LiveKit 等）的语义。
- **业务状态的权威在 store（DB），内核只是现场执行器**：禁言/封禁等管制状态先落库、再向内核尽力传播（`ErrNoParticipant` 不算失败）。内核不需要理解业务状态，只需会对当前参与者执行操作。
- **唯一内核类型，两种形态**：自研内核（Ember 语音、Bellows 推流网关、livekit-ingress 适配）已全部退役，内核只剩 LiveKit 一族。实例类型只剩两个：`livekit-embedded`（内建、alias 固定 `lkembed`，补丁式 fork 的 LiveKit 进程内跑，不接受 DB 注册）与 `livekit`（远端形态 `cmd/stage` 或官方 LiveKit，DB 注册或 `LIVEKIT_API_URL` env 合成同名锁定实例，同类型可多个）。`rtc.Provider`/`rtc.StageProvider` 抽象与注册制不因只剩一个实现而拆掉——远端 `cmd/stage` 与官方 LiveKit 都是同一类型的不同实例，注册制正是为它们服务的。
- 内核选择是**服务实例注册制**：`voice_provider`/`stage_provider` 选择器的值是实例 alias（不再是实现名）。实例三来源：内建（`lkembed`，排最前、是两个槽位的默认值）、env 锁定（`LIVEKIT_API_URL` 存在即合成 alias=`livekit` 的只读实例，至多一条）、DB 注册（`providers` 表，`/api/admin/providers` CRUD）。注册表与实例对象重建在 `api/providers.go`；选择器合法性按「实例存在 + 能力匹配槽位」校验（dyncfg.go），未知 alias 回落内建默认（voice→lkembed、stage→lkembed，显式 `none` 保持纯语音）。语音舞台同选一套实例即单连接（combined）形态，这是默认。
- **推流入口 = 当前舞台实例**，没有独立的 ingest 选择器：剩下的每种实例都自带 WHIP 入口，「推流进哪个实例」与「舞台在哪个实例」不可能不同——推进别的实例观众看不到。OBS 的 WHIP 一律进 `/providers/{alias}/w/{channel}`（令牌在路径段或 `Authorization: Bearer`），alias 必须是当前舞台实例否则 404；hearth 做完入场判定后现签短时效 LiveKit 票换掉用户令牌反代过去（`livekitrtc/whip.go`）。
- 实例连接参数存 params（键名沿用 `livekit_*` 命名空间，rtc 实现零改动）；仍是全局键的只有选择器与进程内网络基建（`portmap_mode`）。旧 `cfg_livekit_*` 等全局键启动时一次性导入为实例后删除（`migrateProviders`）。`lkembed` 类型 `livekit-embedded`（`server/internal/api/lkembed.go`）：实例对象复用 `livekitrtc.New`，`embedCfg` 把它要的 `livekit_api_url/key/secret` 映射到 `lkembed_port`（回环地址）与 `lkembed_api_key`/`lkembed_api_secret`（留空首启生成落库），`livekit_*` 命名空间因此对 lkembed 零改动地复用。`lkembed_public_ip`/`lkembed_stun_servers` 是显式公网 IP / STUN 覆盖键（承接已删内核的同名能力）。
- 接入路径统一 `/providers/{alias}/...`：`/rtc/*` livekit 信令反代、`/w/{channel}[/{token}]` WHIP 推流（按路径 alias 裁决：OBS 推给哪个实例就由哪个实例判定/签发/反代）。
- 打洞与宣告基建在 `rtc/lite`：`lite.Announcer`/`candidate` 周期探测（STUN/显式公网 IP + 端口映射结果并列），公网 IP 或映射变化不重启、不动在途会话；lkembed 的候选地址改写从它的快照取外部地址。`/healthz` 与 `healthcheck` 子命令只表示进程活着、无副作用：探测失败/映射为空不得返回非 200（防 autoheal 误杀）；网络诊断回显走管理接口，不放进 healthz。
- **user_id 是唯一的身份键，username 只做展示/登录/注册**。identity 由 `rtc.Identity(userID, tag)` 组成 `u{user_id}` 或 `u{user_id}-{标签}`（浏览器标签 = 设备标签，推流标签 = 令牌的可改属性，默认 `obs`），归属判断**必须**用 `rtc.MatchesUser(identity, userID)`，禁止手写主体解析。用户名绝不进入判定路径：它可改、改后旧名即释放、且字符集含 `-`——拿它当键会在改名后让归属错位，也会让互为前缀的两个用户名彼此误伤（禁言一个掐掉另一个的推流）。管理接口（踢出/封禁/禁言/移出白名单）一律收 `user_id`；例外只有「加白名单」，那是房主手输名字的一次 `名字 → 用户` 查找（与登录同类），查到后立即换成 user_id。
- 展示信息统一走参与者元数据 `rtc.Meta{uid,username,kind,tag}`：进房令牌（`lktoken.Sign` 的 `SetMetadata`）与推流判定（`admitIngest` 挂 ctx、换票时写入）两条路径都写。前端据此显示名字、按 uid 聚合设备、识别推流设备（`kind=ingest`），**不解析 identity、不按用户名反查**。
- `MuteUserAudio` 契约：禁言 = 禁**全部**媒体发布（音频/摄像头/投屏），不只是音频。
- 凭证是短时效入场券（10 分钟 TTL），不是会话生命周期授权；断线重连必须回到签发路径重新判定。

### 权限判定（server/internal/perm/）

「谁能做什么」收口在 perm 包（角色类型与存取在 store）：handler 与中间件一律调 `perm.SysAtLeast`/`perm.CanActOn`/`perm.ChannelRole`，不得手写 `IsAdmin`/`OwnerID` 比较（grep 只允许命中 perm 与 store）。系统角色是严格阶梯 `guest<user<power<admin<super`（`users.role` 权威，`is_admin` 双写期只读派生、下个版本删列）：只能操作比自己低档的人；super 全站恰好一个，只经 CLI `hearth promote <用户名>` 转移（旧 super 降 admin）。频道角色权威在 `channel_members.role`（owner/moderator/member），`channels.created_by` 只作历史；系统 admin+ 在任何频道隐含 owner。注册产出档由 `reg_default_role`（user/power）或邀请上指定的档决定。前端不推导权限：显隐一律按服务端返回的 `role`/`my_role`/`can_set_roles`。

### 入场判定（server/internal/api/admission.go）

一条规则，两个执行点：`admitUser` 是唯一的"谁能进房、能否发布"决策函数（返回 `UID`/`Username`，identity 由调用方经 `rtc.Identity` 组），`joinToken`（凭证签发）与 `/providers/{alias}/w` POST（WHIP 推流拦截，统一走 `admitIngest`：令牌反查用户 + URL 取频道，换票反代到该实例自带 WHIP，definitive 404/403/503 无 fail-open）都调它。新增入口或新增入场约束时**只改这里**，不得在别处散落 `CanJoin`/`IsGagged` 组合。

### 动态配置（server/internal/api/dyncfg.go）

优先级：环境变量（锁定，后台只读）> DB settings（`cfg_` 前缀，保存即生效）> 实现声明的默认值。带 `Options` 的键后端校验枚举值。例外：两个内核选择器（`voice_provider`/`stage_provider`）不读环境变量——env 的职责只是把 provider 实例合成进可选列表，选择一律走管理后台落库；部署侧旧的选择器 env 由迁移 v2 一次性导入后不再读取。

### 前端（web/）

- 房间页是 Solid（`views/room.tsx`）：状态一律走信号/派生 memo，**禁止**引入第二真相源（手工同步的布尔副本）；引擎产的媒体元素是命令式节点，用 ref 挂载不重建。
- 设置浮层骨架（`views/settings.tsx`）、频道管理（`views/manage.tsx`）、管理后台（`views/admin.tsx`）也是 Solid；设置的个人 pane 暂留命令式渲染（`settings-panes.ts`，由骨架挂进容器、切页调清理函数），逐个迁移即可。一次性渲染的轻页面（shell/lobby/login/join）保持 vanilla TS；vite-plugin-solid 只处理 `.tsx`。
- 设置的三个维度：个人（跟账号/本机走，即改即存）、频道（房主与频道管理员视角，落库即生效、每次操作 toast）、服务器（管理后台 `#/admin`，浮层里只放跳转）。所有齿轮入口都开同一个浮层，只是落点不同；浮层按 `channel` 上下文自查 `my_role`（owner/moderator）决定是否出「频道」分区，入口不必区分谁是房主。
- CSS 统一在 `src/style.css`，类名复用既有设计系统（ember 主题、三态明暗），选择器注意特异性（button 重置用零特异性 `:where`）。
- 引擎抽象 `engine/types.ts`：注册表只剩 `livekit` 一个实现（`engine/index.ts` 动态导入，保持代码分割）；新内核实现 `AVEngine` 即可挂回注册表。

### 单二进制/单容器（自包含镜像 Dockerfile.aio + server/cmd/aioinit 已退役并删除）

- 舞台内核不再靠「拉外部子进程」自包含：`stage_provider` 选中内建实例 `lkembed`（补丁式 fork 的 LiveKit，进程内跑，见 `rtc/livekitembed`）即在 hearth 自己的进程里热启动/热停止（`API.EnsureStageKernel`），没有第二个进程、没有 redis、没有 ingress。`Dockerfile.release`（CI 装配）与根 `Dockerfile`（本地构建）这一个镜像就是完整形态，不再需要 `-livekit`/`-full` 两档（两个过渡别名 tag 只保留了一个版本，已随内核收敛删掉）。
- aioinit 因此整体退役：它唯一的职责——按 `EMBED_LIVEKIT`/`EMBED_INGRESS` 拉起外部 livekit-server/redis/ingress、生成 `livekit.yaml`/`ingress.yaml`、把接入地址注入 hearth 环境——随内嵌形态消失而消失，不是「简化保留」，是没有职责剩下；`Dockerfile.aio` 与 `server/cmd/aioinit` 已删除。
- 残留的 `EMBER_*`/`BELLOWS_*`/`INGRESS_UPSTREAM_URL` 是已删内核的痕迹：一律不再读取，检测到就打一次启动告警（`warnLegacyConfig`，`server/internal/api/dyncfg.go`）提示从部署侧删除。告警集只保留一个版本（比照 `pion_*` 先例），下个版本删除。
- `/data` 仍是唯一的持久化边界，但只剩数据库：内嵌 LiveKit 的密钥不走 `/data/aio/keys.env`，是 `lkembed_api_key`/`lkembed_api_secret` 两个 DB settings 键，留空时首启生成并落库，随数据库一起备份（见上条「rtc 内核插件模型」）。
- 前端产物经 `server/internal/webui`（`//go:embed all:dist`）编进二进制，单文件分发成立：CI/Dockerfile 在 `go build` 前把 `web/dist` 拷入该目录（gitignore，只留 `.keep`）；目录为空时 `Handler()` 返回 nil，`main.go` 回落 `STATIC_DIR`（开发期 vite dev 不受影响）。裸机单文件的数据目录：`--data`/`HEARTH_DATA` → 可执行文件旁的 `data/`（便携优先）→ 系统用户目录回落；`DB_PATH` 默认 `<data>/hearth.db`，`.env` 先读工作目录再读 `<data>/.env`（后者不覆盖前者）。

## 已知的坑（改相关代码前必读）

- `websocket.Accept` hijack 后 `r.Context()` 被 net/http 取消：连接生命周期用 `context.WithoutCancel`。
- `nhooyr.io/websocket` 不允许并发写：所有出站消息必须走参与者的 send channel + writeLoop。
- Solid 视图作为路由页时 `render()` 不能直接挂 `#app`：main.ts 的 hashchange 监听 `route()` 先注册先执行、先把下一个视图画进 `#app`，随后视图自己注册的 dispose 才跑，而 dispose 会清空所挂容器——直接挂 `#app` 会把新视图一起擦成白屏。一律挂到自建的宿主 div（见 manage.tsx / admin.tsx）。
- 改挂载进容器的配置文件后 compose 不会自动重启服务，需手动 `docker restart`。
- livekit `use_external_ip: true` 时默认 STUN 不可达会启动即死（国内必配 `LIVEKIT_STUN_SERVERS`）；`docker cp` 进容器的文件是 root 属主，distroless nonroot（65532）进程会写不动。
- store schema 变更只加迁移文件（`server/internal/store/` 包根的 `NNNNN_name.go`，bun/migrate 从文件名解析迁移名，文件名不可改），不改 `00001_baseline.go`；api 语义迁移走 `settings.migration_version` 游标，两者职责不交叉（启动顺序：store.Open 的 Bun schema 迁移 → api 游标数据迁移）。Bun `NewRaw` 的 `?` 参数是按方言安全内联格式化进 SQL（不是改写成驱动占位符再绑定），传参无需顾虑方言。

## 验证与发布

- 服务端：`cd server && go build ./... && go vet ./...` 必须通过。
- 前端：`cd web && npx tsc --noEmit && npm run build` 必须通过。
- 行为改动尽量本地起服务验证：`go run ./cmd/server` 零外部依赖（选择器默认 voice/stage 均 lkembed，语音 + 投屏开箱可用，浏览器进房说话与投屏可见即通过）。注意 `.env` 里有 `LIVEKIT_*` 时迁移会把选择器落库成 livekit，本地验证要用干净 DB 或先改 settings 里的 `cfg_voice_provider`/`cfg_stage_provider`；lkembed 的媒体端口（默认 47720/udp）被别的 hearth 进程占着时进程内 LiveKit 起不来，管理后台改 `lkembed_udp_port` 即可。
- 发布：打 `v*` tag 触发 CI（`.github/workflows/release.yml`，原生交叉编译 + 纯装配镜像，无 QEMU）。
