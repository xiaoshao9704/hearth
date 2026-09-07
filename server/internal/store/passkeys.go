// 通行密钥（Passkey / WebAuthn）凭证的存取。
// 这一层只搬字节：凭证的语义（挑战校验、签名、sign_count 单调）全在 api/passkey.go，
// 存储侧不认识 WebAuthn。凭证 ID 与公钥都是 base64url 文本（见 00007 迁移的理由），
// 对外只发 id/name/时间戳与备份标记，凭证本体不出库。
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Passkey 一枚通行密钥。json 标签只覆盖对外展示的字段；凭证本体（CredentialID/PublicKey）
// 标 "-"，不进任何响应，也不进日志。
type Passkey struct {
	ID             int64      `json:"id"`
	UserID         int64      `json:"-"`
	CredentialID   string     `json:"-"` // base64url
	PublicKey      string     `json:"-"` // base64url
	SignCount      uint32     `json:"-"`
	AAGUID         string     `json:"-"` // base64url
	Transports     string     `json:"-"` // 逗号分隔
	BackupEligible bool       `json:"backup_eligible"`
	BackupState    bool       `json:"backup_state"` // 已同步/已备份（展示用，每次登录更新）
	UserVerified   bool       `json:"-"`            // 规范里的 uvInitialized（只从 false 变 true）
	Name           string     `json:"name"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     *time.Time `json:"last_used_at"`
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// CreatePasskey 写入一枚新凭证，回填 ID/CreatedAt。credential_id 冲突时返回唯一约束错误
// （调用方用 IsUniqueViolation 判「这枚凭证已经注册过」）。
func (s *Store) CreatePasskey(ctx context.Context, p *Passkey) error {
	row := &passkeyRow{
		UserID:         p.UserID,
		CredentialID:   p.CredentialID,
		PublicKey:      p.PublicKey,
		SignCount:      int64(p.SignCount),
		AAGUID:         p.AAGUID,
		Transports:     p.Transports,
		BackupEligible: b2i(p.BackupEligible),
		BackupState:    b2i(p.BackupState),
		UserVerified:   b2i(p.UserVerified),
		Name:           p.Name,
		CreatedAt:      time.Now(),
	}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return err
	}
	p.ID = row.ID
	p.CreatedAt = row.CreatedAt
	return nil
}

const passkeyCols = `id, user_id, credential_id, public_key, sign_count, aaguid, transports,
backup_eligible, backup_state, user_verified, name, created_at, last_used_at`

func scanPasskey(rows interface {
	Scan(dest ...any) error
}) (Passkey, error) {
	var p Passkey
	var signCount int64
	var be, bs, uv int64
	err := rows.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.PublicKey, &signCount,
		&p.AAGUID, &p.Transports, &be, &bs, &uv, &p.Name, &p.CreatedAt, &p.LastUsedAt)
	p.SignCount = uint32(signCount)
	p.BackupEligible, p.BackupState, p.UserVerified = be != 0, bs != 0, uv != 0
	return p, err
}

// ListPasskeys 该用户的全部凭证，新的排前面。
func (s *Store) ListPasskeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := s.bun.QueryContext(ctx,
		"SELECT "+passkeyCols+" FROM passkeys WHERE user_id = ? ORDER BY id DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPasskeys 该用户有几枚凭证（/api/me 的 passkey_count，登录后推荐卡片据此判断）。
func (s *Store) CountPasskeys(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.bun.NewRaw("SELECT COUNT(1) FROM passkeys WHERE user_id = ?", userID).Scan(ctx, &n)
	return n, err
}

// PasskeyByCredentialID 按凭证 ID（base64url）反查；没有返回 ErrNotFound。
// 这是可发现凭证登录的入口：先由凭证找到用户，再校验签名。
func (s *Store) PasskeyByCredentialID(ctx context.Context, credentialID string) (*Passkey, error) {
	row := s.bun.QueryRowContext(ctx,
		"SELECT "+passkeyCols+" FROM passkeys WHERE credential_id = ?", credentialID)
	p, err := scanPasskey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdatePasskeySignCount 成功断言后更新签名计数与备份状态（backup_state 会变，
// backup_eligible 按规范不变，所以不动）。user_verified 只允许从 false 变 true。
func (s *Store) UpdatePasskeySignCount(ctx context.Context, id int64, signCount uint32, backupState, userVerified bool) error {
	_, err := s.bun.NewRaw(
		"UPDATE passkeys SET sign_count = ?, backup_state = ?, user_verified = CASE WHEN user_verified = 1 THEN 1 ELSE ? END WHERE id = ?",
		int64(signCount), b2i(backupState), b2i(userVerified), id).Exec(ctx)
	return err
}

// UpdatePasskeyLastUsed 记下最近一次成功登录的时间（列表展示用）。
func (s *Store) UpdatePasskeyLastUsed(ctx context.Context, id int64) error {
	_, err := s.bun.NewRaw("UPDATE passkeys SET last_used_at = ? WHERE id = ?", time.Now(), id).Exec(ctx)
	return err
}

// RenamePasskey 改名，限本人的凭证；没有匹配行返回 false。
func (s *Store) RenamePasskey(ctx context.Context, userID, id int64, name string) (bool, error) {
	res, err := s.bun.NewRaw("UPDATE passkeys SET name = ? WHERE id = ? AND user_id = ?", name, id, userID).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeletePasskey 删除，限本人的凭证；没有匹配行返回 false。
func (s *Store) DeletePasskey(ctx context.Context, userID, id int64) (bool, error) {
	res, err := s.bun.NewRaw("DELETE FROM passkeys WHERE id = ? AND user_id = ?", id, userID).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
