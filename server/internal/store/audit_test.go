package store

import (
	"context"
	"testing"
	"time"
)

// auditSeed 造一个操作者、一个目标与一个频道，返回三者 id。
func auditSeed(t *testing.T, s *Store) (actor, target, channel int64) {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateUser(ctx, "actor", "h")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateUser(ctx, "target", "h")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateChannel(ctx, "general", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID, b.ID, c.ID
}

func TestAuditWriteAndRead(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		actor, target, channel := auditSeed(t, s)
		if err := s.Audit(ctx, AuditRecord{
			ActorUID: actor, Action: AuditMute, TargetUID: target, ChannelID: channel, Detail: "禁言测试",
		}); err != nil {
			t.Fatal(err)
		}
		// 不针对某人、也不属于某频道的动作：两个可空列落 NULL，读回为 0/空
		if err := s.Audit(ctx, AuditRecord{ActorUID: actor, Action: AuditChannelClear}); err != nil {
			t.Fatal(err)
		}
		entries, err := s.ListAudit(ctx, AuditFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("应有两条记录: %+v", entries)
		}
		if entries[0].Action != AuditChannelClear || entries[0].TargetUID != 0 ||
			entries[0].TargetName != "" || entries[0].ChannelID != 0 || entries[0].ChannelName != "" {
			t.Fatalf("无目标/无频道的记录形状不符: %+v", entries[0])
		}
		e := entries[1]
		if e.Action != AuditMute || e.ActorUID != actor || e.ActorName != "actor" ||
			e.TargetUID != target || e.TargetName != "target" ||
			e.ChannelID != channel || e.ChannelName != "general" || e.Detail != "禁言测试" {
			t.Fatalf("记录形状不符: %+v", e)
		}
		if e.At.IsZero() {
			t.Fatalf("时间未落库: %+v", e)
		}
	})
}

func TestAuditPurgeByAge(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		actor, target, channel := auditSeed(t, s)
		for i := 0; i < 2; i++ {
			if err := s.Audit(ctx, AuditRecord{
				ActorUID: actor, Action: AuditBan, TargetUID: target, ChannelID: channel,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// 把其中一条挪到 30 天前，保留 7 天时它该被清掉、另一条留着
		entries, err := s.ListAudit(ctx, AuditFilter{})
		if err != nil {
			t.Fatal(err)
		}
		old := entries[len(entries)-1].ID
		if _, err := s.bun.NewRaw("UPDATE audit_log SET at = ? WHERE id = ?",
			time.Now().AddDate(0, 0, -30), old).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		n, err := s.PurgeAudit(ctx, time.Now().AddDate(0, 0, -7))
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("应只删 1 条超期记录，实际 %d", n)
		}
		entries, err = s.ListAudit(ctx, AuditFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].ID == old {
			t.Fatalf("留下的应是未超期的那条: %+v", entries)
		}
	})
}
