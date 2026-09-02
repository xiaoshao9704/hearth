# 计划：store 层迁移到 Bun（三方言统一）

状态：已实施（B1–B6 全过，三方言矩阵容器实测 + 存量库升级 + 冒烟全绿，终审可合入）。2026-09-02。

## 动机

store 是「手写 SQL × 三方言（sqlite / MySQL / Postgres）」，方言分叉散在 6 处，本轮审查暴露的坑全部来自这里：
MySQL `RowsAffected` 只算实际改变的行、pg 的 `TIMESTAMP` 与 `TIMESTAMPTZ` 混用、三份 DDL 手工同步、
「老库兼容加列」靠吞掉 duplicate column 错误。目标：**一份表定义出三方言 DDL、占位符/自增主键/upsert/时间类型
由方言层统一、schema 变更走版本化迁移**，同时保持纯 Go 无 cgo、SQL 可读、`单次使用的代码直接写` 的仓库风格。

## 现状盘点（`server/internal/store/`）

- 文件：`store.go` 850 行、`admin.go` 379 行、`providers.go` 94 行；导出方法 63 个；语句 61 条；无事务。
- 表 12 张：users、sessions、channels、messages、devices、ingresses、channel_bans、channel_gags、
  channel_members、invites、settings、providers；索引 1 个（`idx_messages_channel`）。
- 方言分叉点：
  1. `dialect.rebind`：`?` → pg `$n`（每条查询经 `s.q()`）；
  2. `insertID`：pg 走 `RETURNING id`，其余 `LastInsertId`；
  3. `dialect.ignore`：`INSERT IGNORE` / `ON CONFLICT DO NOTHING` / `INSERT OR IGNORE`；
  4. `RecordDevice` 的 upsert：`ON DUPLICATE KEY UPDATE` vs `ON CONFLICT … DO UPDATE`；
  5. `migrate()`：`sqliteDDL`/`mysqlDDL`/`pgDDL` 三份 + `compat` ALTER 列表（吞 duplicate column）；
  6. `parseDBURL`：驱动名与 DSN（mysql 需 `parseTime=true&clientFoundRows=true`）。
- 驱动：`modernc.org/sqlite`（无 cgo）、`github.com/jackc/pgx/v5`（`pgx` stdlib 驱动名）、
  `github.com/go-sql-driver/mysql`。**全部保留**，Bun 只在 `*sql.DB` 之上包一层。

## 选型

`github.com/uptrace/bun`（pin 最新 v1.2.x）+ 同模块的 `dialect/sqlitedialect`、`dialect/pgdialect`、
`dialect/mysqldialect`、`migrate`。不引入 Bun 自带的 pgdriver/sqliteshim——现有三个驱动已是纯 Go 且经过生产，
`bun.NewDB(sqldb, dialect)` 直接包现有 `*sql.DB`。

Bun 解决每个分叉点的方式：
| 分叉点 | Bun |
|---|---|
| 占位符 | 统一 `?`，方言层改写；`s.q()` 与 `rebind` 删除 |
| 自增主键 | 模型字段 `bun:"id,pk,autoincrement"`：pg 自动 `RETURNING`，mysql/sqlite 自动回填 `LastInsertId`；`insertID` 删除 |
| 重复忽略 | `NewInsert().Ignore()` 三方言统一；`dialect.ignore` 删除 |
| upsert | `NewInsert().On("CONFLICT (user_id, device_id) DO UPDATE")` 对 sqlite/pg 统一，mysql 仍需 `On("DUPLICATE KEY UPDATE")`——保留**两处**方言判断（`RecordDevice` 与 `SetSetting`，原来是三段整句 SQL） |
| 时间类型 | 模型 `time.Time` 由方言映射（pg `timestamptz`、mysql `datetime`、sqlite `timestamp`），`TIMESTAMP`/`TIMESTAMPTZ` 不一致问题消失 |
| DDL | 一份模型定义，`NewCreateTable().Model(&T{}).IfNotExists()` 出三方言；三份 DDL 列表删除 |
| 迁移 | `bun/migrate` 版本化 Go 迁移，自带 `bun_migrations` / `bun_migration_locks` 表 |

