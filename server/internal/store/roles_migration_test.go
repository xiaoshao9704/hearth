package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateRoleDataIsDeterministicAndIdempotent(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		for _, stmt := range []string{
			`INSERT INTO users (id, username, password_hash, is_admin, disabled, role) VALUES (1, 'disabled-admin', 'x', 1, 1, 'user')`,
			`INSERT INTO users (id, username, password_hash, is_admin, disabled, role) VALUES (2, 'active-admin', 'x', 1, 0, 'user')`,
			`INSERT INTO users (id, username, password_hash, is_admin, disabled, role) VALUES (3, 'former-admin', 'x', 1, 0, 'power')`,
			`INSERT INTO users (id, username, password_hash, is_admin, disabled, role) VALUES (4, 'creator', 'x', 0, 0, 'user')`,
			`INSERT INTO channels (id, name, created_by) VALUES (10, 'valid', 4)`,
			`INSERT INTO channels (id, name, created_by) VALUES (11, 'dangling', 999)`,
			`INSERT INTO channel_members (channel_id, user_id, role) VALUES (10, 3, 'owner')`,
			`INSERT INTO channel_members (channel_id, user_id, role) VALUES (10, 4, 'member')`,
		} {
			if _, err := s.bun.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("造旧数据失败: %v\nSQL: %s", err, stmt)
			}
		}

		for run := 1; run <= 2; run++ {
			result, err := s.MigrateRoleData(ctx)
			if err != nil {
				t.Fatalf("第 %d 次迁移失败: %v", run, err)
			}
			if result.SuperID != 2 || result.SkippedChannel != 1 {
				t.Fatalf("第 %d 次迁移结果错误: %+v", run, result)
			}
		}

		for id, want := range map[int64]struct {
			role    string
			isAdmin int
		}{
			1: {"admin", 1},
			2: {"super", 1},
			3: {"power", 0},
			4: {"power", 0},
		} {
			var role string
			var isAdmin int
			if err := s.bun.NewRaw("SELECT role, is_admin FROM users WHERE id = ?", id).Scan(ctx, &role, &isAdmin); err != nil {
				t.Fatal(err)
			}
			if role != want.role || isAdmin != want.isAdmin {
				t.Fatalf("uid=%d 迁移后不符: role=%s is_admin=%d", id, role, isAdmin)
			}
		}

		var ownerID int64
		if err := s.bun.NewRaw(
			"SELECT user_id FROM channel_members WHERE channel_id = 10 AND role = 'owner'").Scan(ctx, &ownerID); err != nil {
			t.Fatal(err)
		}
		if ownerID != 4 {
			t.Fatalf("频道应只认 created_by 为 owner，实际 uid=%d", ownerID)
		}
		var danglingOwners int
		if err := s.bun.NewRaw(
			"SELECT COUNT(1) FROM channel_members WHERE channel_id = 11 AND role = 'owner'").Scan(ctx, &danglingOwners); err != nil {
			t.Fatal(err)
		}
		if danglingOwners != 0 {
			t.Fatalf("悬空 created_by 不得产生 owner 行，实际 %d", danglingOwners)
		}

		if s.d.name != "mysql" {
			if err := s.SetChannelRole(ctx, 10, 3, ChannelRoleOwner); !IsUniqueViolation(err) {
				t.Fatalf("部分唯一索引应拒绝第二行 owner，实际 %v", err)
			}
		}
	})
}

func TestMigrateRoleDataRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	s, err := Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, stmt := range []string{
		`INSERT INTO users (id, username, password_hash, is_admin, disabled, role) VALUES (1, 'admin', 'x', 1, 0, 'user')`,
		`INSERT INTO channels (id, name, created_by) VALUES (1, 'general', 1)`,
		`INSERT INTO channel_members (channel_id, user_id, role) VALUES (1, 1, 'member')`,
		`CREATE TRIGGER fail_owner BEFORE UPDATE OF role ON channel_members
WHEN NEW.role = 'owner' BEGIN SELECT RAISE(FAIL, 'forced owner failure'); END`,
	} {
		if _, err := s.bun.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.MigrateRoleData(ctx); err == nil {
		t.Fatal("触发 owner 写入错误时迁移应失败")
	}
	var role string
	var isAdmin int
	if err := s.bun.NewRaw("SELECT role, is_admin FROM users WHERE id = 1").Scan(ctx, &role, &isAdmin); err != nil {
		t.Fatal(err)
	}
	if role != "user" || isAdmin != 1 {
		t.Fatalf("迁移失败后用户改动应全部回滚: role=%s is_admin=%d", role, isAdmin)
	}
	if err := s.bun.NewRaw(
		"SELECT role FROM channel_members WHERE channel_id = 1 AND user_id = 1").Scan(ctx, &role); err != nil {
		t.Fatal(err)
	}
	if role != "member" {
		t.Fatalf("迁移失败后频道角色应回滚，实际 %s", role)
	}
}
