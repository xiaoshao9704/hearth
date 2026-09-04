// 推流令牌：每用户一把（ingest_tokens，不分频道/设备，频道在 WHIP URL 里）。
// ingest_endpoints 表（旧 livekit-ingress 实例的上游端点凭证）已随内核收敛停用：
// 内容被游标迁移清空，读写代码删除，表本身留到下个版本再出迁移删表。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// IngestToken 每用户一把的推流令牌；tag 是可改的设备标签属性（默认 obs），改标签即改下次推流的 identity。
type IngestToken struct {
	ID        int64     `json:"-"`
	UserID    int64     `json:"-"`
	Tag       string    `json:"tag"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

const ingestTokenCols = `id, user_id, tag, token, created_at`

func scanIngestToken(scanner interface{ Scan(...any) error }, t *IngestToken) error {
	return scanner.Scan(&t.ID, &t.UserID, &t.Tag, &t.Token, &t.CreatedAt)
}

// newIngestTokenValue 生成令牌值（crypto/rand 32 字节 hex，与 CreateSession 同做法）。
func newIngestTokenValue() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// IngestTokenByUser 查用户的推流令牌，无记录返回 ErrNotFound。
func (s *Store) IngestTokenByUser(ctx context.Context, userID int64) (*IngestToken, error) {
	var t IngestToken
	err := scanIngestToken(s.bun.QueryRowContext(ctx,
		"SELECT "+ingestTokenCols+" FROM ingest_tokens WHERE user_id = ?", userID), &t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// IngestTokenByToken 按令牌值反查（WHIP 入场反查用），无记录返回 ErrNotFound。
func (s *Store) IngestTokenByToken(ctx context.Context, token string) (*IngestToken, error) {
	var t IngestToken
	err := scanIngestToken(s.bun.QueryRowContext(ctx,
		"SELECT "+ingestTokenCols+" FROM ingest_tokens WHERE token = ?", token), &t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// CreateIngestToken 为用户建推流令牌（token 由 store 生成）；每用户一把，重复创建触发唯一冲突。
// tag 传空时落库默认值 obs（模型 default）。
func (s *Store) CreateIngestToken(ctx context.Context, userID int64, tag string) (*IngestToken, error) {
	tok, err := newIngestTokenValue()
	if err != nil {
		return nil, err
	}
	row := &ingestTokenRow{UserID: userID, Tag: tag, Token: tok}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	// 回读拿 DB 侧默认值（created_at 与空 tag 的 default）
	return s.IngestTokenByUser(ctx, userID)
}

// ResetIngestToken 换令牌值（旧令牌立即失效），tag 保留；用户无令牌时返回 ErrNotFound。
func (s *Store) ResetIngestToken(ctx context.Context, userID int64) (*IngestToken, error) {
	tok, err := newIngestTokenValue()
	if err != nil {
		return nil, err
	}
	if _, err := s.bun.NewRaw(
		"UPDATE ingest_tokens SET token = ? WHERE user_id = ?", tok, userID).Exec(ctx); err != nil {
		return nil, err
	}
	return s.IngestTokenByUser(ctx, userID)
}

// ImportIngestToken 仅供游标 v3 迁移用：按原值导入旧 stream_key 作为用户的推流令牌
// （升级后 OBS 沿用旧密钥，只需给服务器地址加频道段）。tag 落库默认值 obs。
func (s *Store) ImportIngestToken(ctx context.Context, userID int64, token string) error {
	_, err := s.bun.NewInsert().Model(&ingestTokenRow{UserID: userID, Token: token}).Exec(ctx)
	return err
}

// UpdateIngestTokenTag 改设备标签（不影响令牌值与正在推流的会话，下次推流生效）。
func (s *Store) UpdateIngestTokenTag(ctx context.Context, userID int64, tag string) error {
	_, err := s.bun.NewRaw(
		"UPDATE ingest_tokens SET tag = ? WHERE user_id = ?", tag, userID).Exec(ctx)
	return err
}

// ---- 实例端点（已停用，仅供游标迁移清表）----

// DeleteAllIngestEndpoints 清空端点表（游标 v4/v5 迁移用；表本身下个版本再删）。
func (s *Store) DeleteAllIngestEndpoints(ctx context.Context) error {
	_, err := s.bun.NewRaw("DELETE FROM ingest_endpoints").Exec(ctx)
	return err
}

// ---- 游标迁移专用（勿用于常规路径）----

// LegacyIngressTokens 仅供游标 v3 迁移用：读旧 ingresses 表，每用户取最近创建（id 最大）
// 的一把 stream_key 作为其推流令牌，其余丢弃。旧表已删（v2 半途重入）视为空。
func (s *Store) LegacyIngressTokens(ctx context.Context) (map[int64]string, error) {
	rows, err := s.bun.QueryContext(ctx, `
SELECT user_id, stream_key FROM ingresses
WHERE id IN (SELECT MAX(id) FROM ingresses GROUP BY user_id)`)
	if err != nil {
		if isMissingTableErr(err) {
			return map[int64]string{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var userID int64
		var key string
		if err := rows.Scan(&userID, &key); err != nil {
			return nil, err
		}
		out[userID] = key
	}
	return out, rows.Err()
}

// DropIngresses 仅供游标 v3 迁移用：数据搬迁完成后 DROP 旧 ingresses 表。
func (s *Store) DropIngresses(ctx context.Context) error {
	_, err := s.bun.NewDropTable().Model((*ingressRow)(nil)).IfExists().Exec(ctx)
	return err
}
