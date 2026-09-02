// 账户 / 邀请 / 管理后台相关存储：与 store.go 同包，复用方言与占位符机制。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// ---- 账户 ----

// PasswordHash 取用户当前密码哈希（改密时校验旧密码用）。
func (s *Store) PasswordHash(ctx context.Context, userID int64) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, s.q("SELECT password_hash FROM users WHERE id = ?"), userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return hash, err
}

func (s *Store) UpdateUsername(ctx context.Context, userID int64, username string) error {
	_, err := s.db.ExecContext(ctx, s.q("UPDATE users SET username = ? WHERE id = ?"), username, userID)
	return err
}

func (s *Store) UpdatePassword(ctx context.Context, userID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, s.q("UPDATE users SET password_hash = ? WHERE id = ?"), hash, userID)
	return err
}

// DeleteOtherSessions 改密后退出其他设备的会话，保留当前 token。
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keepToken string) error {
	_, err := s.db.ExecContext(ctx, s.q(
		"DELETE FROM sessions WHERE user_id = ? AND token <> ?"), userID, keepToken)
	return err
}

// ---- 设备档案 ----

type Device struct {
	DeviceID  string    `json:"device_id"`
	Tag       string    `json:"tag"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

func (s *Store) ListDevices(ctx context.Context, userID int64) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
SELECT device_id, tag, first_seen, last_seen FROM devices
WHERE user_id = ? ORDER BY last_seen DESC`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.DeviceID, &d.Tag, &d.FirstSeen, &d.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDevice(ctx context.Context, userID int64, deviceID string) error {
	_, err := s.db.ExecContext(ctx, s.q(
		"DELETE FROM devices WHERE user_id = ? AND device_id = ?"), userID, deviceID)
	return err
}

// ---- 设置（k/v，注册策略等）----

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, s.q("SELECT v FROM settings WHERE k = ?"), key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	var query string
	switch s.d.name {
	case "mysql":
		query = `INSERT INTO settings (k, v) VALUES (?, ?) ON DUPLICATE KEY UPDATE v = VALUES(v)`
	default: // sqlite / postgres 同语法
		query = `INSERT INTO settings (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`
	}
	_, err := s.db.ExecContext(ctx, s.q(query), key, value)
	return err
}

// ---- 数据迁移游标（settings 键 migration_version，非配置项，管理后台不展示）----

// MigrationVersion 读迁移游标；缺失或损坏视为 0。
func (s *Store) MigrationVersion(ctx context.Context) (int, error) {
	v, err := s.GetSetting(ctx, "migration_version")
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func (s *Store) SetMigrationVersion(ctx context.Context, v int) error {
	return s.SetSetting(ctx, "migration_version", strconv.Itoa(v))
}

// RewriteIngressProvider 把 ingress 记录的归属实例别名从 from 改为 to（版本迁移用）。
func (s *Store) RewriteIngressProvider(ctx context.Context, from, to string) error {
	_, err := s.db.ExecContext(ctx, s.q("UPDATE ingresses SET provider = ? WHERE provider = ?"), to, from)
	return err
}

// ---- 邀请链接 ----

type Invite struct {
	ID        int64     `json:"id"`
	Code      string    `json:"code"`
	Note      string    `json:"note"`
	MaxUses   int       `json:"max_uses"` // 0 = 不限
	Used      int       `json:"used"`
	Revoked   bool      `json:"revoked"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Alive 邀请当前是否可用。
func (i *Invite) Alive(now time.Time) bool {
	if i.Revoked || now.After(i.ExpiresAt) {
		return false
	}
	return i.MaxUses == 0 || i.Used < i.MaxUses
}

const inviteAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去掉易混淆字符

func (s *Store) CreateInvite(ctx context.Context, createdBy int64, note string, maxUses int, ttl time.Duration) (*Invite, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	code := make([]byte, 8)
	for i, b := range buf {
		code[i] = inviteAlphabet[int(b)%len(inviteAlphabet)]
	}
	inv := &Invite{Code: string(code), Note: note, MaxUses: maxUses, ExpiresAt: time.Now().Add(ttl)}
	id, err := s.insertID(ctx, `
INSERT INTO invites (code, note, max_uses, expires_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		inv.Code, note, maxUses, inv.ExpiresAt, createdBy)
	if err != nil {
		return nil, err
	}
	inv.ID = id
	return inv, nil
}

func scanInvite(scanner interface{ Scan(...any) error }, i *Invite) error {
	var revoked int64
	err := scanner.Scan(&i.ID, &i.Code, &i.Note, &i.MaxUses, &i.Used, &revoked, &i.CreatedBy, &i.CreatedAt, &i.ExpiresAt)
	i.Revoked = revoked != 0
	return err
}

const inviteCols = `i.id, i.code, i.note, i.max_uses, i.used, i.revoked, u.username, i.created_at, i.expires_at`

func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
SELECT `+inviteCols+` FROM invites i JOIN users u ON u.id = i.created_by ORDER BY i.id DESC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var i Invite
		if err := scanInvite(rows, &i); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) InviteByCode(ctx context.Context, code string) (*Invite, error) {
	var i Invite
	err := scanInvite(s.db.QueryRowContext(ctx, s.q(`
SELECT `+inviteCols+` FROM invites i JOIN users u ON u.id = i.created_by WHERE i.code = ?`), code), &i)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &i, err
}

