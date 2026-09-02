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
- 配置键按实现命名空间隔离（`livekit_*`、`ember_*`、`bellows_*`），由实现自带 `ConfigKeys()` 声明；换内核不迁移配置。选择器与枚举值见 `api/dyncfg.go`。
- 自研内核命名：**Ember**（语音，`rtc/ember`，选择器值 `ember`）、**Bellows**（WHIP 推流网关，`rtc/bellows`，选择器值 `bellows`）。改名前的选择器值 `pion` 与配置键 `pion_*` 只在 v0.3.0 做过兼容映射，v0.3.1 起不再识别：选择器未知值回落 `livekit`，`pion_*` 被忽略回落 `ember_*` 默认值，启动时 `warnLegacyConfig` 打一次告警提示改配置。不要再加回兼容映射。
- 接口分层：`rtc.Provider` 是语音（房间）内核，`rtc.StageProvider` 内嵌 Provider 代表舞台（视频）内核——舞台槽位只接受 StageProvider，视频专属方法只加在 StageProvider 上；Ember 补齐视频能力后实现 StageProvider 即可上舞台线。进程内 ICE-Lite 内核共用的传输基建（UDP mux / 公网 IP 探测）在 `rtc/lite`，不要在各内核里复制。
- identity 约定：`{用户名}` 或 `{用户名}-{设备标签/obs}`，归属判断**必须**用 `rtc.MatchesUser`，禁止手写前缀判断。
- `MuteUserAudio` 契约：禁言 = 禁**全部**媒体发布（音频/摄像头/投屏），不只是音频。
- 凭证是短时效入场券（10 分钟 TTL），不是会话生命周期授权；断线重连必须回到签发路径重新判定。

### 入场判定（server/internal/api/admission.go）

一条规则，三个执行点：`admitUser` 是唯一的"谁能进房、能否发布"决策函数，`joinToken`（凭证签发）、`/api/voice`（ember 验票入会）、`/w` POST（WHIP 推流拦截）都调它。远端 `cmd/bellows` 进程没有数据库也不回调：hearth 在反代前做完判定，把结果签成短时效通行证（grant）塞进请求头，远端只本地验签（与 LiveKit join token 同一模型）。新增入口或新增入场约束时**只改这里**，不得在别处散落 `CanJoin`/`IsGagged` 组合。ember 线走一次性入场票（60s、取出即删、防挪用），不做二次判定。

### 动态配置（server/internal/api/dyncfg.go）

优先级：环境变量（锁定，后台只读）> DB settings（`cfg_` 前缀，保存即生效）> 实现声明的默认值。带 `Options` 的键后端校验枚举值。

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

## 验证与发布

- 服务端：`cd server && go build ./... && go vet ./...` 必须通过。
- 前端：`cd web && npx tsc --noEmit && npm run build` 必须通过。
- 行为改动尽量本地起服务验证：`VOICE_PROVIDER=ember STAGE_PROVIDER=none go run ./cmd/server` 零外部依赖。
- 发布：打 `v*` tag 触发 CI（`.github/workflows/release.yml`，原生交叉编译 + 纯装配镜像，无 QEMU）。
