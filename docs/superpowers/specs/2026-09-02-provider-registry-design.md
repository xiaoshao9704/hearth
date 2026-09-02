# 设计：Provider 注册制（实例化内核管理）

状态：设计已确认，待写实施计划。2026-09-02。基线：main 含 Bellows 通行证模型（plan-bellows-grant 已实施）。

## 动机

现状内核是静态三选一：`voice_provider`/`stage_provider`/`ingest_provider` 三个选择器各选一个固定实现，
实现的连接参数（`livekit_*`、`ingress_*`、`bellows_*`）是全局配置键，全站只有一套 LiveKit、一套 ingress、
一套远端 Bellows。目标改成**注册制**：LiveKit / LiveKit Ingress / 远程 Bellows 三种外部服务可在 DB 里
无限注册实例（alias 命名），选择器从「选实现」变成「选实例」；内建语音（Ember）与内建推流（进程内
Bellows）作为默认实例排最前，零外部依赖开箱即用。

本期只做**全局选择**（每槽位一个生效实例）；频道级覆盖的数据模型预留（alias 引用而非实现名），能力后延。

## 实例模型

### 三类来源（读取时合成，内建与 env 锁定不落库）

| 来源 | alias | 可改性 |
|---|---|---|
| 内建（builtin） | `ember`、`bellows` | 不可改不可删，排最前，是默认值 |
| env 锁定（locked） | 与类型同名：`livekit` / `livekit-ingress` / `bellows-remote` | 后台只读；每类至多一条 |
| DB 注册 | 用户自定 | 无限增删改；alias 创建后不可改名 |

env 锁定实例的判定与现状一致：`LIVEKIT_API_URL` 系列 env 存在 → 合成 alias=`livekit`；
`INGRESS_UPSTREAM_URL` → `livekit-ingress`；`BELLOWS_REMOTE_URL` → `bellows-remote`。

### store 新表 `providers`

- `alias` TEXT PRIMARY KEY —— 实例名，`^[a-z0-9][a-z0-9-]{0,31}$`；不得与内建/env 锁定冲突。
- `type` TEXT —— `livekit` / `livekit-ingress` / `bellows-remote`（内建类型不落库）。
- `params` TEXT —— JSON 连接参数；读取 API 时 secret 字段掩码不回显。
- `created_at` / `updated_at`。

### 类型 × 槽位能力与 params

| 类型 | 语音槽 | 舞台槽 | 推流槽 | params |
|---|---|---|---|---|
| `ember`（内建） | ✓ | – | – | 无 |
| `bellows`（内建进程内） | – | – | ✓ | 无（`bellows_udp_port`/`bellows_public_ip` 仍为全局键：进程内网络基建只有一套） |
| `livekit` | ✓ | ✓ | – | `api_url`, `api_key`, `api_secret` |
| `livekit-ingress` | – | – | ✓ | `livekit_api_url`, `livekit_api_key`, `livekit_api_secret`（端点管理走 Twirp API）, `ingress_upstream_url`（WHIP 上游） |
| `bellows-remote` | – | – | ✓ | `remote_url`, `shared_secret` |

注意：`livekit` 类型在新模型里**只管语音/舞台**，推流面拆出为 `livekit-ingress`（现状是
`livekitrtc.Provider` 一身二任，读 `ingress_*` 全局键，需拆分）。

### 实例即对象

- api 持有 `map[alias]实例对象`（RWMutex），注册/改/删时重建对应对象；`rtc.Provider` /
  `rtc.IngestProvider` 等中性接口零改动（对象构造时把实例 params 包成 `rtc.ConfigFunc` 注入）。
- 内建 bellows 发流的目标 LiveKit = 当前舞台线生效的 `livekit` 实例（与现状「读全局 livekit_*」
  语义对齐）；舞台槽为 `none` 或无可用 livekit 实例时，内建 bellows 的 `Enabled` 为 false。
- 选择器 `voice_provider`/`stage_provider`/`ingest_provider` 的值改为实例 alias；
  **默认值 `ember` / `none` / `bellows`**（内建优先）。选择器仍走 dyncfg（env > DB > 默认），
  但 Options 不再静态声明，改由管理后台吃实例列表渲染；保存时校验「实例存在 + 能力匹配槽位」。
- 旧选择器值/未知 alias 的回落：语音落 `ember`，推流落 `bellows`，舞台落 `none`（不再落 livekit——
  内建优先的默认语义）。

## 反代与接入路径

统一形状 `/providers/{alias}/...`，按 alias 分发：

- `livekit` 实例：`/providers/{alias}/rtc/*` → 反代到该实例 `api_url`（剥 `/providers/{alias}` 前缀，
  等价现状 `/lk` 的语义）。
- `ember`（内建）：`/providers/ember/voice` → 进程内 WS 信令（原 `/api/voice` 删除；一次性入场票
  机制不变，票拼在新 URL query）。
- 推流三类：`/providers/{alias}/w[/{key}]` → WHIP。`/w` 段保留：只剥 `/providers/{alias}` 前缀，
  剩余路径原样透传上游（livekit-ingress 上游期望 `/w/{key}`；bellows-remote 上游的 `/w` 上还挂着
  会话撤销端点 `/w/sessions/{key}`），hearth 不做路径改写。内建 bellows 由 hearth 进程内直接处理。
