// 方言矩阵测试：同一套用例在三种方言上跑。sqlite 用临时文件始终跑；
// 设了 HEARTH_TEST_MYSQL_URL / HEARTH_TEST_PG_URL（指向有建库权限的实例）时
// mysql/postgres 各起一个独立子测试，每个子测试建一个一次性数据库、跑完 DROP。
// 本地验收命令见 server/README.md「测试」节。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// forEachStore 对方言矩阵逐方言开全新库执行 fn（dbURL 供需要重开连接的用例使用）；
// mysql/pg 未配环境变量时对应子测试 Skip。
func forEachStore(t *testing.T, fn func(t *testing.T, s *Store, dbURL string)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		dbURL := "sqlite://" + filepath.Join(t.TempDir(), "test.db")
		s, err := Open(dbURL)
		if err != nil {
			t.Fatalf("打开测试库失败: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		fn(t, s, dbURL)
	})
	t.Run("mysql", func(t *testing.T) {
		raw := os.Getenv("HEARTH_TEST_MYSQL_URL")
		if raw == "" {
			t.Skip("未设 HEARTH_TEST_MYSQL_URL")
		}
		s, dbURL := openRemoteTestStore(t, raw)
		fn(t, s, dbURL)
	})
	t.Run("postgres", func(t *testing.T) {
		raw := os.Getenv("HEARTH_TEST_PG_URL")
		if raw == "" {
			t.Skip("未设 HEARTH_TEST_PG_URL")
		}
		s, dbURL := openRemoteTestStore(t, raw)
		fn(t, s, dbURL)
	})
}

// openRemoteTestStore 在 mysql/pg 实例上建一个一次性数据库并 Open 指向它；
// 测试结束关库并 DROP，不污染实例。
func openRemoteTestStore(t *testing.T, rawURL string) (*Store, string) {
	t.Helper()
	admin, storeURL, drop := freshDatabase(t, rawURL)
	s, err := Open(storeURL)
	if err != nil {
		drop()
		admin.Close()
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		drop()
		admin.Close()
	})
	return s, storeURL
}

// freshDatabase 解析实例 URL，用管理连接建一次性库，返回管理连接、指向新库的
// DATABASE_URL 与清理函数。库名只用十六进制字符，可直接拼接无需转义。
func freshDatabase(t *testing.T, rawURL string) (admin *sql.DB, storeURL string, drop func()) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析测试库 URL 失败: %v", err)
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	name := "htest_" + hex.EncodeToString(buf)

	switch u.Scheme {
	case "mysql":
		auth := u.User.Username()
		if pw, ok := u.User.Password(); ok {
			auth += ":" + pw
		}
		admin, err = sql.Open("mysql", auth+"@tcp("+u.Host+")/")
	case "postgres", "postgresql":
		adminURL := *u
		adminURL.Path = "/postgres"
		admin, err = sql.Open("pgx", adminURL.String())
	default:
		t.Fatalf("不支持的测试库 URL: %s", rawURL)
	}
	if err != nil {
		t.Fatalf("管理连接失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("建一次性库失败: %v", err)
	}
	storeU := *u
	storeU.Path = "/" + name
	drop = func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Errorf("清理一次性库失败: %v", err)
		}
	}
	return admin, storeU.String(), drop
}

