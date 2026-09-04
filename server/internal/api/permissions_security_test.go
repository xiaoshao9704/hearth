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
	peerOwner, err := a.st.CreateUser(ctx, "peer-owner", "x")
	if err != nil {
		t.Fatal(err)
	}
	peerModerator, err := a.st.CreateUser(ctx, "peer-moderator", "x")
	if err != nil {
		t.Fatal(err)
	}
	peerChannel, err := a.st.CreateChannel(ctx, "peer", peerOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetChannelRole(ctx, peerChannel.ID, peerModerator.ID, store.ChannelRoleModerator); err != nil {
		t.Fatal(err)
	}
	attackerToken, _ := a.st.CreateSession(ctx, attacker.ID)
	superToken, _ := a.st.CreateSession(ctx, super.ID)
	peerOwnerToken, _ := a.st.CreateSession(ctx, peerOwner.ID)
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
		map[string]any{"user_id": secondModerator.ID}); rec.Code != http.StatusForbidden {
		t.Fatalf("moderator 不得封禁同级 moderator，实际 %d: %s", rec.Code, rec.Body.String())
	}

	if rec := doReq(t, r, http.MethodPost, "/api/channels/general/ban", attackerToken,
		map[string]any{"user_id": member.ID}); rec.Code != http.StatusOK {
		t.Fatalf("user 档 moderator 应能封禁 user 档 member，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if banned, err := a.st.IsBanned(ctx, c.ID, member.ID); err != nil || !banned {
		t.Fatalf("封禁应落库: banned=%v err=%v", banned, err)
	}
	if rec := doReq(t, r, http.MethodPost, "/api/channels/general/unban", attackerToken,
		map[string]any{"user_id": member.ID}); rec.Code != http.StatusNoContent {
		t.Fatalf("moderator 应能解封低频道角色用户，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, "/api/channels/general/mute", attackerToken,
		map[string]any{"user_id": member.ID}); rec.Code != http.StatusOK {
		t.Fatalf("user 档 moderator 应能禁言 user 档 member，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, "/api/channels/general/unmute", attackerToken,
		map[string]any{"user_id": member.ID}); rec.Code != http.StatusOK {
		t.Fatalf("moderator 应能解除低频道角色用户的禁言，实际 %d: %s", rec.Code, rec.Body.String())
	}

	for uid, wantStatus := range map[int64]int{
		super.ID: http.StatusBadRequest, secondModerator.ID: http.StatusBadRequest, member.ID: http.StatusNoContent,
	} {
		rec := doReq(t, r, http.MethodDelete, "/api/channels/general/members", attackerToken,
			map[string]any{"user_id": uid})
		if rec.Code != wantStatus {
			t.Fatalf("移出成员 uid=%d 应 %d，实际 %d: %s", uid, wantStatus, rec.Code, rec.Body.String())
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

	if rec := doReq(t, r, http.MethodPost, "/api/channels/peer/ban", peerOwnerToken,
		map[string]any{"user_id": peerModerator.ID}); rec.Code != http.StatusOK {
		t.Fatalf("user 档 owner 应能封禁 user 档 moderator，实际 %d: %s", rec.Code, rec.Body.String())
	}
}
