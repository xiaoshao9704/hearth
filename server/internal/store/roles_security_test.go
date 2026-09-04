package store

import (
	"context"
	"testing"
)

func TestRemoveMemberCannotRemoveChannelRoles(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		owner, _ := s.CreateUser(ctx, "owner", "x")
		moderator, _ := s.CreateUser(ctx, "moderator", "x")
		member, _ := s.CreateUser(ctx, "member", "x")
		c, _ := s.CreateChannel(ctx, "general", owner.ID)
		if err := s.SetChannelRole(ctx, c.ID, moderator.ID, ChannelRoleModerator); err != nil {
			t.Fatal(err)
		}
		if err := s.SetChannelRole(ctx, c.ID, member.ID, ChannelRoleMember); err != nil {
			t.Fatal(err)
		}

		for uid, wantRemoved := range map[int64]bool{
			owner.ID: false, moderator.ID: false, member.ID: true,
		} {
			removed, err := s.RemoveMember(ctx, c.ID, uid)
			if err != nil {
				t.Fatal(err)
			}
			if removed != wantRemoved {
				t.Fatalf("uid=%d 移出结果异常: got=%v want=%v", uid, removed, wantRemoved)
			}
		}
		for uid, want := range map[int64]ChannelRole{
			owner.ID:     ChannelRoleOwner,
			moderator.ID: ChannelRoleModerator,
			member.ID:    ChannelRoleNone,
		} {
			if got, err := s.ChannelRoleOf(ctx, c.ID, uid); err != nil || got != want {
				t.Fatalf("uid=%d 角色异常: got=%q want=%q err=%v", uid, got, want, err)
			}
		}
	})
}

func TestChannelOwnershipNeverFallsBackToCreatedBy(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		creator, _ := s.CreateUser(ctx, "creator", "x")
		c, _ := s.CreateChannel(ctx, "private", creator.ID)
		if err := s.SetInviteOnly(ctx, c.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := s.bun.NewRaw(
			"DELETE FROM channel_members WHERE channel_id = ? AND role = 'owner'", c.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}

		got, err := s.ChannelByID(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.OwnerID != 0 || got.CreatedBy != "" {
			t.Fatalf("owner 行不存在时不得回落 created_by: %+v", got)
		}
		if ok, _, err := s.CanJoin(ctx, got, creator.ID); err != nil || ok {
			t.Fatalf("原始创建者没有成员行时不得进入邀请制频道: ok=%v err=%v", ok, err)
		}
	})
}

func TestTransferChannelKeepsExactlyOneOwner(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *Store, _ string) {
		ctx := context.Background()
		oldOwner, _ := s.CreateUser(ctx, "old-owner", "x")
		newOwner, _ := s.CreateUser(ctx, "new-owner", "x")
		c, _ := s.CreateChannel(ctx, "general", oldOwner.ID)
		if err := s.SetChannelRole(ctx, c.ID, newOwner.ID, ChannelRoleMember); err != nil {
			t.Fatal(err)
		}
		if err := s.TransferChannel(ctx, c.ID, newOwner.ID); err != nil {
			t.Fatal(err)
		}

		if got, _ := s.ChannelRoleOf(ctx, c.ID, oldOwner.ID); got != ChannelRoleModerator {
			t.Fatalf("旧房主应降为 moderator，实际 %q", got)
		}
		if got, _ := s.ChannelRoleOf(ctx, c.ID, newOwner.ID); got != ChannelRoleOwner {
			t.Fatalf("新房主应为 owner，实际 %q", got)
		}
		var owners int
		if err := s.bun.NewRaw(
			"SELECT COUNT(1) FROM channel_members WHERE channel_id = ? AND role = 'owner'", c.ID).
			Scan(ctx, &owners); err != nil {
			t.Fatal(err)
		}
		if owners != 1 {
			t.Fatalf("转让后应恰有一行 owner，实际 %d", owners)
		}
	})
}
