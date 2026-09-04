// 存储层：用户、会话、频道、聊天消息、设备、推流令牌、封禁、白名单。
// 多后端：DATABASE_URL 决定方言 —— 空/file:/sqlite: 路径 = sqlite（modernc.org/sqlite，无 cgo）；
// mysql:// = go-sql-driver/mysql；postgres:// = jackc/pgx/v5。三种驱动均为纯 Go，可交叉编译。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/migrate"
	"github.com/uptrace/bun/schema"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("记录不存在")

// ---- 方言 ----

type dialect struct {
	name string // sqlite | mysql | postgres
}

// parseDBURL 解析 DATABASE_URL，返回方言、驱动名与 DSN。
func parseDBURL(dbURL string) (dialect, string, string, error) {
	switch {
	case strings.HasPrefix(dbURL, "mysql://"):
		u, err := url.Parse(dbURL)
		if err != nil {
			return dialect{}, "", "", fmt.Errorf("DATABASE_URL 解析失败: %w", err)
		}
		auth := u.User.Username()
		if pw, ok := u.User.Password(); ok {
			auth += ":" + pw
		}
		// parseTime=true：DATETIME 直接扫成 time.Time；
		// clientFoundRows=true：RowsAffected 统一为「匹配行数」（默认只算实际改变的行，
		// 同值 UPDATE 会误报 0，靠 RowsAffected 判存在的逻辑在 MySQL 下会误判）
		dsn := fmt.Sprintf("%s@tcp(%s)%s?parseTime=true&clientFoundRows=true", auth, u.Host, u.Path)
		return dialect{"mysql"}, "mysql", dsn, nil
	case strings.HasPrefix(dbURL, "postgres://"), strings.HasPrefix(dbURL, "postgresql://"):
		return dialect{"postgres"}, "pgx", dbURL, nil
	default:
		// 空 / file: / sqlite: / 裸路径 都按 sqlite 处理
		dsn := strings.TrimPrefix(dbURL, "sqlite:")
		if dsn == "" {
			dsn = "hearth.db"
		}
		if !strings.Contains(dsn, "?") {
			dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
		}
		return dialect{"sqlite"}, "sqlite", dsn, nil
	}
}

type Store struct {
	bun *bun.DB // Bun 句柄（包在 *sql.DB 上），全部查询与 schema 迁移走它
	d   dialect
}

// bunDialect 把方言名映射到 Bun 方言实现（驱动仍用原有三个纯 Go 驱动）。
func (d dialect) bunDialect() schema.Dialect {
	switch d.name {
	case "mysql":
		return mysqldialect.New()
	case "postgres":
		return pgdialect.New()
	default:
		return sqlitedialect.New()
	}
}

func Open(dbURL string) (*Store, error) {
	d, driver, dsn, err := parseDBURL(dbURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库连接失败: %w", err)
	}
	bunDB := bun.NewDB(db, d.bunDialect())
	s := &Store{bun: bunDB, d: d}
	// schema 迁移失败即启动失败（与旧 migrate() 语义一致）。
	// WithMarkAppliedOnSuccess：Up 成功后才登记 bun_migrations——默认是先登记再执行，
	// 中途失败会被永久跳过、库停在半迁移态；baselineUp 幂等，开此选项后重试安全。
	migrator := migrate.NewMigrator(bunDB, Migrations, migrate.WithMarkAppliedOnSuccess(true))
	ctx := context.Background()
	if err := migrator.Init(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema 迁移初始化失败: %w", err)
	}
	if _, err := migrator.Migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema 迁移失败: %w", err)
	}
	return s, nil
}

// Close 关闭底层连接（bun.Close 会关掉包在里面的 *sql.DB）。
func (s *Store) Close() error { return s.bun.Close() }

// isDuplicateErr 判"列/索引已存在"类错误（三方言文案不同），baseline 迁移吞掉建重错误。
func isDuplicateErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key name") // mysql CREATE INDEX 无 IF NOT EXISTS
}

// isMissingTableErr 判"表不存在"（三方言文案不同），游标 v2 重入时旧 ingresses 表已删属正常。
func isMissingTableErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") || // sqlite
		strings.Contains(msg, "doesn't exist") || // mysql
		strings.Contains(msg, "does not exist") // postgres
}

