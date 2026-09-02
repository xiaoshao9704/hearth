# 计划：Provider 注册制终审修复（版本游标迁移 + 功能性缺陷）

状态：已实施（§1–§9 全部落地并经两轮复审；§10 冒烟五项全过）。2026-09-02。基线：工作区里未提交的 Provider 注册制实现（spec 见
`docs/superpowers/specs/2026-09-02-provider-registry-design.md`），两份代码审查合并后的修复清单。

取舍原则：只修**功能性缺陷**（升级即坏、运行时错误路径、并发、跨实例串流、数据库语义）；
仅告警类的兼容提示（`LIVEKIT_URL` 以 `/lk` 结尾、`INGRESS_PUBLIC_URL` 残留）**不做**。
迁移从「每次启动都跑」改为**版本游标 + 一次性执行**，以后所有跨版本兼容处理都挂在游标上。

## 1. 版本游标迁移机制（结构性，替代现有 `migrateProviders` 的调用方式）

- `store` 新增两个方法：`MigrationVersion(ctx) (int, error)`、`SetMigrationVersion(ctx, v int) error`，
  存 `settings` 表键 `migration_version`（**不带 `cfg_` 前缀**，不是可配置项，管理后台不展示；
  放 DB 而不是文件——`/data` 是唯一持久化边界）。缺失视为 0。
- `api` 新增 `runMigrations(ctx)`：读游标，按序执行版本号大于游标的步骤，**每步成功后立刻写游标**；
  某步失败则记日志并停止（不写游标，下次启动重试），随后照常 `reloadProviders`。
  `api.New` 里用它替换直接调用 `migrateProviders`。
- **v1** = 现有 `migrateProviders` 的 step 1（导入旧 `cfg_` 键）+ step 2（选择器改写）+ step 3
  （老部署选择器落库），加上下面 1.1–1.3。step 3 因此只跑一次：管理员之后清空选择器恢复默认不会
  再被重启撤销；全新部署首次启动游标为 0 也会跑 v1——这是有意的：v1 的 step 3 只在存在旧来源
  （DB 旧键或 env 探针）时落库，零配置部署仍得到内建默认。
- 1.1 **远端 Bellows 老选择器改写**：DB `cfg_ingest_provider` 为 `bellows` 且本次导入了
  `bellows-remote`（或 env 探到 `BELLOWS_REMOTE_URL`）→ 改写为 `bellows-remote`。
  env 里的 `INGEST_PROVIDER=bellows` 无法改写，由部署前手动改配置解决（见 §8），不加告警。
- 1.2 **ingress 记录归属改写**：与 1.1 同条件下 `UPDATE ingresses SET provider='bellows-remote'
  WHERE provider='bellows'`（老远端形态写入的记录 `Name()` 是 `bellows`），避免升级后
  `getIngress` 因归属不符重建密钥、且 `RevokeRemoteSessions` 落到内建实例空转。
  放在 api 层的 v1 里（store 不知 provider 语义），需要 store 加一个
  `RewriteIngressProvider(ctx, from, to string) error`；现有 `store.migrate()` 里对
  `livekit→livekit-ingress` 的无条件 UPDATE 也移到 v1 统一管理。
- 1.3 **不建僵尸行**：`importLegacy` 在 `ProviderByAlias` 之前先看 env 是否已锁定同名实例
  （复用 `envLockedParams` 的探针），已锁定则只清旧 `cfg_` 键、不 `CreateProvider`——
  否则同 alias 的 DB 行被 env 实例遮蔽、UI 不可见、DELETE 409 不可删，撤 env 后带陈旧参数复活。
- 单测：游标从 0 跑到 1 后再次调用不重复执行（step 3 的落库不会覆盖管理员清空后的值）；
  某步失败游标不前进；1.1/1.2 的改写用例；现有 `TestMigrate*` 改为经 `runMigrations` 触发，
  并用 `maskProviderEnv` 屏蔽真实 `LIVEKIT_*` / `INGRESS_*` / `BELLOWS_*` 环境变量
  （`TestMigrateImportsLegacyCfg` 在设了这些变量的机器上目前会误失败）。

## 2. voiceWS 守卫与 voiceInstance 回落一致（`server/internal/api/voice.go:16`）

`voiceInstance` 对未知/无语音能力的 alias 回落 ember 并签发 ember 入场票 + `/providers/ember/voice`，
但 `voiceWS` 仍按原始选择器串 `dynVal("voice_provider") != "ember"` 拒绝 → 回落场景语音全断 409。
修法：删掉这个守卫（`serveProvider` 已限定 `inst.Type == TypeEmber`，票据本身绑定用户与房间），
或改为 `alias, _ := a.voiceInstance(ctx); if alias != TypeEmber { 409 }`。加一个用例：选择器为未知值时
`/providers/ember/voice` 不返回 409。

## 3. 注册表重建的错误路径与并发（`server/internal/api/providers.go`）

- 3.1 `reloadProviders`：`ListProviders` 出错时**保留旧注册表直接返回**（记日志），不要用只含内建+env
  的表覆盖——否则所有 DB 实例消失直到下次管理操作。
