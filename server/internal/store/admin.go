// 账户 / 邀请 / 管理后台相关存储：与 store.go 同包，复用方言与占位符机制。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/uptrace/bun"
)

// ---- 账户 ----

// PasswordHash 取用户当前密码哈希（改密时校验旧密码用）。
func (s *Store) PasswordHash(ctx context.Context, userID int64) (string, error) {
	var hash string
	err := s.bun.NewRaw("SELECT password_hash FROM users WHERE id = ?", userID).Scan(ctx, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return hash, err
}

func (s *Store) UpdateUsername(ctx context.Context, userID int64, username string) error {
	_, err := s.bun.NewRaw("UPDATE users SET username = ? WHERE id = ?", username, userID).Exec(ctx)
	return err
}

func (s *Store) UpdatePassword(ctx context.Context, userID int64, hash string) error {
	_, err := s.bun.NewRaw("UPDATE users SET password_hash = ? WHERE id = ?", hash, userID).Exec(ctx)
	return err
}

// DeleteOtherSessions 改密后退出其他设备的会话，保留当前 token。
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keepToken string) error {
	_, err := s.bun.NewRaw(
		"DELETE FROM sessions WHERE user_id = ? AND token <> ?", userID, keepToken).Exec(ctx)
	return err
}

// ---- 设备档案 ----

