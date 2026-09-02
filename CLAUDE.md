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

- `rtc.Provider` / `rtc.IngestProvider` 接口保持**中性命名**，不得泄漏任何具体实现（LiveKit 等）的语义。
- **业务状态的权威在 store（DB），内核只是现场执行器**：禁言/封禁等管制状态先落库、再向内核尽力传播（`ErrNoParticipant` 不算失败）。新内核不需要理解业务状态，只需会对当前参与者执行操作。
- 内核选择是**服务实例注册制**：`voice_provider`/`stage_provider`/`ingest_provider` 选择器的值是实例 alias（不再是实现名）。实例三来源：内建（`ember` 语音、`bellows` 进程内推流，排最前、是默认值）、env 锁定（`LIVEKIT_API_URL` / `INGRESS_UPSTREAM_URL` / `BELLOWS_REMOTE_URL` 存在即合成同名只读实例，每类至多一条）、DB 注册（`providers` 表，`/api/admin/providers` CRUD，同类型可多个）。注册表与实例对象重建在 `api/providers.go`；选择器合法性按「实例存在 + 能力匹配槽位」校验（dyncfg.go），未知 alias 回落内建默认（voice→ember、stage→none、ingest→bellows）。
- 实例连接参数存 params（键名沿用 `livekit_*` / `ingress_upstream_url` / `bellows_remote_url` 等旧命名空间，rtc 实现零改动）；仍是全局键的只有选择器与进程内网络基建（`bellows_udp_port`/`bellows_public_ip`）。旧 `cfg_livekit_*` 等全局键启动时一次性导入为实例后删除（`migrateProviders`）。
- 接入路径统一 `/providers/{alias}/...`：`/rtc/*` livekit 信令反代、`/voice` ember 信令 WS、`/w/{channel}[/{token}]` WHIP 推流（令牌在路径段或 `Authorization: Bearer`，按路径 alias 裁决：OBS 推给哪个实例就由哪个实例判定/签发/反代）。旧路径 `/lk`、`/w`、`/api/voice` 已删除，不留兼容。
- 自研内核命名：**Ember**（语音，`rtc/ember`，内建实例 alias `ember`）、**Bellows**（WHIP 推流网关，`rtc/bellows`，内建实例 alias `bellows`；远端形态类型 `bellows-remote`）。改名前的选择器值 `pion` 与配置键 `pion_*` 只在 v0.3.0 做过兼容映射，v0.3.1 起不再识别：`pion_*` 被忽略回落 `ember_*` 默认值，启动时 `warnLegacyConfig` 打一次告警提示改配置。不要再加回兼容映射。
- 接口分层：`rtc.Provider` 是语音（房间）内核，`rtc.StageProvider` 内嵌 Provider 代表舞台（视频）内核——舞台槽位只接受 StageProvider，视频专属方法只加在 StageProvider 上；Ember 补齐视频能力后实现 StageProvider 即可上舞台线。进程内 ICE-Lite 内核共用的传输基建在 `rtc/lite`，不要在各内核里复制：`lite.Transport` 持有 UDP mux 与 MediaEngine（只建一次），`webrtc.API` 按 PeerConnection 用 `lite.Announcer` 的当前宣告规则组装——探测经 `/healthz?refresh=1`（仅回环来源）或容器 healthcheck 子命令（`hearth|bellows healthcheck`）周期刷新，公网 IP 变化不重启、不动在途会话。健康检查只表示进程活着：探测失败/映射为空不得返回非 200（防 autoheal 误杀）。
- 推流出口走 `rtc.Publisher` 能力接口（「把轨发布进舞台内核」的客户端能力，挂在舞台实例对象上）：Bellows 对舞台内核中立，进程内形态每次发布时取**当前舞台线实例**的 Publisher（注册表切换即生效，取不到则 `Enabled=false`），远端 `cmd/bellows` 编译进全部实现、由 `BELLOWS_SINK`（默认 `livekit`）选用。PLI 桥接约定：`TrackRemote` 不暴露所属连接，「观众关键帧请求 → 推流端 PLI」的回执无法走接口参数，由 bellows 在调 `PublishRemote` 前用 `rtc.WithKeyframeRelay` 挂到 ctx、Publisher 经 `rtc.KeyframeRelay` 消费（取不到则丢弃关键帧请求）。**回执一律走 ctx**：`Publisher` 接口只描述「发布一条轨」，容纳不下会话级事件。同一机制的第二条是 `rtc.WithPublishLost`/`rtc.PublishLost`——Publisher 与舞台内核断连后**必须**回执给 bellows 拆掉推流会话，让推流端重推：已建立的会话不会再产生新轨去触发重连，只摘连接不拆会话的话轨会一直写进死连接，表现为推流端显示正常而观众永久黑屏。
- `rtc.IngestProvider` 的端点方法是**按发布身份（identity, 标签）而非（用户, 房间）**的语义：`EnsureEndpoint`/`BindRoom`/`DeleteEndpoint` 管理「用户令牌 → 实例凭证」的上游映射（livekit-ingress：反代前 `BindRoom` 写房间、改写 Bearer 为上游 stream key）；Bellows 的实例凭证就是通行证，三个方法空实现。令牌重置/改标签时删除该令牌名下全部端点，下次推流重建。
- **user_id 是唯一的身份键，username 只做展示/登录/注册**。identity 由 `rtc.Identity(userID, tag)` 组成 `u{user_id}` 或 `u{user_id}-{标签}`（浏览器标签 = 设备标签，推流标签 = 令牌的可改属性，默认 `obs`），归属判断**必须**用 `rtc.MatchesUser(identity, userID)`，禁止手写主体解析。用户名绝不进入判定路径：它可改、改后旧名即释放、且字符集含 `-`——拿它当键会在改名后让归属错位，也会让互为前缀的两个用户名彼此误伤（禁言一个掐掉另一个的推流）。管理接口（踢出/封禁/禁言/移出白名单）一律收 `user_id`；例外只有「加白名单」，那是房主手输名字的一次 `名字 → 用户` 查找（与登录同类），查到后立即换成 user_id。
- 展示信息统一走参与者元数据 `rtc.Meta{uid,username,kind,tag}`：进房令牌（`lktoken.Sign` 的 `SetMetadata`）与推流发布（`publisher.go`）两条路径都写，ember 经 `welcome.self` / `roster` 下发。前端据此显示名字、按 uid 聚合设备、识别推流设备（`kind=ingest`），**不解析 identity、不按用户名反查**。
- `MuteUserAudio` 契约：禁言 = 禁**全部**媒体发布（音频/摄像头/投屏），不只是音频。
- 凭证是短时效入场券（10 分钟 TTL），不是会话生命周期授权；断线重连必须回到签发路径重新判定。