不选 GORM（魔法多、sqlite 官方驱动依赖 cgo）、ent（代码生成 + Atlas，对 12 张表过重）、sqlc（SQL 仍按方言各写一份）。

## 与 `migration_version` 游标的关系

`plan-provider-registry-fixes.md` §1 在 api 层加的 `settings.migration_version` 游标负责**数据语义迁移**
（导入旧 cfg_ 键、改写选择器等，需要 api 包的类型），Bun 迁移负责**schema**。两者并存、职责不交叉：
启动顺序 = `store.Open`（Bun schema 迁移）→ `api.New`（游标数据迁移）。不要把 api 语义塞进 store 的迁移文件
（会引入 store→api 的循环依赖）。

## 模型定义策略

- 现有导出类型（`User`、`Channel`、`Ingress`、`Invite`、`ProviderRecord`、`Device`…）已与表一一对应，
  直接补 `bun` tag（`bun:"table:users"` + 字段 tag），json tag 保留；API 响应形状与 DB 模型在本项目里本来就是同一个。
- 列不在导出类型里的（如 `users.password_hash`、`sessions.token/expires_at`、`settings.k/v`、
  三张 `channel_*` 关系表）定义**未导出**的行结构体（`userRow`、`sessionRow`、`settingRow`、`channelBanRow`…），
  只用于建表与写入，读取仍可 Scan 到现有类型。
- 外键：现有 sqlite/pg DDL 带 `REFERENCES`，mysql 不带。模型上不声明外键（Bun 的 `rel` 只用于查询联结），
  与 mysql 现状对齐，避免三方言行为差异；删除级联本来就在应用层做。
- 索引：`idx_messages_channel` 用 `NewCreateIndex().IfNotExists()` 在 baseline 迁移里建。

## 迁移策略（对存量库零风险）

- `00001_baseline.go`（落盘在 `server/internal/store/` 包根，与 store 同包以便用未导出行结构体；
  bun/migrate 从文件名解析迁移名，文件名不可改）：对每个模型 `CreateTable().IfNotExists()`，然后执行现有 `compat` 四条
  ALTER **最后一次**（仍吞 duplicate column）。存量库：表已存在、列已齐，baseline 是空操作，仅登记到
  `bun_migrations`；新库：全套由模型生成。此后 `sqliteDDL`/`mysqlDDL`/`pgDDL`/`compat` 全部删除。
- 以后每次 schema 变更一个新迁移文件（`NewAddColumn` / `NewCreateTable` / 原生 SQL），不再改 baseline。
- `Open` 流程：`sql.Open` → `bun.NewDB` → `migrate.NewMigrator(db, migrations).Init` → `Migrate`；
  迁移失败即启动失败（与现在 `migrate()` 返回错误的语义一致）。
- 存量库校验（验收项）：拿一份 v0.3.x 的 sqlite 库副本升级，前后 `.schema` 对比只多两张 `bun_*` 表；
  mysql/pg 用容器各建一次旧版 schema 再升级，`SHOW CREATE TABLE` / `\d` 对比。
- 模型生成的 DDL 与手写 DDL 在**新库**上允许有类型措辞差异（如 `INTEGER` vs `BIGINT`、`TEXT` vs `VARCHAR`），
  以三方言冒烟通过为准，不追求字面一致。

## 查询改写策略（分两阶段，第一阶段就能删掉全部方言分叉）

**阶段 1（机械替换，目标：删除 `s.q`/`insertID`/`ignore`/三份 DDL）**
- 读：`db.QueryRowContext(ctx, s.q(sql), args...).Scan(...)` → `db.NewRaw(sql, args...).Scan(ctx, &dst...)`，
  SQL 文本不动（`?` 占位符 Bun 自行改写）。多行同理 `NewRaw(...).Scan(ctx, &slice)`。
