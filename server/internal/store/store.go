// 存储层：用户、会话、频道、聊天消息、设备、ingress、封禁、白名单。
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
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("记录不存在")

// ---- 方言 ----

type dialect struct {
	name string // sqlite | mysql | postgres
}

// rebind 把 ? 占位符转换为方言占位符（postgres 用 $1..$n，其余原样）。
func (d dialect) rebind(q string) string {
	if d.name != "postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ignore 把 INSERT 语句变为"重复则忽略"的方言写法。
func (d dialect) ignore(insertSQL string) string {
	switch d.name {
	case "mysql":
		return strings.Replace(insertSQL, "INSERT", "INSERT IGNORE", 1)
	case "postgres":
		return insertSQL + " ON CONFLICT DO NOTHING"
	default:
		return strings.Replace(insertSQL, "INSERT", "INSERT OR IGNORE", 1)
	}
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
	db *sql.DB
	d  dialect
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
	s := &Store{db: db, d: d}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// q 是 rebind 的简写。
func (s *Store) q(query string) string { return s.d.rebind(query) }

// insertID 执行 INSERT 并返回自增主键：postgres 用 RETURNING id（pgx 不支持 LastInsertId），
// sqlite/mysql 用 LastInsertId。
func (s *Store) insertID(ctx context.Context, insertSQL string, args ...any) (int64, error) {
	if s.d.name == "postgres" {
		var id int64
		err := s.db.QueryRowContext(ctx, s.q(insertSQL)+" RETURNING id", args...).Scan(&id)
		return id, err
	}
	res, err := s.db.ExecContext(ctx, s.q(insertSQL), args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---- schema（按方言生成）----

func (s *Store) migrate() error {
	var ddl []string
	switch s.d.name {
	case "mysql":
		ddl = mysqlDDL
	case "postgres":
		ddl = pgDDL
	default:
		ddl = sqliteDDL
	}
	for _, stmt := range ddl {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w\nSQL: %s", err, stmt)
		}
	}
	// 老库兼容加列：重复列则忽略
	compat := []string{
		`ALTER TABLE channels ADD COLUMN invite_only INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE ingresses ADD COLUMN provider VARCHAR(32) NOT NULL DEFAULT 'livekit'`,
	}
	for _, stmt := range compat {
		if _, err := s.db.Exec(stmt); err != nil {
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "duplicate column") && !strings.Contains(msg, "already exists") {
				return err
			}
		}
	}
	return nil
}

var sqliteDDL = []string{
	`CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  expires_at DATETIME NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS channels (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT UNIQUE NOT NULL,
  created_by INTEGER NOT NULL REFERENCES users(id),
  invite_only INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  content TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_channel ON messages(channel_id, id)`,
	`CREATE TABLE IF NOT EXISTS devices (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id),
  device_id TEXT NOT NULL,
  tag TEXT NOT NULL DEFAULT '',
  first_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
  last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(user_id, device_id)
)`,
	`CREATE TABLE IF NOT EXISTS ingresses (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id),
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  ingress_id TEXT NOT NULL,
  stream_key TEXT NOT NULL,
  provider VARCHAR(32) NOT NULL DEFAULT 'livekit',
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(user_id, channel_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_bans (
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_gags (
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_members (
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS invites (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  code TEXT UNIQUE NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  max_uses INTEGER NOT NULL DEFAULT 1,
  used INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0,
  created_by INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  expires_at DATETIME NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS providers (
  alias VARCHAR(64) PRIMARY KEY,
  type VARCHAR(32) NOT NULL,
  params TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
}

var mysqlDDL = []string{
	`CREATE TABLE IF NOT EXISTS users (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  username VARCHAR(64) UNIQUE NOT NULL,
  password_hash VARCHAR(255) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS sessions (
  token VARCHAR(128) PRIMARY KEY,
  user_id BIGINT NOT NULL,
  expires_at DATETIME NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS channels (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  name VARCHAR(128) UNIQUE NOT NULL,
  created_by BIGINT NOT NULL,
  invite_only TINYINT NOT NULL DEFAULT 0,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS messages (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  channel_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  content TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_messages_channel (channel_id, id)
)`,
	`CREATE TABLE IF NOT EXISTS devices (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  user_id BIGINT NOT NULL,
  device_id VARCHAR(32) NOT NULL,
  tag VARCHAR(64) NOT NULL DEFAULT '',
  first_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
  last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_devices (user_id, device_id)
)`,
	`CREATE TABLE IF NOT EXISTS ingresses (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  user_id BIGINT NOT NULL,
  channel_id BIGINT NOT NULL,
  ingress_id VARCHAR(128) NOT NULL,
  stream_key VARCHAR(128) NOT NULL,
  provider VARCHAR(32) NOT NULL DEFAULT 'livekit',
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_ingresses (user_id, channel_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_bans (
  channel_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_channel_bans (channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_gags (
  channel_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_channel_gags (channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_members (
  channel_id BIGINT NOT NULL,
  user_id BIGINT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_channel_members (channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS invites (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  code VARCHAR(32) UNIQUE NOT NULL,
  note VARCHAR(255) NOT NULL DEFAULT '',
  max_uses INT NOT NULL DEFAULT 1,
  used INT NOT NULL DEFAULT 0,
  revoked TINYINT NOT NULL DEFAULT 0,
  created_by BIGINT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  expires_at DATETIME NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS settings (
  k VARCHAR(64) PRIMARY KEY,
  v TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS providers (
  alias VARCHAR(64) PRIMARY KEY,
  type VARCHAR(32) NOT NULL,
  params TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
}

var pgDDL = []string{
	`CREATE TABLE IF NOT EXISTS users (
  id BIGSERIAL PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id),
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS channels (
  id BIGSERIAL PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  created_by BIGINT NOT NULL REFERENCES users(id),
  invite_only INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE IF NOT EXISTS messages (
  id BIGSERIAL PRIMARY KEY,
  channel_id BIGINT NOT NULL REFERENCES channels(id),
  user_id BIGINT NOT NULL REFERENCES users(id),
  content TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_channel ON messages(channel_id, id)`,
	`CREATE TABLE IF NOT EXISTS devices (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id),
  device_id TEXT NOT NULL,
  tag TEXT NOT NULL DEFAULT '',
  first_seen TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  last_seen TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(user_id, device_id)
)`,
	`CREATE TABLE IF NOT EXISTS ingresses (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id),
  channel_id BIGINT NOT NULL REFERENCES channels(id),
  ingress_id TEXT NOT NULL,
  stream_key TEXT NOT NULL,
  provider VARCHAR(32) NOT NULL DEFAULT 'livekit',
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(user_id, channel_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_bans (
  channel_id BIGINT NOT NULL REFERENCES channels(id),
  user_id BIGINT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_gags (
  channel_id BIGINT NOT NULL REFERENCES channels(id),
  user_id BIGINT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS channel_members (
  channel_id BIGINT NOT NULL REFERENCES channels(id),
  user_id BIGINT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE IF NOT EXISTS invites (
  id BIGSERIAL PRIMARY KEY,
  code TEXT UNIQUE NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  max_uses INTEGER NOT NULL DEFAULT 1,
  used INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0,
  created_by BIGINT NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  expires_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS providers (
  alias VARCHAR(64) PRIMARY KEY,
  type VARCHAR(32) NOT NULL,
  params TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
)`,
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
	Disabled bool   `json:"-"` // 停用账号（管理后台用，登录/会话校验时拦截）
}

type Channel struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	InviteOnly bool      `json:"invite_only"`
	IsOwner    bool      `json:"is_owner"` // 对当前请求用户是否房主（接口层填充）
	Online     int       `json:"online"`   // 当前在房人数（接口层从 LiveKit 填充）
	OwnerID    int64     `json:"-"`        // 房主用户 ID（内部用）
}

type Message struct {
	ID        int64     `json:"id"`
	ChannelID int64     `json:"channel_id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- 设备 ----

// RecordDevice 记录用户设备：首次出现建档，之后刷新 last_seen 与设备标签。
func (s *Store) RecordDevice(ctx context.Context, userID int64, deviceID, tag string) error {
	var query string
	switch s.d.name {
	case "mysql":
		query = `INSERT INTO devices (user_id, device_id, tag) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE last_seen = CURRENT_TIMESTAMP, tag = VALUES(tag)`
	case "postgres":
		query = `INSERT INTO devices (user_id, device_id, tag) VALUES (?, ?, ?)
ON CONFLICT(user_id, device_id) DO UPDATE SET last_seen = CURRENT_TIMESTAMP, tag = excluded.tag`
	default:
		query = `INSERT INTO devices (user_id, device_id, tag) VALUES (?, ?, ?)
ON CONFLICT(user_id, device_id) DO UPDATE SET last_seen = CURRENT_TIMESTAMP, tag = excluded.tag`
	}
	_, err := s.db.ExecContext(ctx, s.q(query), userID, deviceID, tag)
	return err
}

// ---- Ingress（每用户每频道一个 OBS WHIP 推流端点）----

type Ingress struct {
	ID        int64     `json:"id"`
	IngressID string    `json:"ingress_id"`
	StreamKey string    `json:"stream_key"`
	Provider  string    `json:"provider"` // 创建该端点的推流入口内核名（删除/失效判断按归属方路由）
	CreatedAt time.Time `json:"created_at"`
}

// IngressByUserChannel 查该用户在该频道的 ingress，无记录返回 ErrNotFound。
func (s *Store) IngressByUserChannel(ctx context.Context, userID, channelID int64) (*Ingress, error) {
	var in Ingress
	err := s.db.QueryRowContext(ctx, s.q(`
SELECT id, ingress_id, stream_key, provider, created_at FROM ingresses
WHERE user_id = ? AND channel_id = ?`), userID, channelID).
		Scan(&in.ID, &in.IngressID, &in.StreamKey, &in.Provider, &in.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &in, err
}

func (s *Store) CreateIngress(ctx context.Context, userID, channelID int64, ingressID, streamKey, provider string) (*Ingress, error) {
	id, err := s.insertID(ctx, `
INSERT INTO ingresses (user_id, channel_id, ingress_id, stream_key, provider) VALUES (?, ?, ?, ?, ?)`,
		userID, channelID, ingressID, streamKey, provider)
	if err != nil {
		return nil, err
	}
	return &Ingress{ID: id, IngressID: ingressID, StreamKey: streamKey, Provider: provider}, nil
}

// IngressOwner 按推流密钥反查归属（含记录的归属实例 alias，WHIP 入口按它校验入口匹配）。
func (s *Store) IngressOwner(ctx context.Context, streamKey string) (userID, channelID int64, provider string, err error) {
	err = s.db.QueryRowContext(ctx, s.q(
		"SELECT user_id, channel_id, provider FROM ingresses WHERE stream_key = ?"), streamKey).
		Scan(&userID, &channelID, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, "", ErrNotFound
	}
	return userID, channelID, provider, err
}

func (s *Store) DeleteIngress(ctx context.Context, userID, channelID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(
		"DELETE FROM ingresses WHERE user_id = ? AND channel_id = ?"), userID, channelID)
	return err
}

// ---- 用户 ----

// CreateUser 创建用户；服务器上的第一个账号自动成为管理员。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM users").Scan(&n); err != nil {
		return nil, err
	}
	isAdmin := 0
	if n == 0 {
		isAdmin = 1
	}
	id, err := s.insertID(ctx, "INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, ?)",
		username, passwordHash, isAdmin)
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username, IsAdmin: isAdmin == 1}, nil
}

func (s *Store) UserByName(ctx context.Context, username string) (*User, string, error) {
	var u User
	var hash string
	var isAdmin, disabled int64
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT id, username, password_hash, is_admin, disabled FROM users WHERE username = ?"), username).
		Scan(&u.ID, &u.Username, &hash, &isAdmin, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	u.IsAdmin = isAdmin != 0
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
	_, err := s.db.ExecContext(ctx, s.q(
		"INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)"),
		token, userID, time.Now().Add(sessionTTL))
	return token, err
}

// UserByToken 校验会话 token，过期、不存在或账号已停用返回 ErrNotFound。
func (s *Store) UserByToken(ctx context.Context, token string) (*User, error) {
	var u User
	var isAdmin int64
	err := s.db.QueryRowContext(ctx, s.q(`
SELECT u.id, u.username, u.is_admin FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token = ? AND s.expires_at > ? AND u.disabled = 0`), token, time.Now()).
		Scan(&u.ID, &u.Username, &isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.IsAdmin = isAdmin != 0
	return &u, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM sessions WHERE token = ?"), token)
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

const channelCols = `c.id, c.name, u.username, c.created_at, c.invite_only, c.created_by`

func (s *Store) CreateChannel(ctx context.Context, name string, userID int64) (*Channel, error) {
	id, err := s.insertID(ctx, "INSERT INTO channels (name, created_by) VALUES (?, ?)", name, userID)
	if err != nil {
		return nil, err
	}
	return s.ChannelByID(ctx, id)
}

// ChannelByID 按 ID 取频道完整信息（含房主与邀请制标记）。
func (s *Store) ChannelByID(ctx context.Context, id int64) (*Channel, error) {
	var c Channel
	err := scanChannel(s.db.QueryRowContext(ctx, s.q(`
SELECT `+channelCols+` FROM channels c
JOIN users u ON u.id = c.created_by WHERE c.id = ?`), id), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
SELECT `+channelCols+` FROM channels c
JOIN users u ON u.id = c.created_by ORDER BY c.id`))
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
	err := scanChannel(s.db.QueryRowContext(ctx, s.q(`
SELECT `+channelCols+` FROM channels c
JOIN users u ON u.id = c.created_by WHERE c.name = ?`), name), &c)
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
	_, err := s.db.ExecContext(ctx, s.q("UPDATE channels SET invite_only = ? WHERE id = ?"), v, channelID)
	return err
}

// ---- 封禁 ----

// IsBanned 用户是否被该频道封禁。
func (s *Store) IsBanned(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT COUNT(1) FROM channel_bans WHERE channel_id = ? AND user_id = ?"), channelID, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) Ban(ctx context.Context, channelID, userID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(s.d.ignore(
		"INSERT INTO channel_bans (channel_id, user_id) VALUES (?, ?)")), channelID, userID)
	return err
}

func (s *Store) Unban(ctx context.Context, channelID, userID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(
		"DELETE FROM channel_bans WHERE channel_id = ? AND user_id = ?"), channelID, userID)
	return err
}

// ListBans 返回该频道被封禁的用户名列表。
func (s *Store) ListBans(ctx context.Context, channelID int64) ([]string, error) {
	return s.listUsernames(ctx, "channel_bans", channelID)
}

// ---- 禁言 ----

// IsGagged 用户是否被该频道禁言。
func (s *Store) IsGagged(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT COUNT(1) FROM channel_gags WHERE channel_id = ? AND user_id = ?"), channelID, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) Gag(ctx context.Context, channelID, userID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(s.d.ignore(
		"INSERT INTO channel_gags (channel_id, user_id) VALUES (?, ?)")), channelID, userID)
	return err
}

func (s *Store) Ungag(ctx context.Context, channelID, userID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(
		"DELETE FROM channel_gags WHERE channel_id = ? AND user_id = ?"), channelID, userID)
	return err
}

// ListGags 返回该频道被禁言的用户名列表。
func (s *Store) ListGags(ctx context.Context, channelID int64) ([]string, error) {
	return s.listUsernames(ctx, "channel_gags", channelID)
}

// ---- 成员（邀请制白名单）----

// IsMember 用户是否在该频道白名单内。
func (s *Store) IsMember(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.q(
		"SELECT COUNT(1) FROM channel_members WHERE channel_id = ? AND user_id = ?"), channelID, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) AddMember(ctx context.Context, channelID, userID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(s.d.ignore(
		"INSERT INTO channel_members (channel_id, user_id) VALUES (?, ?)")), channelID, userID)
	return err
}

func (s *Store) RemoveMember(ctx context.Context, channelID, userID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(
		"DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?"), channelID, userID)
	return err
}

// ListMembers 返回该频道白名单用户名列表。
func (s *Store) ListMembers(ctx context.Context, channelID int64) ([]string, error) {
	return s.listUsernames(ctx, "channel_members", channelID)
}

// listUsernames 取 channel_bans/channel_gags/channel_members 里的用户名（三表结构相同）。
func (s *Store) listUsernames(ctx context.Context, table string, channelID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
SELECT u.username FROM `+table+` t JOIN users u ON u.id = t.user_id
WHERE t.channel_id = ? ORDER BY t.created_at`), channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
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
	id, err := s.insertID(ctx,
		"INSERT INTO messages (channel_id, user_id, content) VALUES (?, ?, ?)", channelID, userID, content)
	if err != nil {
		return nil, err
	}
	var m Message
	err = s.db.QueryRowContext(ctx, s.q(`
SELECT m.id, m.channel_id, u.username, m.content, m.created_at
FROM messages m JOIN users u ON u.id = m.user_id WHERE m.id = ?`), id).
		Scan(&m.ID, &m.ChannelID, &m.Username, &m.Content, &m.CreatedAt)
	return &m, err
}

// RecentMessages 返回频道最近 limit 条消息（按时间正序）。
func (s *Store) RecentMessages(ctx context.Context, channelID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
SELECT m.id, m.channel_id, u.username, m.content, m.created_at
FROM messages m JOIN users u ON u.id = m.user_id
WHERE m.channel_id = ? ORDER BY m.id DESC LIMIT ?`), channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.Username, &m.Content, &m.CreatedAt); err != nil {
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
