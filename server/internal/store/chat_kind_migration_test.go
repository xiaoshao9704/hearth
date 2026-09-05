// 迁移 00004 验收：存量库里没有 kind/meta 两列的旧消息，升级后一律是 text，
// 内容与时间不变；新写入的文件卡片能把 meta 的 JSON 还原成 File。
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestChatKindMigrationDefaultsLegacyRowsToText(t *testing.T) {
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
	for _, stmt := range []string{
		`INSERT INTO users (username, password_hash) VALUES ('alice', 'x')`,
		`INSERT INTO channels (name, created_by) VALUES ('general', 1)`,
		`INSERT INTO messages (channel_id, user_id, content) VALUES (1, 1, 'hello')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("造旧数据失败: %v\nSQL: %s", err, stmt)
		}
	}
	raw.Close()

	s, err := Open("sqlite:" + path)
	if err != nil {
		t.Fatalf("存量库升级失败: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	msgs, err := s.RecentMessages(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hello" || msgs[0].CreatedAt.IsZero() {
		t.Fatalf("旧消息数据不符: %+v", msgs)
	}
	if msgs[0].Kind != KindText || msgs[0].File != nil {
		t.Fatalf("旧消息迁移后应为 text 且无文件元数据: %+v", msgs[0])
	}

	file := &MessageFile{Name: "shot.png", Mime: "image/png", Size: 12345}
	m, err := s.AddMessage(ctx, 1, 1, KindFile, "", file)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindFile || m.File == nil || *m.File != *file {
		t.Fatalf("文件卡片回读不符: %+v", m)
	}
}

// MessagesAfter：after=0 取最近 limit 条，after>0 只回更新的（前端重连补齐用）。
func TestMessagesAfterCursor(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		u, err := s.CreateUser(ctx, "alice", "x")
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.CreateChannel(ctx, "general", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, text := range []string{"一", "二", "三"} {
			m, err := s.AddMessage(ctx, c.ID, u.ID, KindText, text, nil)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, m.ID)
		}

		all, err := s.MessagesAfter(ctx, c.ID, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 3 || all[0].Content != "一" || all[2].Content != "三" {
			t.Fatalf("after=0 应按时间正序回全部: %+v", all)
		}
		rest, err := s.MessagesAfter(ctx, c.ID, ids[0], 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(rest) != 2 || rest[0].ID != ids[1] || rest[1].ID != ids[2] {
			t.Fatalf("after=首条 只应回后两条: %+v", rest)
		}
		if none, err := s.MessagesAfter(ctx, c.ID, ids[2], 50); err != nil || len(none) != 0 {
			t.Fatalf("after=末条 应为空: %+v err=%v", none, err)
		}
	})
}
