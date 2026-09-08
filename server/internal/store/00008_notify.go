// 迁移 00008：Web Push 订阅表 + 频道静音表。
// push_subscriptions.endpoint 是浏览器给的推送地址，唯一（同一地址只留一条），因此列宽
// 卡在 mysql 的索引键长上限（InnoDB 3072 字节，utf8mb4 每字符按 4 字节算）：
// varchar(512) 与 passkeys.credential_id 同一取舍，接口层也按 512 校验；
// 实际地址（各厂商推送网关）都远短于此。
// session_id 存会话指纹（store.SessionID，非 token 本体）：会话下线即删它的订阅。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(notifyUp, notifyDown)
}

func notifyUp(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*pushSubscriptionRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	if _, err := db.NewCreateTable().Model((*channelMuteRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	// 两条索引各服务一个查询：user_id 服务「这批人的订阅」，session_id 服务退出登录时的删除。
	// mysql 不支持 CREATE INDEX IF NOT EXISTS，重复错误吞掉。
	if _, err := db.NewCreateIndex().Model((*pushSubscriptionRow)(nil)).
		Index("idx_push_subs_user").Column("user_id").
		IfNotExists().Exec(ctx); err != nil && !isDuplicateErr(err) {
		return err
	}
	if _, err := db.NewCreateIndex().Model((*pushSubscriptionRow)(nil)).
		Index("idx_push_subs_session").Column("session_id").
		IfNotExists().Exec(ctx); err != nil && !isDuplicateErr(err) {
		return err
	}
	return nil
}

// 与 baseline 同口径：不提供回滚，回退靠回滚二进制。
func notifyDown(context.Context, *bun.DB) error { return nil }