type User struct {
	bun.BaseModel `bun:"table:users,alias:u"`

	ID        int64      `json:"id"`
	Username  string     `json:"username"`
	Role      Role       `json:"role"`       // 系统角色（权威）
	IsAdmin   bool       `json:"is_admin"`   // 派生只读（role ≥ admin），供前端过渡一个版本
	Disabled  bool       `json:"-"`          // 停用账号（管理后台用，登录/会话校验时拦截）
	ExpiresAt *time.Time `json:"expires_at"` // 仅访客有值；普通用户为 null
	InviteID  *int64     `json:"-"`          // 访客来源邀请（审计用，不外发）
}

type Channel struct {
	bun.BaseModel `bun:"table:channels,alias:c"`

	ID         int64     `bun:",pk,autoincrement" json:"id"`
	Name       string    `json:"name"`
	CreatedBy  string    `bun:"-" json:"created_by"` // 房主用户名（JOIN 填充，房主归属见 OwnerID 注释）
	CreatedAt  time.Time `json:"created_at"`
	InviteOnly bool      `json:"invite_only"`
	MyRole     string    `bun:"-" json:"my_role"` // 对当前请求用户的频道角色（接口层填充，owner/moderator/member/""）
	Online     int       `bun:"-" json:"online"`  // 当前在房人数（接口层从内核填充）
	OwnerID    int64     `bun:"-" json:"-"`       // 房主用户 ID（内部用；权威是 channel_members 的 owner 行，查询里 COALESCE 回 created_by 历史列）
}

type Message struct {
	bun.BaseModel `bun:"table:messages,alias:m"`

	ID        int64     `bun:",pk,autoincrement" json:"id"`
	ChannelID int64     `json:"channel_id"`
	UserID    int64     `json:"uid"`              // 发送者 user_id（右键菜单等管理操作的目标）
	Username  string    `bun:"-" json:"username"` // 发送者用户名（JOIN 填充，纯展示）
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- 设备 ----

// RecordDevice 记录用户设备：首次出现建档，之后刷新 last_seen 与设备标签。
// upsert 的方言分叉是全 store 仅保留的两处之一（另一处是 admin.go 的 SetSetting）：mysql 无 ON CONFLICT，用 DUPLICATE KEY UPDATE + VALUES()。
func (s *Store) RecordDevice(ctx context.Context, userID int64, deviceID, tag string) error {
	row := &deviceRow{UserID: userID, DeviceID: deviceID, Tag: tag}
	q := s.bun.NewInsert().Model(row)
	if s.d.name == "mysql" {
		q = q.On("DUPLICATE KEY UPDATE").Set("last_seen = CURRENT_TIMESTAMP, tag = VALUES(tag)")
	} else { // sqlite / postgres 同语法
		q = q.On("CONFLICT (user_id, device_id) DO UPDATE").Set("last_seen = CURRENT_TIMESTAMP, tag = EXCLUDED.tag")
	}
	_, err := q.Exec(ctx)
	return err
}

// ---- 用户 ----

// CreateUser 创建用户（普通角色 user）；服务器上的第一个账号自动成为超级管理员。
// 注册产出其他档走 CreateUserWithRole。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	return s.CreateUserWithRole(ctx, username, passwordHash, RoleUser)
}

// CreateUserWithRole 按指定系统角色创建用户；首个账号无条件成为 super（全站恰好一个的保底）。
func (s *Store) CreateUserWithRole(ctx context.Context, username, passwordHash string, role Role) (*User, error) {
	var n int
	if err := s.bun.NewRaw("SELECT COUNT(1) FROM users").Scan(ctx, &n); err != nil {
		return nil, err
	}
	if n == 0 {
		role = RoleSuper
	}
	row := &userRow{Username: username, PasswordHash: passwordHash, Role: string(role)}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	u := &User{ID: row.ID, Username: username}
	scanUserParts(u, string(role), nil)
	return u, nil
}

func (s *Store) UserByName(ctx context.Context, username string) (*User, string, error) {
	var u User
	var hash, role string
	var disabled int64
	var expiresAt *time.Time
	err := s.bun.NewRaw(
		"SELECT id, username, password_hash, role, disabled, expires_at FROM users WHERE username = ?", username).
		Scan(ctx, &u.ID, &u.Username, &hash, &role, &disabled, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	scanUserParts(&u, role, expiresAt)
	u.Disabled = disabled != 0
	return &u, hash, err
}

// ---- 会话 ----

const sessionTTL = 7 * 24 * time.Hour // 会话有效期 7 天

func (s *Store) CreateSession(ctx context.Context, userID int64) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	_, err := s.bun.NewRaw(
		"INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)",
		token, userID, time.Now().Add(sessionTTL)).Exec(ctx)
	return token, err
}

