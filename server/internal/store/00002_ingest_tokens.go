// 迁移 00002：推流令牌改为用户维度——建 ingest_tokens（每用户一把）与
// ingest_endpoints（livekit-ingress 实例按 (令牌, alias) 持有的上游端点凭证）两张表。
// 旧 ingresses 表的数据搬迁与 DROP 不在本迁移：那是数据语义迁移（需读旧密钥发给用户），
// 走 api 层 settings.migration_version 游标 v2（store 侧提供 LegacyIngressTokens/DropIngresses）。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(ingestTokensUp, ingestTokensDown)
}

func ingestTokensUp(ctx context.Context, db *bun.DB) error {
	for _, m := range []any{(*ingestTokenRow)(nil), (*ingestEndpointRow)(nil)} {
		if _, err := db.NewCreateTable().Model(m).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	return nil
}

// 与 baseline 同口径：不提供回滚，回退靠回滚二进制。
func ingestTokensDown(context.Context, *bun.DB) error { return nil }
