// baseline 迁移验收：存量库升级只多 bun_migrations/bun_migration_locks 两张表，数据无损；
// 新库由模型生成全套 schema 并可正常读写。
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/migrate"
)

// legacyDDL 是 Bun 接管前的 sqlite schema，且故意不含 compat 四条加列
// （invite_only/is_admin/disabled/provider），模拟更老的存量库走 ALTER 升级路径。
var legacyDDL = []string{
	`CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  expires_at DATETIME NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE channels (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT UNIQUE NOT NULL,
  created_by INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  content TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
	`CREATE INDEX idx_messages_channel ON messages(channel_id, id)`,
	`CREATE TABLE devices (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id),
  device_id TEXT NOT NULL,
  tag TEXT NOT NULL DEFAULT '',
  first_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
  last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(user_id, device_id)
)`,
	`CREATE TABLE ingresses (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id),
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  ingress_id TEXT NOT NULL,
  stream_key TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(user_id, channel_id)
)`,
	`CREATE TABLE channel_bans (
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE channel_gags (
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE channel_members (
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(channel_id, user_id)
)`,
	`CREATE TABLE invites (
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
	`CREATE TABLE settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
)`,
	`CREATE TABLE providers (
  alias VARCHAR(64) PRIMARY KEY,
  type VARCHAR(32) NOT NULL,
  params TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`,
}

// tableNames 取库内全部用户表名（升序），含 bun_* 表。
func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(
		"SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

// columnNames 取一张表的全部列名。
func columnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

func migrationRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(1) FROM bun_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 存量库升级：建一份旧版 schema（缺 compat 列）的 sqlite 库并写入数据，
// 走新 Open 后表集合只多两张 bun_* 表、compat 列补齐、数据无损；再次 Open 为空操作。
func TestOpenUpgradesLegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range legacyDDL {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("建旧库失败: %v\nSQL: %s", err, stmt)
		}
	}
	if _, err := raw.Exec(
		"INSERT INTO users (username, password_hash) VALUES ('alice', 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		"INSERT INTO channels (name, created_by) VALUES ('general', 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		"INSERT INTO messages (channel_id, user_id, content) VALUES (1, 1, 'hello')"); err != nil {
		t.Fatal(err)
	}
	before := tableNames(t, raw)
	raw.Close()

	s, err := Open("sqlite:" + path)
	if err != nil {
		t.Fatalf("存量库升级失败: %v", err)
	}

	// schema 对比：只多 bun_migrations / bun_migration_locks
	after := tableNames(t, s.bun.DB)
	want := append(slices.Clone(before), "bun_migration_locks", "bun_migrations")
	slices.Sort(want)
	if !slices.Equal(after, want) {
		t.Fatalf("升级后表集合不符:\n got %v\nwant %v", after, want)
	}

	// compat 加列生效
	for table, cols := range map[string][]string{
		"users":     {"is_admin", "disabled"},
		"channels":  {"invite_only"},
		"ingresses": {"provider"},
	} {
		have := columnNames(t, s.bun.DB, table)
		for _, c := range cols {
			if !slices.Contains(have, c) {
				t.Fatalf("%s 缺列 %s，实际列: %v", table, c, have)
			}
		}
	}

	// 数据无损
	ctx := context.Background()
	u, hash, err := s.UserByName(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 1 || hash != "x" || u.IsAdmin || u.Disabled {
		t.Fatalf("用户数据不符: %+v hash=%s", u, hash)
	}
	c, err := s.ChannelByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "general" || c.CreatedBy != "alice" || c.InviteOnly {
		t.Fatalf("频道数据不符: %+v", c)
	}
	msgs, err := s.RecentMessages(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hello" || msgs[0].CreatedAt.IsZero() {
		t.Fatalf("消息数据不符: %+v", msgs)
	}
	if n := migrationRows(t, s.bun.DB); n != 1 {
		t.Fatalf("bun_migrations 应有 1 行，实际 %d", n)
	}
	s.Close()

	// 再次 Open 是空操作（baseline 已登记）
	s2, err := Open("sqlite:" + path)
	if err != nil {
		t.Fatalf("重复 Open 失败: %v", err)
	}
	if n := migrationRows(t, s2.bun.DB); n != 1 {
		t.Fatalf("重复 Open 后 bun_migrations 应仍为 1 行，实际 %d", n)
	}
	s2.Close()
}

// 新库：全套 schema 由模型生成，写入读取正常（自增回填、时间默认值、唯一约束）。
func TestOpenFreshDB(t *testing.T) {
	s, err := Open("sqlite:" + filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	want := []string{
		"bun_migration_locks", "bun_migrations",
		"channel_bans", "channel_gags", "channel_members", "channels", "devices",
		"ingresses", "invites", "messages", "providers", "sessions", "settings", "users",
	}
	if got := tableNames(t, s.bun.DB); !slices.Equal(got, want) {
		t.Fatalf("新库表集合不符:\n got %v\nwant %v", got, want)
	}

	ctx := context.Background()
	u, err := s.CreateUser(ctx, "bob", "h")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 1 || !u.IsAdmin {
		t.Fatalf("首个用户应为管理员且 ID 回填: %+v", u)
	}
	c, err := s.CreateChannel(ctx, "room", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != 1 || c.CreatedAt.IsZero() {
		t.Fatalf("频道创建异常: %+v", c)
	}
	m, err := s.AddMessage(ctx, c.ID, u.ID, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != 1 || m.CreatedAt.IsZero() {
		t.Fatalf("消息创建异常: %+v", m)
	}
	if err := s.RecordDevice(ctx, u.ID, "dev1", "tag"); err != nil {
		t.Fatal(err)
	}
	devs, err := s.ListDevices(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].FirstSeen.IsZero() || devs[0].LastSeen.IsZero() {
		t.Fatalf("设备档案异常: %+v", devs)
	}
	if err := s.SetSetting(ctx, "k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSetting(ctx, "k1"); err != nil || v != "v1" {
		t.Fatalf("settings 读写异常: %v %q", err, v)
	}
	if _, err := s.CreateUser(ctx, "bob", "h2"); !IsUniqueViolation(err) {
		t.Fatalf("唯一约束应触发，实际: %v", err)
	}
}

// WithMarkAppliedOnSuccess 语义验证：迁移 Up 中途失败不得登记 bun_migrations，
// 否则下次启动会永久跳过该迁移、库停在半迁移态（Open 里的 Migrator 已带此选项）。
func TestMigrationFailureNotMarkedApplied(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	bunDB := bun.NewDB(raw, sqlitedialect.New())
	defer bunDB.Close()

	// 第一条语句成功、第二条语法错误，制造"执行到一半失败"
	ms := migrate.NewMigrations()
	err = ms.Discover(fstest.MapFS{
		"00001_fail.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE t_half (id INTEGER);\nTHIS IS NOT SQL;")},
		"00001_fail.down.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	})
	if err != nil {
		t.Fatal(err)
	}
	migrator := migrate.NewMigrator(bunDB, ms, migrate.WithMarkAppliedOnSuccess(true))
	if err := migrator.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(ctx); err == nil {
		t.Fatal("坏迁移应执行失败")
	}
	var n int
	if err := raw.QueryRow("SELECT COUNT(1) FROM bun_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("失败的迁移不应登记进 bun_migrations，实际 %d 行", n)
	}
}
