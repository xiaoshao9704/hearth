// 迁移 00005：聊天消息第二版数据模型——messages 加引用回复（reply_to）与软删标记
// （deleted_at），新建 message_reactions 表（每人每条每种表情至多一行，主键即去重约束）。
// 软删只清内容不删行：id 是数据线广播与前端去重的键，行没了对端就补不齐"这条被撤回了"。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(chatV2Up, chatV2Down)
}

func chatV2Up(ctx context.Context, db *bun.DB) error {
	// deleted_at 用 TIMESTAMP 而非 TEXT：写进去的是 CURRENT_TIMESTAMP，postgres 不做隐式转文本。
	for _, stmt := range []string{
		`ALTER TABLE messages ADD COLUMN reply_to BIGINT NULL`,
		`ALTER TABLE messages ADD COLUMN deleted_at TIMESTAMP NULL`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isDuplicateErr(err) {
			return err
		}
	}
	if _, err := db.NewCreateTable().Model((*messageReactionRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	return nil
}

// 与 baseline 同口径：不提供回滚，回退靠回滚二进制。
func chatV2Down(context.Context, *bun.DB) error { return nil }