// ConsumeInvite 占用一次邀请名额；名额已满/撤销/过期时返回 ErrNotFound。
func (s *Store) ConsumeInvite(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, s.q(`
UPDATE invites SET used = used + 1
WHERE id = ? AND revoked = 0 AND expires_at > ? AND (max_uses = 0 OR used < max_uses)`),
		id, time.Now())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) RevokeInvite(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.q("UPDATE invites SET revoked = 1 WHERE id = ?"), id)
	return err
}

func (s *Store) DeleteInvite(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM invites WHERE id = ?"), id)
	return err
}

// ---- 管理后台：用户 ----

type AdminUser struct {
	ID        int64      `json:"id"`
	Username  string     `json:"username"`
	IsAdmin   bool       `json:"is_admin"`
	Disabled  bool       `json:"disabled"`
	CreatedAt time.Time  `json:"created_at"`
	Devices   int        `json:"devices"`
	LastSeen  *time.Time `json:"last_seen"` // 任一设备最近活跃；无设备档案为 null
}

func (s *Store) ListUsersAdmin(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
SELECT u.id, u.username, u.is_admin, u.disabled, u.created_at,
       COUNT(d.id), MAX(d.last_seen)
FROM users u LEFT JOIN devices d ON d.user_id = u.id
GROUP BY u.id, u.username, u.is_admin, u.disabled, u.created_at
ORDER BY u.id`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminUser{}
	for rows.Next() {
		var u AdminUser
		var isAdmin, disabled int64
		// MAX() 聚合没有列声明类型:sqlite 驱动返回字符串,MySQL/PG 返回 time.Time
		var last any
		if err := rows.Scan(&u.ID, &u.Username, &isAdmin, &disabled, &u.CreatedAt, &u.Devices, &last); err != nil {
			return nil, err
		}
		u.IsAdmin = isAdmin != 0
		u.Disabled = disabled != 0
		u.LastSeen = aggTime(last)
		out = append(out, u)
	}
	return out, rows.Err()
}

// aggTime 解析聚合时间列在不同驱动下的返回:sqlite 是 "2006-01-02 15:04:05"(UTC) 字符串,
// MySQL/PG 是 time.Time,nil 表示无设备记录。
func aggTime(v any) *time.Time {
	switch t := v.(type) {
	case nil:
		return nil
	case time.Time:
		return &t
	case []byte:
		return aggTime(string(t))
	case string:
		if ts, err := time.Parse("2006-01-02 15:04:05", t); err == nil {
			return &ts
		}
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return &ts
		}
	}
	return nil
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	var u User
	var isAdmin, disabled int64
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT id, username, is_admin, disabled FROM users WHERE id = ?"), id).
		Scan(&u.ID, &u.Username, &isAdmin, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.IsAdmin = isAdmin != 0
	u.Disabled = disabled != 0
	return &u, err
}

// SetUserDisabled 停用即刻生效：现有会话一并清除。
func (s *Store) SetUserDisabled(ctx context.Context, id int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	if _, err := s.db.ExecContext(ctx, s.q("UPDATE users SET disabled = ? WHERE id = ?"), v, id); err != nil {
		return err
	}
	if disabled {
		_, err := s.db.ExecContext(ctx, s.q("DELETE FROM sessions WHERE user_id = ?"), id)
		return err
	}
	return nil
}

var ErrOwnsChannels = errors.New("用户仍是频道房主")

// DeleteUser 删除用户及其会话/设备/白名单/封禁/ingress 记录；
// 名下还有频道时拒绝（避免频道悬空），历史消息保留。
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	var n int
	if err := s.db.QueryRowContext(ctx, s.q(
		"SELECT COUNT(1) FROM channels WHERE created_by = ?"), id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrOwnsChannels
	}
	for _, q := range []string{
		"DELETE FROM sessions WHERE user_id = ?",
		"DELETE FROM devices WHERE user_id = ?",
		"DELETE FROM ingresses WHERE user_id = ?",
		"DELETE FROM channel_members WHERE user_id = ?",
		"DELETE FROM channel_bans WHERE user_id = ?",
		"DELETE FROM users WHERE id = ?",
	} {
		if _, err := s.db.ExecContext(ctx, s.q(q), id); err != nil {
			return err
		}
	}
	return nil
}

// ---- 管理后台：频道 ----

// DeleteChannel 删除频道及其消息/封禁/白名单/ingress 记录。
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	for _, q := range []string{
		"DELETE FROM messages WHERE channel_id = ?",
		"DELETE FROM channel_bans WHERE channel_id = ?",
		"DELETE FROM channel_members WHERE channel_id = ?",
		"DELETE FROM ingresses WHERE channel_id = ?",
		"DELETE FROM channels WHERE id = ?",
	} {
		if _, err := s.db.ExecContext(ctx, s.q(q), id); err != nil {
			return err
		}
	}
	return nil
}

// ---- 概览 ----

func (s *Store) Counts(ctx context.Context) (users, channels int, err error) {
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM users").Scan(&users); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM channels").Scan(&channels)
	return
}
