package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"hearth/server/internal/store"
)

// TestLobbyChannelVisibility 覆盖大厅列表的可见性规则：
// 邀请制频道对非成员不可见（唯一例外是 super，可见但标 hidden）；
// 封禁只影响能否进入，不影响是否出现在列表里（banned 标记，前端据此禁用进入）。
func TestLobbyChannelVisibility(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()

	// 建号顺序很重要：首个账号自动成为 super（全站恰好一个的保底）
	super, err := a.st.CreateUser(ctx, "super", "x")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := a.st.CreateUser(ctx, "owner", "x")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := a.st.CreateUserWithRole(ctx, "admin", "x", store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := a.st.CreateUser(ctx, "member", "x")
	if err != nil {
		t.Fatal(err)
	}
	memberBanned, err := a.st.CreateUser(ctx, "member-banned", "x")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := a.st.CreateUser(ctx, "outsider", "x")
	if err != nil {
		t.Fatal(err)
	}
	publicBanned, err := a.st.CreateUser(ctx, "public-banned", "x")
	if err != nil {
		t.Fatal(err)
	}

	pub, err := a.st.CreateChannel(ctx, "public", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := a.st.CreateChannel(ctx, "invited", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetInviteOnly(ctx, inv.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetChannelRole(ctx, inv.ID, member.ID, store.ChannelRoleMember); err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetChannelRole(ctx, inv.ID, memberBanned.ID, store.ChannelRoleMember); err != nil {
		t.Fatal(err)
	}
	if err := a.st.Ban(ctx, inv.ID, memberBanned.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.st.Ban(ctx, pub.ID, publicBanned.ID); err != nil {
		t.Fatal(err)
	}

	sessionOf := func(uid int64) string {
		tok, err := a.st.CreateSession(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	r := a.Router()
	type chResp struct {
		Channels []store.Channel `json:"channels"`
	}
	fetch := func(token string) map[string]store.Channel {
		t.Helper()
		rec := doReq(t, r, http.MethodGet, "/api/channels", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/channels 应 200，实际 %d: %s", rec.Code, rec.Body.String())
		}
		var resp chResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("解析响应失败: %v: %s", err, rec.Body.String())
		}
		out := map[string]store.Channel{}
		for _, c := range resp.Channels {
			out[c.Name] = c
		}
		return out
	}

	// 普通非成员：看不到邀请制频道
	outsiderChs := fetch(sessionOf(outsider.ID))
	if _, ok := outsiderChs["invited"]; ok {
		t.Fatal("普通非成员不应看到邀请制频道")
	}
	if _, ok := outsiderChs["public"]; !ok {
		t.Fatal("普通用户应看到公开频道")
	}

	// 成员：看得到，正常状态，非 hidden
	memberChs := fetch(sessionOf(member.ID))
	invForMember, ok := memberChs["invited"]
	if !ok {
		t.Fatal("成员应看到自己所在的邀请制频道")
	}
	if invForMember.Hidden {
		t.Fatal("成员视角不应标 hidden")
	}
	if invForMember.Banned {
		t.Fatal("未被封禁的成员不应标 banned")
	}
	if invForMember.MyRole != string(store.ChannelRoleMember) {
		t.Fatalf("成员的 my_role 应为 member，实际 %q", invForMember.MyRole)
	}

	// 系统 admin（非 super）非成员：看不到邀请制频道（唯一例外是 super）
	adminChs := fetch(sessionOf(admin.ID))
	if _, ok := adminChs["invited"]; ok {
		t.Fatal("非成员的系统 admin 不应看到邀请制频道，只有 super 例外")
	}

	// super 非成员：看得到，标 hidden
	superChs := fetch(sessionOf(super.ID))
	invForSuper, ok := superChs["invited"]
	if !ok {
		t.Fatal("super 应能看到非成员的邀请制频道")
	}
	if !invForSuper.Hidden {
		t.Fatal("super 看到的非成员邀请制频道应标 hidden")
	}

	// 公开频道被封禁：仍可见，标 banned
	publicBannedChs := fetch(sessionOf(publicBanned.ID))
	pubForBanned, ok := publicBannedChs["public"]
	if !ok {
		t.Fatal("被封禁用户仍应在公开频道列表里看到该频道")
	}
	if !pubForBanned.Banned {
		t.Fatal("被封禁用户看到的公开频道应标 banned")
	}

	// 邀请制成员被封禁：仍可见（封禁不摘白名单），标 banned
	memberBannedChs := fetch(sessionOf(memberBanned.ID))
	invForBannedMember, ok := memberBannedChs["invited"]
	if !ok {
		t.Fatal("被封禁的邀请制成员仍应看到该频道（封禁不摘白名单）")
	}
	if !invForBannedMember.Banned {
		t.Fatal("被封禁的邀请制成员看到的频道应标 banned")
	}
	if invForBannedMember.Hidden {
		t.Fatal("成员视角不应标 hidden，即便被封禁")
	}
}
