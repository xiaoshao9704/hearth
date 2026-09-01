# Hearth 开发规范

## 语言与风格

- 代码注释、提交信息正文、面向用户的错误文案用中文；标识符、日志键、技术术语保留英文。
- 注释只写代码本身说不清的约束与取舍，不复述代码、不写"本次改动说明"。
- 最小改动原则：不顺手重构、不加未被要求的抽象/配置项；单次使用的代码直接写。

## 架构铁律

### rtc 内核插件模型（server/internal/rtc/）

- `rtc.Provider` / `rtc.IngestProvider` 接口保持**中性命名**，不得泄漏任何具体实现（LiveKit 等）的语义。
- **业务状态的权威在 store（DB），内核只是现场执行器**：禁言/封禁等管制状态先落库、再向内核尽力传播（`ErrNoParticipant` 不算失败）。新内核不需要理解业务状态，只需会对当前参与者执行操作。
- 配置键按实现命名空间隔离（`livekit_*`、`pion_*`），由实现自带 `ConfigKeys()` 声明；换内核不迁移配置。选择器与枚举值见 `api/dyncfg.go`。
- identity 约定：`{用户名}` 或 `{用户名}-{设备标签/obs}`，归属判断**必须**用 `rtc.MatchesUser`，禁止手写前缀判断。
- `MuteUserAudio` 契约：禁言 = 禁**全部**媒体发布（音频/摄像头/投屏），不只是音频。
- 凭证是短时效入场券（10 分钟 TTL），不是会话生命周期授权；断线重连必须回到签发路径重新判定。

### 入场判定（server/internal/api/admission.go）

一条规则，三个执行点：`admitUser` 是唯一的"谁能进房、能否发布"决策函数，`joinToken`（凭证签发）、`/api/voice`（pion 验票入会）、`/w` POST（WHIP 推流拦截）都调它。新增入口或新增入场约束时**只改这里**，不得在别处散落 `CanJoin`/`IsGagged` 组合。pion 线走一次性入场票（60s、取出即删、防挪用），不做二次判定。

### 动态配置（server/internal/api/dyncfg.go）

优先级：环境变量（锁定，后台只读）> DB settings（`cfg_` 前缀，保存即生效）> 实现声明的默认值。带 `Options` 的键后端校验枚举值。

### 前端（web/）

- 房间页是 Solid（`views/room.tsx`）：状态一律走信号/派生 memo，**禁止**引入第二真相源（手工同步的布尔副本）；引擎产的媒体元素是命令式节点，用 ref 挂载不重建。
- 其余视图保持 vanilla TS；vite-plugin-solid 只处理 `.tsx`。
- CSS 统一在 `src/style.css`，类名复用既有设计系统（ember 主题、三态明暗），选择器注意特异性（button 重置用零特异性 `:where`）。
- 引擎抽象 `engine/types.ts`：新内核实现 `AVEngine` 并在 `engine/index.ts` 注册动态导入（保持代码分割）。

## 已知的坑（改相关代码前必读）

- `websocket.Accept` hijack 后 `r.Context()` 被 net/http 取消：连接生命周期用 `context.WithoutCancel`。
- pionvoice 的 `vroom.mu` 不可重入：持锁时禁止调用 `snapshot()`/`roster()` 等会再抢锁的方法。
- `nhooyr.io/websocket` 不允许并发写：所有出站消息必须走参与者的 send channel + writeLoop。
- 前端引擎重连前必须解绑旧 ws 的 handler（`teardown()`），否则旧连接被判 duplicate 时会误伤新连接。
- 客户端向 ICE-Lite 服务端发 offer 不必等 gathering complete（最多等 1s），部分环境 gathering 永不完成。
- 改挂载进容器的配置文件后 compose 不会自动重启服务，需手动 `docker restart`。

## 验证与发布

- 服务端：`cd server && go build ./... && go vet ./...` 必须通过。
- 前端：`cd web && npx tsc --noEmit && npm run build` 必须通过。
- 行为改动尽量本地起服务验证：`VOICE_PROVIDER=pion STAGE_PROVIDER=none go run ./cmd/server` 零外部依赖。
- 发布：打 `v*` tag 触发 CI（`.github/workflows/release.yml`，原生交叉编译 + 纯装配镜像，无 QEMU）。
