package api

import (
	"context"
	"net/http"
	"testing"

	"hearth/server/internal/store"
)

func TestChannelModerationRespectsSystemRolesAndProtectsOwners(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	super, err := a.st.CreateUser(ctx, "super", "x")
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := a.st.CreateUser(ctx, "moderator", "x")
	if err != nil {
		t.Fatal(err)
	}
	member, err := a.st.CreateUser(ctx, "member", "x")
	if err != nil {
		t.Fatal(err)
	}
	secondModerator, err := a.st.CreateUser(ctx, "moderator-two", "x")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := a.st.CreateUser(ctx, "disabled", "x")
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.st.CreateChannel(ctx, "general", super.ID)
	if err != nil {
		t.Fatal(err)
	}
	for uid, role := range map[int64]store.ChannelRole{
		attacker.ID:        store.ChannelRoleModerator,
		secondModerator.ID: store.ChannelRoleModerator,
		member.ID:          store.ChannelRoleMember,
	} {
		if err := a.st.SetChannelRole(ctx, c.ID, uid, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.st.SetUserDisabled(ctx, disabled.ID, true); err != nil {
		t.Fatal(err)
	}
	attackerToken, _ := a.st.CreateSession(ctx, attacker.ID)
	superToken, _ := a.st.CreateSession(ctx, super.ID)
	r := a.Router()

	for _, tc := range []struct {
		name, path string
	}{
		{"ban", "/api/channels/general/ban"},
		{"mute", "/api/channels/general/mute"},
		{"kick", "/api/channels/general/kick"},
	} {
		t.Run(tc.name+" super", func(t *testing.T) {
			rec := doReq(t, r, http.MethodPost, tc.path, attackerToken, map[string]any{"user_id": super.ID})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("低档频道管理员操作 super 应 403，实际 %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if rec := doReq(t, r, http.MethodPost, "/api/channels/general/ban", attackerToken,
		map[string]any{"user_id": member.ID}); rec.Code != http.StatusForbidden {
		t.Fatalf("同档系统角色之间不得封禁，实际 %d: %s", rec.Code, rec.Body.String())
	}

	for _, uid := range []int64{super.ID, secondModerator.ID, member.ID} {
		rec := doReq(t, r, http.MethodDelete, "/api/channels/general/members", attackerToken,
			map[string]any{"user_id": uid})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("移出成员 uid=%d 应 204，实际 %d: %s", uid, rec.Code, rec.Body.String())
		}
	}
	for uid, want := range map[int64]store.ChannelRole{
		super.ID:           store.ChannelRoleOwner,
		secondModerator.ID: store.ChannelRoleModerator,
		member.ID:          store.ChannelRoleNone,
	} {
		if got, err := a.st.ChannelRoleOf(ctx, c.ID, uid); err != nil || got != want {
			t.Fatalf("移出后 uid=%d 角色异常: got=%q want=%q err=%v", uid, got, want, err)
		}
	}

	if err := a.st.Ban(ctx, c.ID, attacker.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.st.Gag(ctx, c.ID, attacker.ID); err != nil {
		t.Fatal(err)
	}
	if rec := doReq(t, r, http.MethodPost, "/api/channels/general/unban", attackerToken,
		map[string]any{"user_id": attacker.ID}); rec.Code != http.StatusNoContent {
		t.Fatalf("频道管理员应能解除自身封禁，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, "/api/channels/general/unmute", attackerToken,
		map[string]any{"user_id": attacker.ID}); rec.Code != http.StatusOK {
		t.Fatalf("频道管理员应能解除自身禁言，实际 %d: %s", rec.Code, rec.Body.String())
	}

	for _, path := range []string{"/api/channels/general/transfer", "/api/channels/general/moderators"} {
		rec := doReq(t, r, http.MethodPost, path, superToken, map[string]any{"user_id": disabled.ID})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("停用账号不得接管频道角色，%s 实际 %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}