type Device struct {
	bun.BaseModel `bun:"table:devices"`

	DeviceID  string    `json:"device_id"`
	Tag       string    `json:"tag"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

func (s *Store) ListDevices(ctx context.Context, userID int64) ([]Device, error) {
	out := []Device{}
	err := s.bun.NewRaw(`
SELECT device_id, tag, first_seen, last_seen FROM devices
WHERE user_id = ? ORDER BY last_seen DESC`, userID).Scan(ctx, &out)
	return out, err
}

func (s *Store) DeleteDevice(ctx context.Context, userID int64, deviceID string) error {
	_, err := s.bun.NewRaw(
		"DELETE FROM devices WHERE user_id = ? AND device_id = ?", userID, deviceID).Exec(ctx)
	return err
}

// ---- 设置（k/v，注册策略等）----

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.bun.NewRaw("SELECT v FROM settings WHERE k = ?", key).Scan(ctx, &v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	row := &settingRow{K: key, V: value}
	// upsert 方言分叉：mysql 无 ON CONFLICT，其余两种同语法
	q := s.bun.NewInsert().Model(row)
	if s.d.name == "mysql" {
		q = q.On("DUPLICATE KEY UPDATE").Set("v = VALUES(v)")
	} else {
		q = q.On("CONFLICT (k) DO UPDATE").Set("v = EXCLUDED.v")
	}
	_, err := q.Exec(ctx)
	return err
}

// DeleteSetting 删除设置键本身（区别于 SetSetting 置空）；键不存在不算错误。
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.bun.NewRaw("DELETE FROM settings WHERE k = ?", key).Exec(ctx)
	return err
}

// DeleteSettingsByPrefix 按前缀批量删除设置键，返回删除行数（游标迁移清退场内核的全局键用）。
func (s *Store) DeleteSettingsByPrefix(ctx context.Context, prefix string) (int64, error) {
	res, err := s.bun.NewRaw("DELETE FROM settings WHERE k LIKE ?", prefix+"%").Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

// ---- 邀请链接 ----

type Invite struct {
	bun.BaseModel `bun:"table:invites,alias:i"`

	ID          int64     `bun:",pk,autoincrement" json:"id"`
	Code        string    `json:"code"`
	Kind        string    `json:"kind"`                 // register/guest
	ChannelID   *int64    `json:"channel_id"`           // guest 类授予的频道
	ChannelName string    `bun:"-" json:"channel_name"` // guest 类授予的频道名（JOIN 填充）
	Role        string    `json:"role"`                 // register 类产出的系统角色
	GuestTTLSec int       `json:"guest_ttl_sec"`        // guest 类产出访客的寿命（秒）
	AllowGuest  bool      `json:"allow_guest"`          // register 类是否允许「先以访客进入」（阶段三）
	Note        string    `json:"note"`
	MaxUses     int       `json:"max_uses"` // 0 = 不限
	Used        int       `json:"used"`
	Revoked     bool      `json:"revoked"`
	CreatedBy   string    `bun:"-" json:"created_by"` // 创建者用户名（JOIN 填充）
	CreatedByID int64     `json:"-"`                  // 创建者 user_id（归属判断用，不外发）
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Alive 邀请当前是否可用。
func (i *Invite) Alive(now time.Time) bool {
	if i.Revoked || now.After(i.ExpiresAt) {
		return false
	}
	return i.MaxUses == 0 || i.Used < i.MaxUses
}

const inviteAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去掉易混淆字符

// CreateInvite 创建注册类邀请；role 为空表示跟随注册默认档（消费时按 cfg_reg_default_role 解析）。
func (s *Store) CreateInvite(ctx context.Context, createdBy int64, note string, maxUses int, ttl time.Duration, role Role) (*Invite, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	code := make([]byte, 8)
	for i, b := range buf {
		code[i] = inviteAlphabet[int(b)%len(inviteAlphabet)]
	}
	inv := &Invite{Code: string(code), Kind: "register", Role: string(role),
		Note: note, MaxUses: maxUses, ExpiresAt: time.Now().Add(ttl)}
	row := &inviteRow{Code: inv.Code, Note: note, MaxUses: maxUses, CreatedBy: createdBy,
		ExpiresAt: inv.ExpiresAt, Role: string(role)}
	// max_uses = 0 是合法值（不限次数），而模型带 default:1 时零值会被写成 DEFAULT，
	// 用 Column + Value 强制按实参写入；显式 Column 后自增 id 不进自动 RETURNING 列表，
	// 需显式 Returning("id") 才能回填主键（mysql 不支持 RETURNING，自动回落 LastInsertId）。
	_, err := s.bun.NewInsert().Model(row).
		Column("code", "note", "max_uses", "expires_at", "created_by", "role").
		Value("max_uses", "?", maxUses).
		Value("role", "?", string(role)).
		Returning("id").
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	inv.ID = row.ID
	inv.CreatedByID = createdBy
	return inv, nil
}

func scanInvite(scanner interface{ Scan(...any) error }, i *Invite) error {
	var revoked, allowGuest int64
	err := scanner.Scan(&i.ID, &i.Code, &i.Kind, &i.ChannelID, &i.Role, &i.GuestTTLSec, &allowGuest,
		&i.Note, &i.MaxUses, &i.Used, &revoked, &i.CreatedByID, &i.CreatedBy, &i.ChannelName, &i.CreatedAt, &i.ExpiresAt)
	i.Revoked = revoked != 0
	i.AllowGuest = allowGuest != 0
	return err
}

const inviteCols = `i.id, i.code, i.kind, i.channel_id, i.role, i.guest_ttl_sec, i.allow_guest,
	i.note, i.max_uses, i.used, i.revoked, i.created_by, u.username, COALESCE(ic.name, ''), i.created_at, i.expires_at`
const inviteJoins = ` FROM invites i
JOIN users u ON u.id = i.created_by
LEFT JOIN channels ic ON ic.id = i.channel_id`

// ListInvites 全部邀请（管理员视角）。
func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	return s.listInvites(ctx, "")
}

// ListInvitesByCreator 某个用户发的邀请。
func (s *Store) ListInvitesByCreator(ctx context.Context, createdBy int64) ([]Invite, error) {
	return s.listInvites(ctx, " WHERE i.created_by = ?", createdBy)
}

func (s *Store) listInvites(ctx context.Context, where string, args ...any) ([]Invite, error) {
	rows, err := s.bun.QueryContext(ctx,
		`SELECT `+inviteCols+inviteJoins+where+` ORDER BY i.id DESC`, args...)
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
	err := scanInvite(s.bun.QueryRowContext(ctx,
		`SELECT `+inviteCols+inviteJoins+` WHERE i.code = ?`, code), &i)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &i, err
}

func (s *Store) InviteByID(ctx context.Context, id int64) (*Invite, error) {
	var i Invite
	err := scanInvite(s.bun.QueryRowContext(ctx,
		`SELECT `+inviteCols+inviteJoins+` WHERE i.id = ?`, id), &i)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &i, err
}

// ConsumeInvite 占用一次邀请名额；名额已满/撤销/过期时返回 ErrNotFound。
func (s *Store) ConsumeInvite(ctx context.Context, id int64) error {
	res, err := s.bun.NewRaw(`
UPDATE invites SET used = used + 1
WHERE id = ? AND revoked = 0 AND expires_at > ? AND (max_uses = 0 OR used < max_uses)`,
		id, time.Now()).Exec(ctx)
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
	_, err := s.bun.NewRaw("UPDATE invites SET revoked = 1 WHERE id = ?", id).Exec(ctx)
	return err
}

func (s *Store) DeleteInvite(ctx context.Context, id int64) error {
	_, err := s.bun.NewRaw("DELETE FROM invites WHERE id = ?", id).Exec(ctx)
	return err
}

// ---- 管理后台：用户 ----

type AdminUser struct {
	ID        int64      `json:"id"`
	Username  string     `json:"username"`
	Role      Role       `json:"role"`
	IsAdmin   bool       `json:"is_admin"` // 派生只读（role ≥ admin），前端过渡用
	Disabled  bool       `json:"disabled"`
	ExpiresAt *time.Time `json:"expires_at"` // 访客行：过期时间
	Invite    string     `json:"invite"`     // 访客行：来源邀请码（无则空）
	CreatedAt time.Time  `json:"created_at"`
	Devices   int        `json:"devices"`
	LastSeen  *time.Time `json:"last_seen"` // 任一设备最近活跃；无设备档案为 null
	// CanSetRoles 当前请求的管理员可给该用户授予的角色候选（接口层按 perm 规则填充，前端不推导阶梯）
	CanSetRoles []Role `json:"can_set_roles"`
}

func (s *Store) ListUsersAdmin(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.bun.QueryContext(ctx, `
SELECT u.id, u.username, u.role, u.disabled, u.created_at, u.expires_at, COALESCE(iv.code, ''),
       COUNT(d.id), MAX(d.last_seen)
FROM users u
LEFT JOIN devices d ON d.user_id = u.id
LEFT JOIN invites iv ON iv.id = u.invite_id
GROUP BY u.id, u.username, u.role, u.disabled, u.created_at, u.expires_at, iv.code
ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminUser{}
	for rows.Next() {
		var u AdminUser
		var role, invite string
		var disabled int64
		var expiresAt *time.Time
		// MAX() 聚合没有列声明类型:sqlite 驱动返回字符串,MySQL/PG 返回 time.Time
		var last any
		if err := rows.Scan(&u.ID, &u.Username, &role, &disabled, &u.CreatedAt, &expiresAt, &invite,
			&u.Devices, &last); err != nil {
			return nil, err
		}
		u.Role = Role(role)
		u.IsAdmin = u.Role.Rank() >= RoleAdmin.Rank()
		u.Disabled = disabled != 0
		u.ExpiresAt = expiresAt
		u.Invite = invite
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
	var role string
	var disabled int64
	var expiresAt *time.Time
	err := s.bun.NewRaw(
		"SELECT id, username, role, disabled, expires_at FROM users WHERE id = ?", id).
		Scan(ctx, &u.ID, &u.Username, &role, &disabled, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	scanUserParts(&u, role, expiresAt)
	u.Disabled = disabled != 0
	return &u, err
}

// SetUserDisabled 停用即刻生效：现有会话一并清除。
func (s *Store) SetUserDisabled(ctx context.Context, id int64, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	if _, err := s.bun.NewRaw("UPDATE users SET disabled = ? WHERE id = ?", v, id).Exec(ctx); err != nil {
		return err
	}
	if disabled {
		_, err := s.bun.NewRaw("DELETE FROM sessions WHERE user_id = ?", id).Exec(ctx)
		return err
	}
	return nil
}

// DeleteUser 删除用户及其会话/设备/成员行/封禁/推流令牌与端点记录；其名下的 owner 频道
// 过户给 adoptTo（执行删除的管理员，避免误伤活跃频道），历史消息保留。返回过户的频道数。
func (s *Store) DeleteUser(ctx context.Context, id, adoptTo int64) (int, error) {
	// owner 频道过户：旧 owner 行改到接收人名下（接收人在该频道的旧行先删，避免唯一冲突）
	rows, err := s.bun.QueryContext(ctx,
		"SELECT channel_id FROM channel_members WHERE user_id = ? AND role = 'owner'", id)
	if err != nil {
		return 0, err
	}
	var chans []int64
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			rows.Close()
			return 0, err
		}
		chans = append(chans, cid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, cid := range chans {
		if _, err := s.bun.NewRaw(
			"DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?", cid, adoptTo).Exec(ctx); err != nil {
			return 0, err
		}
		if _, err := s.bun.NewRaw(
			"UPDATE channel_members SET user_id = ? WHERE channel_id = ? AND user_id = ? AND role = 'owner'",
			adoptTo, cid, id).Exec(ctx); err != nil {
			return 0, err
		}
	}
	for _, q := range []string{
		"DELETE FROM sessions WHERE user_id = ?",
		"DELETE FROM devices WHERE user_id = ?",
		"DELETE FROM ingest_endpoints WHERE token_id IN (SELECT id FROM ingest_tokens WHERE user_id = ?)",
		"DELETE FROM ingest_tokens WHERE user_id = ?",
		"DELETE FROM channel_members WHERE user_id = ?",
		"DELETE FROM channel_bans WHERE user_id = ?",
		"DELETE FROM channel_gags WHERE user_id = ?",
		"DELETE FROM users WHERE id = ?",
	} {
		if _, err := s.bun.NewRaw(q, id).Exec(ctx); err != nil {
			return 0, err
		}
	}
	return len(chans), nil
}

// ---- 管理后台：频道 ----

// DeleteChannel 删除频道及其消息/封禁/白名单记录。
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	for _, q := range []string{
		"DELETE FROM messages WHERE channel_id = ?",
		"DELETE FROM channel_bans WHERE channel_id = ?",
		"DELETE FROM channel_members WHERE channel_id = ?",
		"DELETE FROM channels WHERE id = ?",
	} {
		if _, err := s.bun.NewRaw(q, id).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ---- 概览 ----

func (s *Store) Counts(ctx context.Context) (users, channels int, err error) {
	if err = s.bun.NewRaw("SELECT COUNT(1) FROM users").Scan(ctx, &users); err != nil {
		return
	}
	err = s.bun.NewRaw("SELECT COUNT(1) FROM channels").Scan(ctx, &channels)
	return
}