- 旧路径 `/lk`、`/w`、`/api/voice` 全部删除，不留兼容（项目惯例：跨版本兼容只留一个版本，
  本次直接换）。

### 命名论证（为何不与Provider 管理接口冲突）

- 管理 CRUD 在 `/api/admin/providers/*`（`/api` 前缀），代理在 `/providers/{alias}/*`：第一级路径段
  不同，结构上不可能冲突。
- alias 限定单段 `[a-z0-9-]`，不会与 `/api/*`、聊天 WS（`/api/chat`）、前端静态路由碰撞；chi 中
  `/providers/{alias}/*` 比静态托管 `/*` 更具体，优先命中。
- 前端无感：joinToken 的 credentials.url、`/api/ingress` 的推流地址都由服务端按生效实例 alias 拼好。

### WHIP 按路径 alias 裁决

OBS 推给哪个实例的地址，就由哪个实例做入场判定、签 grant、反代（不按当前全局选择器）。实例还在，
已发出的推流地址就持续有效，切选择器不炸存量 OBS。PATCH/DELETE 会话收尾先按 `HasSession` 扫
进程内实例，再落到路径 alias 实例反代。`ingress` 记录的 provider 字段改存实例 alias。

## 迁移（一次性，启动时做，只保留一个版本）

- DB 旧键导入：`cfg_livekit_*` → 若无 alias=`livekit` 实例则导入为 DB 实例（type=livekit）；
  `cfg_ingress_*` → `livekit-ingress`；`cfg_bellows_remote_url`/`cfg_bellows_shared_secret` →
  `bellows-remote`。导入后删除旧 `cfg_` 键。
- 选择器改写：`voice/stage` 的 `livekit`/`ember` 同名兼容；**`ingest_provider=livekit` 改写为
  `livekit-ingress`**（旧值指 livekit 的 ingress 面，新模型 alias=livekit 无推流能力）；
  ingress 存量记录 provider 字段 `livekit`→`livekit-ingress`，`bellows` 同名。
- env 场景无需迁移（锁定实例自动合成）。

## 管理 API 与后台 UI

- `GET /api/admin/providers` —— 列表（内建 + env 锁定 + DB；params 掩码；带 caps/locked/builtin 标记）。
- `POST /api/admin/providers` —— 注册 `{type, alias, params}`；alias 合法性/冲突校验。
- `PUT /api/admin/providers/{alias}` —— 改 params（locked/builtin → 409）。
- `DELETE /api/admin/providers/{alias}` —— 删除（locked/builtin → 409；仍被某槽位选择器引用 → 409
  并提示先切换）。
- 管理后台新增「服务实例」区块：实例列表 + 按类型的注册表单（类型决定 params 字段）；三个选择器
  下拉吃实例列表，内建排最前。

## rtc 层改动点

- `livekitrtc` 拆成两个构造：`NewVoice`（voice/stage，params api_url/key/secret）与 `NewIngress`
  （ ingest，params upstream_url）；`ingress_public_url` 键删除（PublicBase 恒为空，地址由接入层按
  `/providers/{alias}/w/` 推导——与 bellows 现状一致）。
- `bellows`：远端形态改造为按实例构造（params remote_url/shared_secret）；进程内形态保留全局端口键。
  grant 机制（plan-bellows-grant）不变，仅配置来源从全局键换成实例 params。
- `ember`：无连接参数，仅信令路径搬家。
- 各实现的 `ConfigKeys()` 全局键收敛：env 锁定实例的参数键不再出现在管理后台表单（改为只读实例
  展示）；`bellows_udp_port`/`bellows_public_ip` 保留为全局键。

## 测试与验证

- store：`providers` 表 CRUD 单测。
- api：实例注册表（增删改 → 对象重建、alias 冲突、locked/builtin 保护、删除引用保护）、proxy 按
  alias 分发三类（livekit 信令 / ember WS / WHIP）、WHIP 按 alias 裁决（404/403/签发）、迁移单测。
- grant 流现有单测在 bellows-remote 实例化后保持通过（改造相应构造）。
- `cd server && go build ./... && go vet ./... && go test -race ./...`；前端 `npx tsc --noEmit && npm run build`。
- 冒烟：两进程（hearth + cmd/bellows），注册 bellows-remote 实例，OBS 推 `/providers/{alias}/w/{key}`
  → 201；假 key 404；改 offer 重发同 grant 401；重置密钥远端掐会话；ember 语音 `/providers/ember/voice`
  连通。

## 风险与取舍

- 默认选择器改为内建（ember/none/bellows）：对现有 livekit 部署是行为变化，靠迁移（旧选择器值
  显式落库过的不受影响；从未设置的站点本来就该零依赖起步）+ 发布说明覆盖。
- 旧 `/lk`、`/w`、`/api/voice` 删除：升级后 OBS 已配置地址与旧前端立即失效，发布说明注明；
  与项目「不保留跨版本兼容」惯例一致。
- 内建 bellows 绑定舞台线 livekit 实例是隐式耦合，文档与后台 Hint 写明；未来频道级覆盖时再显式化。