// remoteTableNames 取当前库全部表名（升序），mysql/pg 通用。
func remoteTableNames(t *testing.T, db *sql.DB, dialectName string) []string {
	t.Helper()
	var q string
	if dialectName == "mysql" {
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name"
	} else {
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name"
	}
	rows, err := db.Query(q)
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

// remoteMigrationRows 数 bun_migrations 行数（当前应恰登记 baseline 与 00002 两行）。
func remoteMigrationRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(1) FROM bun_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- 自增主键回填（CreateUser/CreateChannel/CreateIngestToken/AddMessage/CreateInvite）----

func TestDialectAutoincrementBackfill(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		u, err := s.CreateUser(ctx, "alice", "h")
		if err != nil {
			t.Fatal(err)
		}
		if u.ID <= 0 || !u.IsAdmin {
			t.Fatalf("首用户 ID 应回填且为管理员: %+v", u)
		}
		c, err := s.CreateChannel(ctx, "general", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c.ID <= 0 {
			t.Fatalf("频道 ID 未回填: %+v", c)
		}
		tk, err := s.CreateIngestToken(ctx, u.ID, "obs")
		if err != nil {
			t.Fatal(err)
		}
		if tk.ID <= 0 {
			t.Fatalf("推流令牌 ID 未回填: %+v", tk)
		}
		m, err := s.AddMessage(ctx, c.ID, u.ID, "hello")
		if err != nil {
			t.Fatal(err)
		}
		if m.ID <= 0 || m.Username != "alice" {
			t.Fatalf("消息 ID 未回填或 JOIN 异常: %+v", m)
		}
		inv, err := s.CreateInvite(ctx, u.ID, "n", 3, time.Hour, "")
		if err != nil {
			t.Fatal(err)
		}
		if inv.ID <= 0 {
			t.Fatalf("邀请 ID 未回填: %+v", inv)
		}
		// 回读按回填的 ID 能命中，证明回填值就是库里的主键
		if got, err := s.InviteByCode(ctx, inv.Code); err != nil || got.ID != inv.ID {
			t.Fatalf("邀请回读异常: %+v err=%v", got, err)
		}
	})
}

// ---- Ignore 重复插入（Ban/Gag/AddMember）----

func TestDialectIgnoreDuplicates(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		u, _ := s.CreateUser(ctx, "alice", "h")
		v, _ := s.CreateUser(ctx, "bob", "h")
		c, _ := s.CreateChannel(ctx, "general", u.ID)

		for i := 0; i < 2; i++ {
			if err := s.Ban(ctx, c.ID, v.ID); err != nil {
				t.Fatalf("第 %d 次 Ban 失败: %v", i+1, err)
			}
			if err := s.Gag(ctx, c.ID, v.ID); err != nil {
				t.Fatalf("第 %d 次 Gag 失败: %v", i+1, err)
			}
			if err := s.AddMember(ctx, c.ID, v.ID); err != nil {
				t.Fatalf("第 %d 次 AddMember 失败: %v", i+1, err)
			}
		}
		if bans, _ := s.ListBans(ctx, c.ID); len(bans) != 1 {
			t.Fatalf("重复 Ban 应被 Ignore，实际名单: %v", bans)
		}
		if gags, _ := s.ListGags(ctx, c.ID); len(gags) != 1 {
			t.Fatalf("重复 Gag 应被 Ignore，实际名单: %v", gags)
		}
		// 名单含建频道时自动写入的 owner 行（alice）+ 重复 AddMember 被 Ignore 的 bob 一行
		if members, _ := s.ListMembers(ctx, c.ID); len(members) != 2 {
			t.Fatalf("重复 AddMember 应被 Ignore，实际名单: %v", members)
		}
	})
}

// ---- RecordDevice 二次调用刷新 last_seen 与 tag ----

func TestDialectRecordDeviceRefresh(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		u, _ := s.CreateUser(ctx, "alice", "h")
		if err := s.RecordDevice(ctx, u.ID, "dev1", "old"); err != nil {
			t.Fatal(err)
		}
		// 三方言的时间默认值都是秒级 CURRENT_TIMESTAMP，隔一秒以上才能观察到刷新
		time.Sleep(1100 * time.Millisecond)
		if err := s.RecordDevice(ctx, u.ID, "dev1", "new"); err != nil {
			t.Fatal(err)
		}
		devs, err := s.ListDevices(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(devs) != 1 {
			t.Fatalf("二次 RecordDevice 应 upsert 而非新增: %+v", devs)
		}
		d := devs[0]
		if d.Tag != "new" {
			t.Fatalf("tag 应被刷新为 new，实际 %q", d.Tag)
		}
		if d.FirstSeen.IsZero() || d.LastSeen.IsZero() {
			t.Fatalf("时间字段应非零: %+v", d)
		}
		if !d.LastSeen.After(d.FirstSeen) {
			t.Fatalf("last_seen 应晚于 first_seen: %+v", d)
		}
	})
}