### 入场判定（server/internal/api/admission.go）

一条规则，三个执行点：`admitUser` 是唯一的"谁能进房、能否发布"决策函数（返回 `UID`/`Username`，identity 由调用方经 `rtc.Identity` 组），`joinToken`（凭证签发）、`/providers/ember/voice`（ember 验票入会）、`/providers/{alias}/w` POST（WHIP 推流拦截，统一走 `admitIngest`：令牌反查用户 + URL 取频道，进程内 bellows / 远端 bellows / livekit-ingress 三条路径共用，definitive 404/403/503 无 fail-open）都调它。远端 `cmd/bellows` 进程没有数据库也不回调：hearth 在反代前做完判定，把结果签成短时效通行证（grant）塞进请求头，远端只本地验签（与 LiveKit join token 同一模型）。新增入口或新增入场约束时**只改这里**，不得在别处散落 `CanJoin`/`IsGagged` 组合。ember 线走一次性入场票（60s、取出即删、防挪用），不做二次判定。

### 动态配置（server/internal/api/dyncfg.go）

优先级：环境变量（锁定，后台只读）> DB settings（`cfg_` 前缀，保存即生效）> 实现声明的默认值。带 `Options` 的键后端校验枚举值。例外：三个内核选择器（`*_provider`）不读环境变量——env 的职责只是把 provider 实例合成进可选列表，选择一律走管理后台落库；部署侧旧的选择器 env 由迁移 v2 一次性导入后不再读取。