- 3.2 `adminCreateProvider`/`adminUpdateProvider`：`a.instance(alias)` 为 nil 时返回 500「实例加载失败」，
  不解引用；`providerView` 对 nil 入参也返回空视图而不是 panic。
- 3.3 **串行化重建**：加 `reloadMu sync.Mutex`，`reloadProviders` 整个「读 old → 查 DB → 构造 → 换指针」
  持锁；三个管理 handler 的「写 DB → reload」也在同一把锁内完成，避免两次并发 CRUD 交错时后写者把
  过期快照换上去（已删除的实例复活）。`providersMu`（RWMutex）继续只保护读取。
- 单测：注入会失败的 `ListProviders`（用一个包装 store 或关闭 DB 连接）验证旧表保留；
  并发创建+删除后注册表与 DB 一致。

## 4. `livekit_api_url` 默认值在两条新路径上丢失

- 4.1 `server/internal/rtc/livekitrtc/ingress.go` `IngressKeys()` 的 `livekit_api_url` 补
  `Default: apiURLDefault()`（与 `ConfigKeys()` 同名键一致）。
- 4.2 `builtinBellowsCfg`（`providers.go`）对 `livekit_*` 键不要直读 `inst.Params[name]`，改经
  `paramsCfg(inst.Params, a.providerTypeFields(TypeLivekit))(ctx, name)`；建议把每个
  `ProviderInstance` 构造时的 `rtc.ConfigFunc` 存到字段上（如 `Cfg`），内建 bellows 直接委托，
  避免第二套解析路径。顺手把 `builtinBellowsCfg` 里重复的 `stageInstance` + `instance(dynVal)`
  两次选择器解析合成一次。
- `Ingress.Enabled` 与 bellows 的 `Enabled` 不必再改（Default 补上后为空的情况只剩显式配错）。
- 单测：env 只设 `LIVEKIT_URL`+KEY+SECRET 不设 `LIVEKIT_API_URL` 时，`livekit-ingress` 实例与内建
  bellows 读到的 `livekit_api_url` 均等于 `apiURLDefault()`。

## 5. 选择器回落不得触发推流端点的删除重建（`api.go` `getIngress`）

`ingestInstance` 对无推流能力的 alias（如 env 残留 `INGEST_PROVIDER=livekit`）静默回落内建 bellows，
`getIngress` 把「记录归属 ≠ 生效 alias」当作管理员切换内核，走 `deleteOldEndpoint` 删掉上游端点并
重建密钥——用户没有做任何选择就丢了推流地址，改正 env 后再丢一次。
修法：`ingestInstance` 增加返回值 `fellBack bool`（或返回 `*ProviderInstance` 并附带标记）；
`getIngress`/`resetIngress` 在回落状态下**不做**归属自愈，只记一条日志并按记录原归属实例返回地址
（找不到归属实例时返回 503「推流入口配置无效」）。同样的保护加在 `createIngress`：回落状态下不用回落
实例创建新端点。单测：env `INGEST_PROVIDER=livekit` + 记录 provider=`livekit-ingress` 时
`getIngress` 不调用 `DeleteEndpoint`。

## 6. WHIP 判定校验记录归属与路径 alias 一致（`proxy.go` `admitWhipRemote`）

推流密钥是全局命名空间，任何有效 key 经任一 `bellows-remote` 实例的路径都能过判定、用该实例的
secret 签 grant，把流发进另一套 LiveKit 的同名房间。修法：`ingressOwner` 带回记录的 `Provider`，
`admitWhipRemote` 在 `admitUser` 之前校验 `rec.Provider == inst.Alias`，不符 404「推流密钥与该入口不匹配」。
进程内 bellows 与 livekit-ingress 的 fail-open 路径同样加此校验（`canPublishByStreamKey` 传入 alias）。
spec 的「切选择器不炸存量 OBS」不受影响：记录的 provider 就是签发地址里的 alias。

## 7. MySQL 下 `UpdateProviderParams` 误报不存在（`server/internal/store/providers.go`）

`RowsAffected()==0 → ErrNotFound` 在 MySQL 上错误：DSN 未加 `clientFoundRows=true`，驱动只统计**实际改变**
的行，`updated_at` 为秒精度，同一秒内重复保存相同 params → 0 行 → 后台 404「实例不存在」。
修法：`store.Open` 的 MySQL DSN 追加 `&clientFoundRows=true`（让全库 UPDATE 语义统一为「匹配行数」，
顺带保护其他用 RowsAffected 判存在的地方），并把 `UpdateProviderParams` 改为先 `ProviderByAlias`
判存在再 UPDATE、不再从 RowsAffected 推断。单测（sqlite 跑不出差异）：至少覆盖「相同参数重复保存返回 nil」。

## 8. 隐私与注释（提交前必做）

- `docs/superpowers/plans/2026-09-02-provider-registry.md` 第 9、15、760 行的本机工具链路径
  （`~/.proto/bin`）删除，构建命令一律写 `cd server && go build ./...`；全文再 grep 一遍
  `/Users/`、`~/`、`.proto`。