// UserByToken 校验会话 token，过期、不存在或账号已停用返回 ErrNotFound。
func (s *Store) UserByToken(ctx context.Context, token string) (*User, error) {
	var u User
	var role string
	var expiresAt *time.Time
	err := s.bun.NewRaw(`
SELECT u.id, u.username, u.role, u.expires_at FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token = ? AND s.expires_at > ? AND u.disabled = 0`, token, time.Now()).
		Scan(ctx, &u.ID, &u.Username, &role, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	scanUserParts(&u, role, expiresAt)
	return &u, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.bun.NewRaw("DELETE FROM sessions WHERE token = ?", token).Exec(ctx)
	return err
}

// ---- 频道 ----

// scanChannel 统一扫描频道行（invite_only 以整数存储，兼容三种方言的布尔语义）。
func scanChannel(scanner interface{ Scan(...any) error }, c *Channel) error {
	var inviteOnly int64
	err := scanner.Scan(&c.ID, &c.Name, &c.CreatedBy, &c.CreatedAt, &inviteOnly, &c.OwnerID)
	c.InviteOnly = inviteOnly != 0
	return err
}

// 房主（OwnerID/CreatedBy）的权威是 channel_members 的 owner 行，channels.created_by
// 只作历史记录兜底（迁移前的库在游标 v5 之后才一致，COALESCE 保证期间读出旧值）。
const channelCols = `c.id, c.name, COALESCE(ou.username, ''), c.created_at, c.invite_only, COALESCE(om.user_id, c.created_by)`
const channelJoins = ` FROM channels c
LEFT JOIN channel_members om ON om.channel_id = c.id AND om.role = 'owner'
LEFT JOIN users ou ON ou.id = COALESCE(om.user_id, c.created_by)`

func (s *Store) CreateChannel(ctx context.Context, name string, userID int64) (*Channel, error) {
	row := &channelRow{Name: name, CreatedBy: userID}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	// 归属的权威行：建频道即写 owner 成员行
	if err := s.SetChannelRole(ctx, row.ID, userID, ChannelRoleOwner); err != nil {
		return nil, err
	}
	return s.ChannelByID(ctx, row.ID)
}

// ChannelByID 按 ID 取频道完整信息（含房主与邀请制标记）。
func (s *Store) ChannelByID(ctx context.Context, id int64) (*Channel, error) {
	var c Channel
	err := scanChannel(s.bun.QueryRowContext(ctx,
		`SELECT `+channelCols+channelJoins+` WHERE c.id = ?`, id), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.bun.QueryContext(ctx,
		`SELECT `+channelCols+channelJoins+` ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := scanChannel(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ChannelByName(ctx context.Context, name string) (*Channel, error) {
	var c Channel
	err := scanChannel(s.bun.QueryRowContext(ctx,
		`SELECT `+channelCols+channelJoins+` WHERE c.name = ?`, name), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) SetInviteOnly(ctx context.Context, channelID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.bun.NewRaw("UPDATE channels SET invite_only = ? WHERE id = ?", v, channelID).Exec(ctx)
	return err
}

// ---- 封禁 ----

// IsBanned 用户是否被该频道封禁。
func (s *Store) IsBanned(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.bun.NewRaw(
		"SELECT COUNT(1) FROM channel_bans WHERE channel_id = ? AND user_id = ?", channelID, userID).Scan(ctx, &n)
	return n > 0, err
}

func (s *Store) Ban(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewInsert().Model(&channelBanRow{ChannelID: channelID, UserID: userID}).Ignore().Exec(ctx)
	return err
}

func (s *Store) Unban(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewRaw(
		"DELETE FROM channel_bans WHERE channel_id = ? AND user_id = ?", channelID, userID).Exec(ctx)
	return err
}

// ListBans 返回该频道被封禁的用户名列表。
func (s *Store) ListBans(ctx context.Context, channelID int64) ([]UserRef, error) {
	return s.listUserRefs(ctx, "channel_bans", channelID)
}

// ---- 禁言 ----

// IsGagged 用户是否被该频道禁言。
func (s *Store) IsGagged(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.bun.NewRaw(
		"SELECT COUNT(1) FROM channel_gags WHERE channel_id = ? AND user_id = ?", channelID, userID).Scan(ctx, &n)
	return n > 0, err
}

func (s *Store) Gag(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewInsert().Model(&channelGagRow{ChannelID: channelID, UserID: userID}).Ignore().Exec(ctx)
	return err
}

func (s *Store) Ungag(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewRaw(
		"DELETE FROM channel_gags WHERE channel_id = ? AND user_id = ?", channelID, userID).Exec(ctx)
	return err
}

// ListGags 返回该频道被禁言的用户名列表。
func (s *Store) ListGags(ctx context.Context, channelID int64) ([]UserRef, error) {
	return s.listUserRefs(ctx, "channel_gags", channelID)
}

// ---- 成员（邀请制白名单）----

// IsMember 用户是否在该频道白名单内。
func (s *Store) IsMember(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.bun.NewRaw(
		"SELECT COUNT(1) FROM channel_members WHERE channel_id = ? AND user_id = ?", channelID, userID).Scan(ctx, &n)
	return n > 0, err
}

func (s *Store) AddMember(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewInsert().Model(&channelMemberRow{ChannelID: channelID, UserID: userID}).Ignore().Exec(ctx)
	return err
}

func (s *Store) RemoveMember(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewRaw(
		"DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?", channelID, userID).Exec(ctx)
	return err
}

// ListMembers 返回该频道全部成员行（owner/moderator/member 都带角色）。
func (s *Store) ListMembers(ctx context.Context, channelID int64) ([]UserRef, error) {
	out := []UserRef{}
	rows, err := s.bun.QueryContext(ctx, `
SELECT u.id, u.username, t.role FROM channel_members t JOIN users u ON u.id = t.user_id
WHERE t.channel_id = ? ORDER BY t.created_at`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref UserRef
		if err := rows.Scan(&ref.ID, &ref.Username, &ref.Role); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// UserRef 名单条目：id 是操作目标（解禁/移出白名单按它发），username 只用于展示；
// Role 仅 ListMembers 填充（channel_members 的角色列），其余名单为空。
type UserRef struct {
	ID       int64       `json:"id"`
	Username string      `json:"username"`
	Role     ChannelRole `json:"role,omitempty"`
}

// listUserRefs 取 channel_bans/channel_gags/channel_members 的名单（三表结构相同）。
func (s *Store) listUserRefs(ctx context.Context, table string, channelID int64) ([]UserRef, error) {
	out := []UserRef{}
	rows, err := s.bun.QueryContext(ctx, `
SELECT u.id, u.username FROM `+table+` t JOIN users u ON u.id = t.user_id
WHERE t.channel_id = ? ORDER BY t.created_at`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref UserRef
		if err := rows.Scan(&ref.ID, &ref.Username); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// CanJoin 进入权限：被封禁拒绝；邀请制频道仅房主与白名单可进（房主天然豁免）。
func (s *Store) CanJoin(ctx context.Context, c *Channel, userID int64) (bool, string, error) {
	banned, err := s.IsBanned(ctx, c.ID, userID)
	if err != nil {
		return false, "", err
	}
	if banned {
		return false, "已被该频道封禁", nil
	}
	if c.InviteOnly && userID != c.OwnerID {
		member, err := s.IsMember(ctx, c.ID, userID)
		if err != nil {
			return false, "", err
		}
		if !member {
			return false, "该频道为邀请制，未在白名单内", nil
		}
	}
	return true, "", nil
}

// ---- 消息 ----

func (s *Store) AddMessage(ctx context.Context, channelID, userID int64, content string) (*Message, error) {
	row := &messageRow{ChannelID: channelID, UserID: userID, Content: content}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	var m Message
	err := s.bun.NewRaw(`
SELECT m.id, m.channel_id, m.user_id, u.username, m.content, m.created_at
FROM messages m JOIN users u ON u.id = m.user_id WHERE m.id = ?`, row.ID).
		Scan(ctx, &m.ID, &m.ChannelID, &m.UserID, &m.Username, &m.Content, &m.CreatedAt)
	return &m, err
}

// RecentMessages 返回频道最近 limit 条消息（按时间正序）。
func (s *Store) RecentMessages(ctx context.Context, channelID int64, limit int) ([]Message, error) {
	rows, err := s.bun.QueryContext(ctx, `
SELECT m.id, m.channel_id, m.user_id, u.username, m.content, m.created_at
FROM messages m JOIN users u ON u.id = m.user_id
WHERE m.channel_id = ? ORDER BY m.id DESC LIMIT ?`, channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.UserID, &m.Username, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	// 反转为时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// IsUniqueViolation 判断唯一约束冲突（用户名/频道名重复），覆盖三种方言的错误文案。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint failed") || // sqlite
		strings.Contains(msg, "duplicate entry") || // mysql
		strings.Contains(msg, "duplicate key") // postgres
}
