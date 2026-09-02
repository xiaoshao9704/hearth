// baseline 迁移：对每张表 CreateTable().IfNotExists() + 建消息索引 + compat 加列（最后一次执行）。
// 存量库：表已存在、列已齐，baseline 是空操作，仅登记到 bun_migrations；
// 新库：全套由模型生成。以后每次 schema 变更加一个新迁移文件（NNNNN_name.go），不改本文件。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Migrations 是 store 的 schema 迁移集合；api 层的数据语义迁移走 settings.migration_version 游标，不进这里。
var Migrations = migrate.NewMigrations()

func init() {
	Migrations.MustRegister(baselineUp, baselineDown)
}

// baselineModels 建表顺序保持被引用表在前（与旧 DDL 一致；模型不声明外键，仅影响阅读）。
var baselineModels = []any{
	(*userRow)(nil), (*sessionRow)(nil), (*channelRow)(nil), (*messageRow)(nil),
	(*deviceRow)(nil), (*ingressRow)(nil),
	(*channelBanRow)(nil), (*channelGagRow)(nil), (*channelMemberRow)(nil),
	(*inviteRow)(nil), (*settingRow)(nil), (*providerRow)(nil),
}

func baselineUp(ctx context.Context, db *bun.DB) error {
	for _, m := range baselineModels {
		if _, err := db.NewCreateTable().Model(m).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	// mysql 不支持 CREATE INDEX IF NOT EXISTS，老库已带该索引，重复错误吞掉
	if _, err := db.NewCreateIndex().Model((*messageRow)(nil)).
		Index("idx_messages_channel").Column("channel_id", "id").
		IfNotExists().Exec(ctx); err != nil && !isDuplicateErr(err) {
		return err
	}
	// compat 老库兼容加列，此处是最后一次执行；此后不再新增条目，schema 变更走新迁移文件
	for _, stmt := range []string{
		`ALTER TABLE channels ADD COLUMN invite_only INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ingresses ADD COLUMN provider VARCHAR(32) NOT NULL DEFAULT 'livekit'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isDuplicateErr(err) {
			return err
		}
	}
	return nil
}

// baseline 不提供回滚：回退靠回滚二进制，schema 上只多两张 bun_* 表。
func baselineDown(context.Context, *bun.DB) error { return nil }
