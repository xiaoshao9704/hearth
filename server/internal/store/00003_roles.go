// 迁移 00003：分层权限与访客的 schema——角色/访客列、会话设备绑定、邀请扩列。
// 只加列不改旧列；新库由 baseline 模型直接带出这些列，这里的 ALTER 会为「列已存在」，
// 与 baseline compat 加列同口径吞掉重复错误。数据语义迁移（is_admin→role、owner 迁入
// channel_members 等）走 api 层 settings.migration_version 游标，不进这里。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(rolesUp, rolesDown)
}

func rolesUp(ctx context.Context, db *bun.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE users ADD COLUMN role VARCHAR(16) NOT NULL DEFAULT 'user'`,
		`ALTER TABLE users ADD COLUMN expires_at TIMESTAMP NULL`,
		`ALTER TABLE users ADD COLUMN invite_id BIGINT NULL`,
		`ALTER TABLE channel_members ADD COLUMN role VARCHAR(16) NOT NULL DEFAULT 'member'`,
		`ALTER TABLE sessions ADD COLUMN device_id VARCHAR(32) NOT NULL DEFAULT ''`,
		`ALTER TABLE invites ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'register'`,
		`ALTER TABLE invites ADD COLUMN channel_id BIGINT NULL`,
		`ALTER TABLE invites ADD COLUMN role VARCHAR(16) NOT NULL DEFAULT 'user'`,
		`ALTER TABLE invites ADD COLUMN guest_ttl_sec INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE invites ADD COLUMN allow_guest INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isDuplicateErr(err) {
			return err
		}
	}
	return nil
}

// 与 baseline 同口径：不提供回滚，回退靠回滚二进制。
func rolesDown(context.Context, *bun.DB) error { return nil }
