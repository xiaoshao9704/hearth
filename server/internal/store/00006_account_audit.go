// 迁移 00006：审计日志表 + 会话行补三列。
// audit_log 记录管制动作（禁言/踢出/封禁/改频道角色…）的事实，不承载权限判定；
// target_uid / channel_id 允许为空（不针对某人或不属于某频道的动作）。
// sessions 的 user_agent/created_at/last_seen 是「我的登录设备」列表要展示的三项，
// created_at 在 baseline 模型里已有，存量库缺列时这里补上——只加列不改旧列。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"

	"github.com/uptrace/bun"
	bundialect "github.com/uptrace/bun/dialect"
)

func init() {
	Migrations.MustRegister(accountAuditUp, accountAuditDown)
}

func accountAuditUp(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*auditRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	// 两条索引各服务一个查询：at 服务保留策略的清理，(channel_id, id) 服务按频道翻页。
	// mysql 不支持 CREATE INDEX IF NOT EXISTS，重复错误吞掉。
	if _, err := db.NewCreateIndex().Model((*auditRow)(nil)).
		Index("idx_audit_log_at").Column("at").
		IfNotExists().Exec(ctx); err != nil && !isDuplicateErr(err) {
		return err
	}
	if _, err := db.NewCreateIndex().Model((*auditRow)(nil)).
		Index("idx_audit_log_channel").Column("channel_id", "id").
		IfNotExists().Exec(ctx); err != nil && !isDuplicateErr(err) {
		return err
	}
	tsType := "TIMESTAMP"
	switch db.Dialect().Name() {
	case bundialect.PG:
		tsType = "TIMESTAMPTZ"
	case bundialect.MySQL:
		tsType = "DATETIME"
	}
	for _, stmt := range []string{
		`ALTER TABLE sessions ADD COLUMN user_agent VARCHAR(255) NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN created_at ` + tsType + ` NULL`,
		`ALTER TABLE sessions ADD COLUMN last_seen ` + tsType + ` NULL`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isDuplicateErr(err) {
			return err
		}
	}
	return nil
}

// 与 baseline 同口径：不提供回滚，回退靠回滚二进制。
func accountAuditDown(context.Context, *bun.DB) error { return nil }