### 前端（web/）

- 房间页是 Solid（`views/room.tsx`）：状态一律走信号/派生 memo，**禁止**引入第二真相源（手工同步的布尔副本）；引擎产的媒体元素是命令式节点，用 ref 挂载不重建。
- 其余视图保持 vanilla TS；vite-plugin-solid 只处理 `.tsx`。
- CSS 统一在 `src/style.css`，类名复用既有设计系统（ember 主题、三态明暗），选择器注意特异性（button 重置用零特异性 `:where`）。
- 引擎抽象 `engine/types.ts`：新内核实现 `AVEngine` 并在 `engine/index.ts` 注册动态导入（保持代码分割）。

### 自包含镜像（Dockerfile.aio + server/cmd/aioinit）

- aioinit 是容器 PID1：按 `EMBED_LIVEKIT` / `EMBED_INGRESS` 拉起内嵌 livekit/redis/ingress，再拉起 hearth；子进程退避重启、SIGTERM 广播。
- 密钥只持久化 `/data/aio/keys.env`（首启生成）；`livekit.yaml`/`ingress.yaml` **每次重启按环境变量重生成（env 权威，手改不保留）**——新增可调参数一律加环境变量，不要往 yaml 里塞静态值。
- `/data` 是唯一的持久化边界：数据库、密钥、生成的 yaml 全在里面，不加自定义路径开关，用户挂卷即备份。
- 内嵌服务与 hearth 的接线（`LIVEKIT_API_URL`、`INGRESS_UPSTREAM_URL` 等）由 aioinit 按端口 env 推导注入，业务代码不感知 aio 形态。

## 已知的坑（改相关代码前必读）

- `websocket.Accept` hijack 后 `r.Context()` 被 net/http 取消：连接生命周期用 `context.WithoutCancel`。
- ember（语音内核，`rtc/ember`）的 `vroom.mu` 不可重入：持锁时禁止调用 `snapshot()`/`roster()` 等会再抢锁的方法。
- `nhooyr.io/websocket` 不允许并发写：所有出站消息必须走参与者的 send channel + writeLoop。
- 前端引擎重连前必须解绑旧 ws 的 handler（`teardown()`），否则旧连接被判 duplicate 时会误伤新连接。
- 客户端向 ICE-Lite 服务端发 offer 不必等 gathering complete（最多等 1s），部分环境 gathering 永不完成。
- 改挂载进容器的配置文件后 compose 不会自动重启服务，需手动 `docker restart`。
- livekit `use_external_ip: true` 时默认 STUN 不可达会启动即死（国内必配 `LIVEKIT_STUN_SERVERS`）；`docker cp` 进容器的文件是 root 属主，distroless nonroot（65532）进程会写不动。
- ingress 的 WHIP 直通（bypass_transcoding）只收 H.264+opus：HEVC/AV1 会 `unsupported codec in SDP offer`（实测于 ingress v1.5.0）；OBS bearer 模式端点是精确 `/w`（`/w/` 会 404，hearth 代理层已做规范化宽容）。
- store schema 变更只加迁移文件（`server/internal/store/` 包根的 `NNNNN_name.go`，bun/migrate 从文件名解析迁移名，文件名不可改），不改 `00001_baseline.go`；api 语义迁移走 `settings.migration_version` 游标，两者职责不交叉（启动顺序：store.Open 的 Bun schema 迁移 → api 游标数据迁移）。Bun `NewRaw` 的 `?` 参数是按方言安全内联格式化进 SQL（不是改写成驱动占位符再绑定），传参无需顾虑方言。

## 验证与发布

- 服务端：`cd server && go build ./... && go vet ./...` 必须通过。
- 前端：`cd web && npx tsc --noEmit && npm run build` 必须通过。
- 行为改动尽量本地起服务验证：`go run ./cmd/server` 零外部依赖（选择器默认 ember 语音 + 关闭舞台线）。
- 发布：打 `v*` tag 触发 CI（`.github/workflows/release.yml`，原生交叉编译 + 纯装配镜像，无 QEMU）。
