package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

// doReqDev 与 doReq 同形，另外带上设备头（访客会话的绑定校验用）。
func doReqDev(t *testing.T, r *chi.Mux, method, path, token, device string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if device != "" {
		req.Header.Set("X-Device-Id", device)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

type guestEntryResp struct {
	Token   string     `json:"token"`
	User    store.User `json:"user"`
	Channel string     `json:"channel"`
}

// guestFixture 造一个频道主（首个账号即 super）与一个频道，返回 API、路由与房主 token。
func guestFixture(t *testing.T) (*API, *chi.Mux, string, *store.Channel, *store.User) {
	t.Helper()
	a := testAPI(t)
	ctx := context.Background()
	owner, err := a.st.CreateUser(ctx, "owner", "x")
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.st.CreateChannel(ctx, "general", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetChannelRole(ctx, c.ID, owner.ID, store.ChannelRoleOwner); err != nil {
		t.Fatal(err)
	}
	tok, err := a.st.CreateSession(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, a.Router(), tok, c, owner
}

// newGuestInvite 经频道管理接口发一条访客邀请，返回邀请码。
func newGuestInvite(t *testing.T, r *chi.Mux, ownerTok, channel string, body any) store.Invite {
	t.Helper()
	rec := doReq(t, r, http.MethodPost, "/api/channels/"+channel+"/invites", ownerTok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("发访客邀请状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Invite store.Invite `json:"invite"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析邀请失败: %v (%s)", err, rec.Body.String())
	}
	return resp.Invite
}

// 三种邀请形态在访客入口上的允许/拒绝：频道访客邀请放行、勾了 allow_guest 的注册邀请放行、
// 普通注册邀请拒绝。
func TestGuestEntryByInviteKind(t *testing.T) {
	a, r, ownerTok, c, owner := guestFixture(t)
	ctx := context.Background()

	guestInv := newGuestInvite(t, r, ownerTok, c.Name, map[string]any{"ttl": "24h", "guest_ttl": "1h", "max_uses": 5})
	plain, err := a.st.CreateInvite(ctx, owner.ID, store.InviteSpec{Kind: "register", MaxUses: 5, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	allowGuest, err := a.st.CreateInvite(ctx, owner.ID,
		store.InviteSpec{Kind: "register", MaxUses: 5, TTL: time.Hour, AllowGuest: true})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, code, username string
		want                 int
	}{
		{"频道访客邀请", guestInv.Code, "visitor", http.StatusOK},
		{"允许访客的注册邀请", allowGuest.Code, "tryout", http.StatusOK},
		{"普通注册邀请", plain.Code, "nope", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doReq(t, r, http.MethodPost, "/api/invites/"+tt.code+"/guest", "",
				map[string]any{"username": tt.username, "device_id": "abcd1234"})
			if rec.Code != tt.want {
				t.Fatalf("状态码=%d, 期望 %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want != http.StatusOK {
				return
			}
			var resp guestEntryResp
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			if resp.User.Role != store.RoleGuest {
				t.Fatalf("角色应是 guest，实际 %q", resp.User.Role)
			}
			if resp.User.ExpiresAt == nil {
				t.Fatal("访客应带 expires_at")
			}
		})
	}

	// 频道访客邀请的访客进了该频道的白名单；注册邀请的访客不绑频道
	guests, err := a.st.ListMembers(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range guests {
		names = append(names, m.Username)
	}
	if !slices.Contains(names, "visitor") {
		t.Fatalf("频道访客应在白名单里，实际 %v", names)
	}
	if slices.Contains(names, "tryout") {
		t.Fatalf("注册邀请的访客不该被写进频道白名单，实际 %v", names)
	}
}

// 会话绑定设备：带对的 X-Device-Id 通过，换一个设备（或不带）即 401。
func TestGuestSessionBoundToDevice(t *testing.T) {
	_, r, ownerTok, c, _ := guestFixture(t)
	inv := newGuestInvite(t, r, ownerTok, c.Name, map[string]any{"ttl": "24h", "guest_ttl": "1h"})
	rec := doReq(t, r, http.MethodPost, "/api/invites/"+inv.Code+"/guest", "",
		map[string]any{"username": "visitor", "device_id": "dev0aaaa"})
	if rec.Code != http.StatusOK {
		t.Fatalf("访客入场状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var resp guestEntryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := doReqDev(t, r, http.MethodGet, "/api/me", resp.Token, "dev0aaaa", nil); got.Code != http.StatusOK {
		t.Fatalf("原设备应通过，状态码=%d: %s", got.Code, got.Body.String())
	}
	if got := doReqDev(t, r, http.MethodGet, "/api/me", resp.Token, "dev0bbbb", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("换设备应 401，状态码=%d", got.Code)
	}
	if got := doReq(t, r, http.MethodGet, "/api/me", resp.Token, nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("不带设备头应 401，状态码=%d", got.Code)
	}
}

// 访客的能力裁剪与频道范围：拿不到推流令牌、建不了频道，也进不了没被授予的频道。
func TestGuestScopeAndCapabilities(t *testing.T) {
	a, r, ownerTok, c, owner := guestFixture(t)
	ctx := context.Background()
	other, err := a.st.CreateChannel(ctx, "other", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv := newGuestInvite(t, r, ownerTok, c.Name, map[string]any{"ttl": "24h", "guest_ttl": "1h"})
	rec := doReq(t, r, http.MethodPost, "/api/invites/"+inv.Code+"/guest", "",
		map[string]any{"username": "visitor", "device_id": "dev0aaaa"})
	var resp guestEntryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	guest, err := a.st.UserByID(ctx, resp.User.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, _, err := a.admitUser(ctx, c, guest); err != nil || !ok {
		t.Fatalf("被授予的频道应放行: ok=%v err=%v", ok, err)
	}
	if _, ok, reason, err := a.admitUser(ctx, other, guest); err != nil || ok {
		t.Fatalf("未授予的频道应拒绝: ok=%v reason=%q err=%v", ok, reason, err)
	}

	if got := doReqDev(t, r, http.MethodGet, "/api/ingest/token", resp.Token, "dev0aaaa", nil); got.Code != http.StatusForbidden {
		t.Fatalf("访客取推流令牌应 403，状态码=%d: %s", got.Code, got.Body.String())
	}
	if got := doReqDev(t, r, http.MethodPost, "/api/channels", resp.Token, "dev0aaaa",
		map[string]any{"name": "mine"}); got.Code != http.StatusForbidden {
		t.Fatalf("访客建频道应 403，状态码=%d", got.Code)
	}
}

// 转正：user_id 不变、角色变、过期时间清空、当前会话仍有效（设备绑定解除），邀请不二次消耗。
func TestGuestClaimKeepsIdentity(t *testing.T) {
	a, r, ownerTok, c, _ := guestFixture(t)
	ctx := context.Background()
	// 转正默认关闭，本用例测的是开着时的行为
	if err := a.st.SetSetting(ctx, "cfg_guest_claim", "on"); err != nil {
		t.Fatal(err)
	}
	inv := newGuestInvite(t, r, ownerTok, c.Name, map[string]any{"ttl": "24h", "guest_ttl": "1h", "max_uses": 5})
	rec := doReq(t, r, http.MethodPost, "/api/invites/"+inv.Code+"/guest", "",
		map[string]any{"username": "visitor", "device_id": "dev0aaaa"})
	var entered guestEntryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &entered); err != nil {
		t.Fatal(err)
	}
	usedBefore, err := a.st.InviteByID(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}

	got := doReqDev(t, r, http.MethodPost, "/api/account/claim", entered.Token, "dev0aaaa",
		map[string]any{"username": "visitor", "password": "secret123"})
	if got.Code != http.StatusOK {
		t.Fatalf("转正状态码=%d: %s", got.Code, got.Body.String())
	}
	var claimed store.User
	if err := json.Unmarshal(got.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.ID != entered.User.ID {
		t.Fatalf("user_id 不该变：%d → %d", entered.User.ID, claimed.ID)
	}
	if claimed.Role != store.RoleUser {
		t.Fatalf("转正后角色应是 user，实际 %q", claimed.Role)
	}
	if claimed.ExpiresAt != nil {
		t.Fatalf("转正后不该还有过期时间：%v", claimed.ExpiresAt)
	}

	// 会话仍有效，且已经不再要求设备头
	if r2 := doReq(t, r, http.MethodGet, "/api/me", entered.Token, nil); r2.Code != http.StatusOK {
		t.Fatalf("转正后原会话应仍有效（且不再绑设备），状态码=%d: %s", r2.Code, r2.Body.String())
	}
	// 频道成员关系保留
	if member, err := a.st.IsMember(ctx, c.ID, claimed.ID); err != nil || !member {
		t.Fatalf("转正后仍应在白名单里: member=%v err=%v", member, err)
	}
	// 能用新密码登录
	if r3 := doReq(t, r, http.MethodPost, "/api/login", "",
		map[string]any{"username": "visitor", "password": "secret123"}); r3.Code != http.StatusOK {
		t.Fatalf("转正后应能用密码登录，状态码=%d: %s", r3.Code, r3.Body.String())
	}
	// 邀请名额不二次消耗
	usedAfter, err := a.st.InviteByID(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usedAfter.Used != usedBefore.Used {
		t.Fatalf("转正不该再消耗邀请名额：%d → %d", usedBefore.Used, usedAfter.Used)
	}
	// 已是注册账号，再调一次转正应被拒
	if r4 := doReq(t, r, http.MethodPost, "/api/account/claim", entered.Token,
		map[string]any{"username": "visitor2", "password": "secret123"}); r4.Code != http.StatusBadRequest {
		t.Fatalf("重复转正应 400，状态码=%d", r4.Code)
	}
}

// 过期清理：过期的访客连账号带会话一并消失，未过期的不动。
func TestPurgeExpiredGuests(t *testing.T) {
	a, r, ownerTok, c, _ := guestFixture(t)
	ctx := context.Background()
	// 过期访客直接按已过去的到期时间造（免得等一小时或改库）
	gone, err := a.st.CreateGuest(ctx, "gone", time.Now().Add(-time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	goneTok, err := a.st.CreateSessionWithDevice(ctx, gone.ID, "test/1.0", "dev0000a")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetChannelRole(ctx, c.ID, gone.ID, store.ChannelRoleMember); err != nil {
		t.Fatal(err)
	}
	inv := newGuestInvite(t, r, ownerTok, c.Name, map[string]any{"ttl": "24h", "guest_ttl": "1h", "max_uses": 5})
	rec := doReq(t, r, http.MethodPost, "/api/invites/"+inv.Code+"/guest", "",
		map[string]any{"username": "stay", "device_id": "dev0000b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("访客入场状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var stay guestEntryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &stay); err != nil {
		t.Fatal(err)
	}

	// 过期的访客在被清理前就已经进不来了（auth 直接 401）
	if got := doReqDev(t, r, http.MethodGet, "/api/me", goneTok, "dev0000a", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("过期访客应 401，状态码=%d", got.Code)
	}
	n, err := a.st.PurgeExpiredGuests(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 个过期访客，实际 %d", n)
	}
	if _, err := a.st.UserByID(ctx, gone.ID); err == nil {
		t.Fatal("过期访客账号应已删除")
	}
	if member, err := a.st.IsMember(ctx, c.ID, gone.ID); err != nil || member {
		t.Fatalf("过期访客的成员行应一并删除: member=%v err=%v", member, err)
	}
	if got := doReqDev(t, r, http.MethodGet, "/api/me", stay.Token, "dev0000b", nil); got.Code != http.StatusOK {
		t.Fatalf("未过期的访客不该被清理，状态码=%d", got.Code)
	}
}

// 频道访客邀请的边界：只有 moderator+ 能发，且撤销只能动本频道的邀请。
func TestGuestInvitePermissions(t *testing.T) {
	a, r, ownerTok, c, _ := guestFixture(t)
	ctx := context.Background()
	stranger, err := a.st.CreateUser(ctx, "stranger", "x")
	if err != nil {
		t.Fatal(err)
	}
	strangerTok, err := a.st.CreateSession(ctx, stranger.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := doReq(t, r, http.MethodPost, "/api/channels/"+c.Name+"/invites", strangerTok,
		map[string]any{"ttl": "24h"}); got.Code != http.StatusForbidden {
		t.Fatalf("非频道管理员发访客邀请应 403，状态码=%d", got.Code)
	}
	inv := newGuestInvite(t, r, ownerTok, c.Name, map[string]any{"ttl": "24h"})
	// guest 类邀请不能拿来注册
	if got := doReq(t, r, http.MethodPost, "/api/register", "",
		map[string]any{"username": "someone", "password": "secret123", "invite": inv.Code}); got.Code == http.StatusOK {
		t.Fatalf("访客邀请不该能注册账号：%s", got.Body.String())
	}
	if got := doReq(t, r, http.MethodDelete, "/api/channels/"+c.Name+"/invites/"+strconv.FormatInt(inv.ID, 10), ownerTok, nil); got.Code != http.StatusNoContent {
		t.Fatalf("撤销访客邀请状态码=%d: %s", got.Code, got.Body.String())
	}
}

// 转正开关：默认 off 时 403，置 on 后放行；/api/me 的 can_claim 与开关同步，非访客恒 false。
func TestGuestClaimGate(t *testing.T) {
	a, r, ownerTok, c, _ := guestFixture(t)
	ctx := context.Background()
	inv := newGuestInvite(t, r, ownerTok, c.Name, map[string]any{"ttl": "24h", "guest_ttl": "1h", "max_uses": 5})
	rec := doReq(t, r, http.MethodPost, "/api/invites/"+inv.Code+"/guest", "",
		map[string]any{"username": "visitor", "device_id": "dev0aaaa"})
	var entered struct {
		Token string `json:"token"`
		User  struct {
			CanClaim bool `json:"can_claim"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entered); err != nil {
		t.Fatal(err)
	}
	// 入场响应与 /api/me 同源：默认关闭时进大厅那一刻就该知道没有转正入口
	if entered.User.CanClaim {
		t.Fatal("默认关闭时入场响应的 can_claim 应为 false")
	}

	canClaim := func(token, device string) bool {
		t.Helper()
		got := doReqDev(t, r, http.MethodGet, "/api/me", token, device, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("/api/me 状态码=%d: %s", got.Code, got.Body.String())
		}
		var me struct {
			CanClaim bool `json:"can_claim"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		return me.CanClaim
	}

	if canClaim(entered.Token, "dev0aaaa") {
		t.Fatal("默认关闭时 /api/me 的 can_claim 应为 false")
	}
	if got := doReqDev(t, r, http.MethodPost, "/api/account/claim", entered.Token, "dev0aaaa",
		map[string]any{"username": "visitor", "password": "secret123"}); got.Code != http.StatusForbidden {
		t.Fatalf("未开放访客转正时应 403，状态码=%d: %s", got.Code, got.Body.String())
	}
	// 非访客（房主）恒 false，与开关无关
	if canClaim(ownerTok, "") {
		t.Fatal("非访客的 can_claim 应恒为 false")
	}

	if err := a.st.SetSetting(ctx, "cfg_guest_claim", "on"); err != nil {
		t.Fatal(err)
	}
	if !canClaim(entered.Token, "dev0aaaa") {
		t.Fatal("开关置 on 后访客的 can_claim 应为 true")
	}
	if canClaim(ownerTok, "") {
		t.Fatal("开关置 on 也不该让非访客的 can_claim 变 true")
	}
	if got := doReqDev(t, r, http.MethodPost, "/api/account/claim", entered.Token, "dev0aaaa",
		map[string]any{"username": "visitor", "password": "secret123"}); got.Code != http.StatusOK {
		t.Fatalf("开放后转正应成功，状态码=%d: %s", got.Code, got.Body.String())
	}
	// 转正后不再是访客，can_claim 回到 false
	if canClaim(entered.Token, "") {
		t.Fatal("转正后 can_claim 应回到 false")
	}
}