// ---- SetSetting upsert ----

func TestDialectSetSettingUpsert(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		if err := s.SetSetting(ctx, "k1", "v1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSetting(ctx, "k1", "v2"); err != nil {
			t.Fatalf("二次 SetSetting（upsert 路径）失败: %v", err)
		}
		if v, err := s.GetSetting(ctx, "k1"); err != nil || v != "v2" {
			t.Fatalf("upsert 后应读回 v2，实际 %q err=%v", v, err)
		}
	})
}

// ---- 推流令牌（ingest_tokens / ingest_endpoints）----

func TestDialectIngestTokenLifecycle(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		u, err := s.CreateUser(ctx, "alice", "h")
		if err != nil {
			t.Fatal(err)
		}

		// 未创建前两条反查路径都 ErrNotFound
		if _, err := s.IngestTokenByUser(ctx, u.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("无令牌时 ByUser 应 ErrNotFound，实际 %v", err)
		}
		if _, err := s.IngestTokenByToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("无令牌时 ByToken 应 ErrNotFound，实际 %v", err)
		}

		tk, err := s.CreateIngestToken(ctx, u.ID, "obs")
		if err != nil {
			t.Fatal(err)
		}
		if tk.ID <= 0 || len(tk.Token) != 64 || tk.Tag != "obs" || tk.CreatedAt.IsZero() {
			t.Fatalf("令牌创建异常: %+v", tk)
		}
		// 两条反查路径命中同一条
		byUser, err := s.IngestTokenByUser(ctx, u.ID)
		if err != nil || byUser.Token != tk.Token {
			t.Fatalf("ByUser 反查异常: %+v err=%v", byUser, err)
		}
		byToken, err := s.IngestTokenByToken(ctx, tk.Token)
		if err != nil || byToken.UserID != u.ID {
			t.Fatalf("ByToken 反查异常: %+v err=%v", byToken, err)
		}
		// 每用户一把：重复创建触发唯一冲突
		if _, err := s.CreateIngestToken(ctx, u.ID, "obs"); !IsUniqueViolation(err) {
			t.Fatalf("重复创建应唯一冲突，实际 %v", err)
		}

		// 重置：token 变、tag 保留，旧 token 反查失效
		tk2, err := s.ResetIngestToken(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tk2.Token == tk.Token || tk2.Tag != "obs" || tk2.ID != tk.ID {
			t.Fatalf("重置后应换 token 保留 tag: %+v → %+v", tk, tk2)
		}
		if _, err := s.IngestTokenByToken(ctx, tk.Token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("旧 token 应失效，实际 %v", err)
		}

		// 改标签：tag 变、token 保留
		if err := s.UpdateIngestTokenTag(ctx, u.ID, "cam"); err != nil {
			t.Fatal(err)
		}
		got, err := s.IngestTokenByUser(ctx, u.ID)
		if err != nil || got.Tag != "cam" || got.Token != tk2.Token {
			t.Fatalf("改标签后应只变 tag: %+v err=%v", got, err)
		}
	})
}

// ---- 时间字段往返非零 ----

