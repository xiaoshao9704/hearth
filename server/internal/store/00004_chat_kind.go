// 迁移 00004：聊天消息加类型与文件元数据两列——kind（text/file）与 meta（file 卡片的
// JSON {name,mime,size}）。只加列不改旧列：旧行 kind 由 DEFAULT 'text' 兜底，meta 留空。
// 文件字节不入库（走 LiveKit 数据通道经 SFU 扇出），这里只存"有过这么一个文件"的卡片。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(chatKindUp, chatKindDown)
}

func chatKindUp(ctx context.Context, db *bun.DB) error {
	// kind 用 VARCHAR 而非 TEXT：mysql 的 TEXT 列不能带 DEFAULT。
	for _, stmt := range []string{
		`ALTER TABLE messages ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'text'`,
		`ALTER TABLE messages ADD COLUMN meta TEXT NULL`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isDuplicateErr(err) {
			return err
		}
	}
	return nil
}

// 与 baseline 同口径：不提供回滚，回退靠回滚二进制。
func chatKindDown(context.Context, *bun.DB) error { return nil }