- 写：`ExecContext(ctx, s.q(sql), args...)` → `db.NewRaw(sql, args...).Exec(ctx)`。
- 插入取主键：`insertID(...)` → `db.NewInsert().Model(&row).Exec(ctx)` 后读 `row.ID`。
- `ignore(...)` → `NewInsert().Model(&row).Ignore().Exec(ctx)`。
- `RecordDevice` → `NewInsert().Model(&row).On(<按方言二选一>).Set("last_seen = CURRENT_TIMESTAMP, tag = EXCLUDED.tag")`
  （mysql 用 `VALUES(tag)`）；`SetSetting` 的 upsert 同理。全 store 只保留这两处方言判断，各写在对应函数内。
- `scanChannel`/`scanInvite` 这类共享 scan 帮助函数保留（`NewRaw.Scan` 支持 `Scan(ctx, &a, &b, ...)` 位置扫描）。
- `RowsAffected` 判存在的地方（`UpdateProviderParams` 等）：修复计划已改为先查存在，Bun 不改变驱动语义，
  mysql DSN 的 `clientFoundRows=true` 保留。

**阶段 2（可选，逐包）**：把简单查询改成构造器（`NewSelect().Model(&u).Where("username = ?", name)`），
只在可读性更好时做；复杂 SQL（消息分页、名单联结）保留 `NewRaw`。仓库原则是最小改动，阶段 2 不设截止。

## 测试

- `providers_test.go` 现有用例保留；新增 `store_test.go` 的**方言矩阵**：`openTestStore(t)` 默认 sqlite 临时文件；
  设了 `HEARTH_TEST_MYSQL_URL` / `HEARTH_TEST_PG_URL` 时同一套用例再跑一遍（未设则 `t.Skip`）。
- 用例覆盖每个方言分叉点：自增回填、`Ignore` 重复插入、`RecordDevice` 二次调用刷新 `last_seen`、
  时间字段往返（`created_at` 扫回 `time.Time` 且非零）、baseline 在已建库上重复运行为空操作。
- CI 暂不起 mysql/pg 服务（release 工作流保持轻量）；本地用 docker 起两个容器跑一次矩阵作为验收，命令写进
  `server/README.md`。

## 步骤

1. `go get github.com/uptrace/bun@latest` 及三个 dialect、`migrate` 子包；`go mod tidy`；确认
   `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` 仍通过（Bun 纯 Go）。
2. 模型与 baseline 迁移：补 tag / 未导出行结构体，写 `00001_baseline.go`，`Open` 接上 Migrator；
   此时仍保留旧 `migrate()` 不调用，跑一次 sqlite 存量库升级校验。
3. 阶段 1 机械替换：按文件顺序 `providers.go` → `admin.go` → `store.go`，每个文件替换完跑一次矩阵测试。
4. 删除 `dialect.rebind`/`ignore`、`s.q`、`insertID`、三份 DDL、`compat`、旧 `migrate()`；`dialect` 只剩名字
   （给 `RecordDevice` 和 DSN 用）。
5. 文档：`server/README.md` 的存储段与验收命令；CLAUDE.md「已知的坑」补一条「schema 变更只加迁移文件，
   不改 baseline；api 语义迁移走 migration_version 游标」。

## 验收

- `cd server && go build ./... && go vet ./... && go test -race -count=1 ./...`；`gofmt -l .` 为空。
- 方言矩阵测试在 sqlite + mysql 容器 + pg 容器三者上全过。
- 存量库升级校验（上文）三方言各一次；升级后完整冒烟：登录、建频道、进房令牌、推流密钥、管理后台实例 CRUD。
- `grep -n "s\.q(\|insertID\|sqliteDDL\|mysqlDDL\|pgDDL" internal/store/` 为空。

## 风险与回退

- Bun 对 mysql 的 `Returning` 不支持：靠 `autoincrement` tag 回填，插入必须走 `NewInsert().Model`，不能用 `NewRaw`。
- 模型生成的列类型与旧手写 DDL 不同只影响新库；存量库由 `IfNotExists` 保护。若发现新库上某列类型导致行为差异
  （如 mysql `TEXT` 不能带 DEFAULT），在模型 tag 里显式指定 `type:varchar(32)`。
- 回退：整个改动是一个提交序列，schema 上只多两张 `bun_*` 表，回滚二进制即可，无需回滚数据库。