func TestDialectTimeRoundTrip(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		u, _ := s.CreateUser(ctx, "alice", "h")
		c, _ := s.CreateChannel(ctx, "general", u.ID)
		if c.CreatedAt.IsZero() {
			t.Fatalf("频道 created_at 往返为零: %+v", c)
		}
		m, _ := s.AddMessage(ctx, c.ID, u.ID, "hi")
		if m.CreatedAt.IsZero() {
			t.Fatalf("消息 created_at 往返为零: %+v", m)
		}
		inv, _ := s.CreateInvite(ctx, u.ID, "", 1, time.Hour, "")
		got, err := s.InviteByCode(ctx, inv.Code)
		if err != nil {
			t.Fatal(err)
		}
		if got.CreatedAt.IsZero() || got.ExpiresAt.IsZero() {
			t.Fatalf("邀请时间字段往返为零: %+v", got)
		}
		if !got.Alive(time.Now()) {
			t.Fatalf("未过期邀请应 Alive: %+v", got)
		}
		// 会话校验依赖 expires_at 的正确写入与时间比较
		tok, err := s.CreateSession(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UserByToken(ctx, tok); err != nil {
			t.Fatalf("有效会话应通过校验: %v", err)
		}
	})
}

// ---- CreateInvite 的 max_uses=0 落库（模型带 default:1，0 是合法业务值）----

func TestDialectCreateInviteMaxUsesZero(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		u, _ := s.CreateUser(ctx, "alice", "h")

		inv0, err := s.CreateInvite(ctx, u.ID, "unlimited", 0, time.Hour, "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.InviteByCode(ctx, inv0.Code)
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxUses != 0 {
			t.Fatalf("max_uses=0 应原样落库（而非 DEFAULT 1），实际 %d", got.MaxUses)
		}

		inv2, err := s.CreateInvite(ctx, u.ID, "two", 2, time.Hour, "")
		if err != nil {
			t.Fatal(err)
		}
		got2, err := s.InviteByCode(ctx, inv2.Code)
		if err != nil {
			t.Fatal(err)
		}
		if got2.MaxUses != 2 {
			t.Fatalf("max_uses=2 应落库为 2，实际 %d", got2.MaxUses)
		}
	})
}

// ---- baseline 在已建库上重复 Open 为空操作 ----

func TestBaselineReopenNoop(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, dbURL string) {
		ctx := context.Background()
		if _, err := s.CreateUser(ctx, "alice", "h"); err != nil {
			t.Fatal(err)
		}
		s.Close() // 关掉 forEachStore 的句柄，用同一 URL 重开

		s2, err := Open(dbURL)
		if err != nil {
			t.Fatalf("重复 Open 失败: %v", err)
		}
		if n := remoteMigrationRows(t, s2.bun.DB); n != 3 {
			t.Fatalf("重复 Open 后 bun_migrations 应仍为 3 行，实际 %d", n)
		}
		// 数据无损
		if _, _, err := s2.UserByName(ctx, "alice"); err != nil {
			t.Fatalf("重复 Open 后数据丢失: %v", err)
		}
		s2.Close()

		// forEachStore 的 Cleanup 会再 Close 一次旧句柄（幂等），远端库由 Cleanup 正常 DROP
	})
}

// ---- 存量库升级（mysql/pg）：旧版 schema 建库 → 新 Open → 只多 bun_* 两张表 ----
// 旧 DDL 取自 Bun 迁移前最后一个提交（6e8fce1）的手写 DDL，已含 providers 表、
// compat 四列与 idx_messages_channel——正好覆盖 baseline 建索引吞 duplicate key name 的分支。