- `server/internal/api/admin_providers_test.go:62` 的「见 Task 5 简报」、`providers_test.go:19` 的
  「签名按现状对齐」——执行过程留言，删掉或改成描述约束的注释。
- `providers.go` 文件头「从 X 改为 Y」、`proxy.go` 里「原 /api/voice 处理器，路径搬家」「原 /w 处理器」
  这类改动说明改为描述现状。
- `gofmt -w server/internal/rtc/bellows/grant_test.go`。
- `.superpowers/sdd/` 目录不入库（已 gitignore，确认一下）。

## 9. 顺手清理（可选，改动小且与上面同区域）

- `rtc.IngestProvider.PublicBase` 三个实现恒为空：删接口方法与实现，`ingressURL(r, alias, key)` 去掉
  再次解析实例的分支。
- `config.go` 里 `LiveKitURL/LiveKitKey/LiveKitSecret/LiveKitAPIURL/IngressPublicURL/IngressUpstreamURL`
  六个无人读取的字段与 `deriveAPIURL` 删除。
- `joinToken`/`setGag` 的 `combined` 判定改用 `voiceInstance`/`stageInstance` 已返回的 alias，不再重读选择器。
- pg 的 `providers` 表 `TIMESTAMP` → `TIMESTAMPTZ`；`ListProviders` 排序加 `, alias`；
  `proxy_test.go:13` 注释改为「无 token → 401」。

## 10. 验证

- `cd server && go build ./... && go vet ./... && go test -race -count=1 ./...`；`gofmt -l .` 为空。
- `cd web && npx tsc --noEmit && npm run build`。
- 两进程冒烟（hearth + `cmd/bellows`，与 v0.3.3 做法相同）：
  1. 先用 v0.3.3 形态的数据起一次（`cfg_ingest_provider=bellows` + `cfg_bellows_remote_url` + 一条
     provider=`bellows` 的 ingress 记录），升级启动后：游标=1、选择器变 `bellows-remote`、
     记录 provider 变 `bellows-remote`、`/api/ingress` 返回原 key 不重建；再重启一次游标不变、无重复日志。
  2. 未知选择器值下进房拿到 ember 票，`/providers/ember/voice` 不 409。
  3. env `INGEST_PROVIDER=livekit` + 已有 livekit-ingress 记录：打开推流设置不删端点、不换 key。
  4. 两个 bellows-remote 实例，用 A 的 key 推 B 的路径 → 404。
  5. 并发 10 次创建/删除实例后 `GET /api/admin/providers` 与 DB 一致。

## 附：统一 ORM 的评估（本计划不做，单独立项）

本次暴露的三处方言坑（MySQL `RowsAffected` 语义、pg `TIMESTAMP` vs `TIMESTAMPTZ`、三份 DDL 手工同步 +
`compat` ALTER 列表）都来自 store 层「手写 SQL × 三方言」的结构，值得评估统一框架，但**不要与注册制修复
混在一起**——store 约 10 张表、60 条查询，换框架是全量重写，应在本计划合入并发布一版之后单独做。

选型倾向（按项目原则：纯 Go 无 cgo、最小抽象、SQL 可读）：
- **Bun**（SQL-first 查询构造器 + 迁移）：dialect 层统一占位符/时间类型/RowsAffected；建表用 struct tag
  一份定义三方言生成；自带版本化迁移，正好接管本计划的游标机制；sqlite 走 `sqliteshim`（底层 modernc，
  无 cgo），pg 走 pgx/pgdriver，mysql 走 go-sql-driver——与现有依赖同源。查询仍是显式 SQL 风格，
  不改变「单次使用的代码直接写」的习惯。首选。
- **GORM**：AutoMigrate 省事但魔法多（关联、钩子、软删除默认行为），sqlite 官方驱动依赖 cgo（需换
  glebarez/sqlite），与仓库风格相悖。不推荐。
- **ent**：schema 代码生成 + Atlas 迁移，类型最强但仪式感重，对这个规模过重。不推荐。
- **sqlc**：SQL 编译成类型安全 Go，但 SQL 本身仍按方言各写一份，解决不了三份同步的问题。不适用。

若立项：先只迁 DDL/迁移到 Bun（三份 DDL 合一、compat ALTER 变成版本化迁移文件），查询按包逐步替换；
`s.q()` 占位符改写在全部迁完后删除。验收标准是三方言各起一遍完整冒烟（sqlite 本地、mysql/pg 容器）。

## 部署前手动改配置（本次不做代码兼容的部分）

- hearth 侧 compose：`INGEST_PROVIDER=bellows` → `INGEST_PROVIDER=bellows-remote`
  （`BELLOWS_REMOTE_URL`/`BELLOWS_SHARED_SECRET` 保留，自动合成 env 锁定实例）。
- Bellows 侧 compose：删除 `HEARTH_URL`（通行证模型不再回调）。
- 两端同一镜像版本一起升级；升级后进设置页确认推流地址仍是 `/providers/bellows-remote/w/…` 且密钥未变。
