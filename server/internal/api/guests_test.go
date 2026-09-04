// 频道访客的验收测试：访客邀请 → 入场 → 设备绑定 → 频道范围 → 过期。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

// 备一个频道 + owner 会话 + 一条访客邀请，返回 (ownerToken, channelName, inviteCode)。
func setupGuestInvite(t *testing.T, a *API) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	tok := adminToken(t, a)
	u, _, _ := a.st.UserByName(ctx, "admin")
	if _, err := a.st.CreateChannel(ctx, "hall", u.ID); err != nil {
		t.Fatalf("建频道失败: %v", err)
	}
	r := a.Router()
	rec := doReq(t, r, http.MethodPost, "/api/channels/hall/invites", tok,
		map[string]any{"ttl": "24h", "guest_ttl": "1h", "max_uses": 0})
	if rec.Code != http.StatusCreated {
		t.Fatalf("发访客邀请失败: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Invite store.Invite `json:"invite"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return tok, "hall", resp.Invite.Code
}

// doReqDev 同 doReq，附带设备头（访客会话的每个请求都要带）。
func doReqDev(t *testing.T, r *chi.Mux, method, path, token, deviceID string, body any) *httptest.ResponseRecorder {
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Device-Id", deviceID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// guestEnter 走公开入场接口，返回响应记录器。
func guestEnter(t *testing.T, a *API, code, username, deviceID string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, a.Router(), http.MethodPost, "/api/invites/"+code+"/guest", "",
		map[string]any{"username": username, "device_id": deviceID})
}

func TestGuestInviteJoinAndScope(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	tok, channel, code := setupGuestInvite(t, a)
	r := a.Router()
	ctx := context.Background()

	// 加入页信息：kind=guest + 频道名
	rec := doReq(t, r, http.MethodGet, "/api/invites/"+code, "", nil)
	var info struct {
		Kind        string `json:"kind"`
		ChannelName string `json:"channel_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info.Kind != "guest" || info.ChannelName != "hall" {
		t.Fatalf("inviteInfo 不符: %s", rec.Body.String())
	}

	// 访客入场
	rec = guestEnter(t, a, code, "visitor1", "dev0001")
	if rec.Code != http.StatusOK {
		t.Fatalf("访客入场失败: %d %s", rec.Code, rec.Body.String())
	}
	var join struct {
		Token   string     `json:"token"`
		User    store.User `json:"user"`
		Channel string     `json:"channel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &join); err != nil {
		t.Fatal(err)
	}
	if join.Channel != channel || join.User.Role != store.RoleGuest || join.User.ExpiresAt == nil {
		t.Fatalf("入场响应不符: %+v", join)
	}

	// 设备绑定：带对设备头能取 /api/me，不带/带错都 401
	rec = doReq(t, r, http.MethodGet, "/api/me", join.Token, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺设备头应 401，实际 %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+join.Token)
	req.Header.Set("X-Device-Id", "dev0002") // 换设备
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("设备不匹配应 401，实际 %d", rec2.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+join.Token)
	req.Header.Set("X-Device-Id", "dev0001")
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusOK {
		t.Fatalf("设备匹配应通过，实际 %d", rec3.Code)
	}

	// 频道范围：访客列表只见被授予的频道；进别的频道被拒
	rec = doReqDev(t, r, http.MethodGet, "/api/channels", join.Token, "dev0001", nil)
	var chs struct {
		Channels []store.Channel `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chs); err != nil || len(chs.Channels) != 1 || chs.Channels[0].MyRole != "member" {
		t.Fatalf("访客频道列表不符: %s", rec.Body.String())
	}
	owner, _, _ := a.st.UserByName(ctx, "admin")
	if _, err := a.st.CreateChannel(ctx, "other", owner.ID); err != nil {
		t.Fatal(err)
	}
	rec = doReqDev(t, r, http.MethodPost, "/api/token", join.Token, "dev0001", map[string]any{"channel": "other", "device_id": "dev0001"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("访客进未授予频道应 403，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 能力裁剪：建频道 / 发邀请 / 推流令牌 / 改密码 全部 403
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/channels"},
		{http.MethodPost, "/api/invites"},
		{http.MethodGet, "/api/ingest/token"},
		{http.MethodPost, "/api/account/password"},
	} {
		rec := doReqDev(t, r, tc.method, tc.path, join.Token, "dev0001", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 访客应 403，实际 %d", tc.method, tc.path, rec.Code)
		}
	}

	// 同名冲突提示
	if rec := guestEnter(t, a, code, "visitor1", "dev0009"); rec.Code != http.StatusConflict {
		t.Fatalf("重名应 409，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 次数耗尽的链接第二次打开被拒（1 次链接：访客入场后名额即用完）
	rec = doReq(t, r, http.MethodPost, "/api/channels/"+channel+"/invites", tok,
		map[string]any{"ttl": "24h", "guest_ttl": "1h", "max_uses": 1})
	if rec.Code != http.StatusCreated {
		t.Fatalf("发 1 次访客邀请失败: %d %s", rec.Code, rec.Body.String())
	}
	var inv1 struct {
		Invite store.Invite `json:"invite"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inv1); err != nil {
		t.Fatal(err)
	}
	code1 := inv1.Invite.Code
	if rec := guestEnter(t, a, code1, "visitor3", "dev0003"); rec.Code != http.StatusOK {
		t.Fatalf("首次入场应成功: %d %s", rec.Code, rec.Body.String())
	}
	if rec := guestEnter(t, a, code1, "visitor4", "dev0004"); rec.Code != http.StatusForbidden {
		t.Fatalf("名额用尽的链接第二次打开应 403，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 过期访客：auth 直接 401，清理协程删行且消息保留（发送者兜底文案）。
func TestGuestExpiryAndSweep(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	_, _, code := setupGuestInvite(t, a)
	r := a.Router()
	ctx := context.Background()

	// 直接造一个已过期的访客（负寿命）与其绑定设备的会话
	inv, err := a.st.InviteByCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	g, err := a.st.CreateGuest(ctx, "visitor2", inv.ID, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := a.st.ChannelByID(ctx, *inv.ChannelID)
	if err := a.st.SetChannelRole(ctx, c.ID, g.ID, store.ChannelRoleMember); err != nil {
		t.Fatal(err)
	}
	tok, err := a.st.CreateSessionForDevice(ctx, g.ID, "dev0007")
	if err != nil {
		t.Fatal(err)
	}
	// 留一条消息
	if _, err := a.st.AddMessage(ctx, c.ID, g.ID, "我来过"); err != nil {
		t.Fatal(err)
	}

	// auth：过期访客 401
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Device-Id", "dev0007")
	rec4 := httptest.NewRecorder()
	r.ServeHTTP(rec4, req)
	if rec4.Code != http.StatusUnauthorized {
		t.Fatalf("过期访客应 401，实际 %d", rec4.Code)
	}

	// 清理协程删行
	a.sweepGuests(ctx)
	if _, err := a.st.UserByID(ctx, g.ID); err == nil {
		t.Fatal("过期访客应被清理")
	}
	// 消息保留，发送者兜底
	msgs, err := a.st.RecentMessages(ctx, c.ID, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Username != "已离开的访客" {
		t.Fatalf("消息应保留且发送者兜底: %+v %v", msgs, err)
	}
}