var legacyMySQLDDL = []string{
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

var legacyPgDDL = []string{
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

// 存量库升级：用旧版 DDL 建库写数据，走新 Open 后表集合只多两张 bun_* 表、
// 数据无损、bun_migrations 恰 1 行；mysql 场景顺带验证 idx_messages_channel 已存在时
// baseline 建索引的 duplicate key name 吞错分支真实执行（否则 Open 必然报错）。
func TestLegacyUpgradeRemote(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		env  string
		ddl  []string
	}{
		{"mysql", "HEARTH_TEST_MYSQL_URL", legacyMySQLDDL},
		{"postgres", "HEARTH_TEST_PG_URL", legacyPgDDL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := os.Getenv(tc.env)
			if raw == "" {
				t.Skip("未设 " + tc.env)
			}
			admin, storeURL, drop := freshDatabase(t, raw)
			defer func() {
				drop()
				admin.Close()
			}()

			// 用旧版 schema 建库（管理连接直连新库）
			var legacy *sql.DB
			var err error
			if tc.name == "mysql" {
				u, _ := url.Parse(storeURL)
				auth := u.User.Username()
				if pw, ok := u.User.Password(); ok {
					auth += ":" + pw
				}
				legacy, err = sql.Open("mysql", auth+"@tcp("+u.Host+")"+u.Path)
			} else {
				legacy, err = sql.Open("pgx", storeURL)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range tc.ddl {
				if _, err := legacy.Exec(stmt); err != nil {
					t.Fatalf("建旧库失败: %v\nSQL: %s", err, stmt)
				}
			}
			if _, err := legacy.Exec(
				"INSERT INTO users (username, password_hash) VALUES ('alice', 'x')"); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(
				"INSERT INTO channels (name, created_by) VALUES ('general', 1)"); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.Exec(
				"INSERT INTO messages (channel_id, user_id, content) VALUES (1, 1, 'hello')"); err != nil {
				t.Fatal(err)
			}
			before := remoteTableNames(t, legacy, tc.name)
			legacy.Close()

			// 新 Open 升级存量库（mysql 此处必走吞 duplicate key name 分支：索引已存在）
			s, err := Open(storeURL)
			if err != nil {
				t.Fatalf("存量库升级失败: %v", err)
			}
			defer s.Close()

			after := remoteTableNames(t, s.bun.DB, tc.name)
			want := append(slices.Clone(before),
				"bun_migration_locks", "bun_migrations", "ingest_endpoints", "ingest_tokens")
			slices.Sort(want)
			if !slices.Equal(after, want) {
				t.Fatalf("升级后表集合不符:\n got %v\nwant %v", after, want)
			}
			if n := remoteMigrationRows(t, s.bun.DB); n != 3 {
				t.Fatalf("bun_migrations 应有 3 行，实际 %d", n)
			}

			// compat 加列生效：旧库 users 无 is_admin/disabled（baseline ALTER 补齐），
			// 无 role/expires_at/invite_id（00003 补齐）
			var colQ string
			if tc.name == "mysql" {
				colQ = `SELECT column_name FROM information_schema.columns
				 WHERE table_schema = DATABASE() AND table_name = 'users'`
			} else {
				colQ = `SELECT column_name FROM information_schema.columns
				 WHERE table_schema = 'public' AND table_name = 'users'`
			}
			rows, err := s.bun.DB.Query(colQ)
			if err != nil {
				t.Fatal(err)
			}
			var cols []string
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					t.Fatal(err)
				}
				cols = append(cols, name)
			}
			rows.Close()
			for _, wantCol := range []string{"is_admin", "disabled", "role", "expires_at", "invite_id"} {
				if !slices.Contains(cols, wantCol) {
					t.Fatalf("users 缺 compat 列 %s，实际列: %v", wantCol, cols)
				}
			}

			// mysql：索引仍恰有一条（吞错分支执行过且未重复建）
			if tc.name == "mysql" {
				var n int
				if err := s.bun.DB.QueryRow(
					`SELECT COUNT(1) FROM information_schema.statistics
					 WHERE table_schema = DATABASE() AND table_name = 'messages'
					 AND index_name = 'idx_messages_channel'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 2 { // (channel_id, id) 两列各一行
					t.Fatalf("idx_messages_channel 应保持原有定义，实际索引行数 %d", n)
				}
			}

			// 数据无损
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

			// 升级后正常读写（自增序列/计数器不与存量数据冲突）
			if _, err := s.CreateUser(ctx, "bob", "h"); err != nil {
				t.Fatalf("升级后写入失败: %v", err)
			}
		})
	}
}
