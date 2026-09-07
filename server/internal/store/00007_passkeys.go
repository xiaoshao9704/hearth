// 迁移 00007：通行密钥（Passkey / WebAuthn）表。
// 二进制值（credential_id / public_key / aaguid）一律存 base64url 文本而不是 BLOB：
// credential_id 要建唯一索引，而三方言对可索引的二进制列措辞不一（mysql 的 BLOB 不能直接
// 建唯一索引、要 VARBINARY，pg 没有 VARBINARY），文本列在三方言下一致；另两列跟着同一
// 口径，免得同一张表里两种二进制表示。
// 文件名即迁移名（bun/migrate 从调用者文件名解析），不要改名。
package store

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(passkeysUp, passkeysDown)
}

func passkeysUp(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*passkeyRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	// 列表按 user_id 取；登录时按 credential_id 反查（唯一约束自带索引）。
	// mysql 不支持 CREATE INDEX IF NOT EXISTS，重复错误吞掉。
	if _, err := db.NewCreateIndex().Model((*passkeyRow)(nil)).
		Index("idx_passkeys_user").Column("user_id").
		IfNotExists().Exec(ctx); err != nil && !isDuplicateErr(err) {
		return err
	}
	return nil
}

// 与 baseline 同口径：不提供回滚，回退靠回滚二进制。
func passkeysDown(context.Context, *bun.DB) error { return nil }
