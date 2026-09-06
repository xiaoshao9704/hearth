package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hearth/server/internal/store"

	"golang.org/x/crypto/bcrypt"
)

// mustHash 造密码哈希（与注册/登录同一套 bcrypt）。
func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

// accountFixture 造一个用户并用带 UA 的登录请求换两条会话（模拟两台设备）。
func accountFixture(t *testing.T) (*API, *store.User, string, string) {
	t.Helper()
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", mustHash(t, "old-password"))
	if err != nil {
		t.Fatal(err)
	}
	return a, u, loginAs(t, a, "alice", "old-password", "device-one/1.0"),
		loginAs(t, a, "alice", "old-password", "device-two/1.0")
}

func loginAs(t *testing.T, a *API, username, password, ua string) string {
	t.Helper()
	rec := doReqUA(t, a, http.MethodPost, "/api/login", "", ua,
		map[string]any{"username": username, "password": password})
	if rec.Code != http.StatusOK {
		t.Fatalf("登录状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Token
}

// doReqUA 同 doReq，另带 User-Agent（会话列表要展示它）。
func doReqUA(t *testing.T, a *API, method, path, token, ua string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := a.Router()
	rec := httptest.NewRecorder()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", ua)
	r.ServeHTTP(rec, req)
	return rec
}

func decodeSessions(t *testing.T, body []byte) []store.Session {
	t.Helper()
	var resp struct {
		Sessions []store.Session `json:"sessions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析会话列表失败: %v (%s)", err, body)
	}
	return resp.Sessions
}

func TestListSessionsMarksCurrentAndUA(t *testing.T) {
	a, _, tokenA, tokenB := accountFixture(t)
	r := a.Router()

	rec := doReq(t, r, http.MethodGet, "/api/account/sessions", tokenA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("会话列表状态码=%d: %s", rec.Code, rec.Body.String())
	}
	sessions := decodeSessions(t, rec.Body.Bytes())
	if len(sessions) != 2 {
		t.Fatalf("应有两条会话，实际 %d: %+v", len(sessions), sessions)
	}
	var current, other store.Session
	for _, s := range sessions {
		if s.Current {
			current = s
		} else {
			other = s
		}
	}
	if current.ID != store.SessionID(tokenA) {
		t.Fatalf("当前会话应是发请求的这条: %+v", sessions)
	}
	if other.ID != store.SessionID(tokenB) {
		t.Fatalf("另一条会话指纹不符: %+v", sessions)
	}
	if current.UserAgent != "device-one/1.0" || other.UserAgent != "device-two/1.0" {
		t.Fatalf("UA 未按登录记录: %+v", sessions)
	}
	// token 本身绝不能出现在响应里
	if body := rec.Body.String(); strings.Contains(body, tokenA) || strings.Contains(body, tokenB) {
		t.Fatal("会话列表泄漏了 token")
	}
}

func TestDeleteSessionKicksThatDeviceOnly(t *testing.T) {
	a, _, tokenA, tokenB := accountFixture(t)
	r := a.Router()

	id := store.SessionID(tokenB)
	if rec := doReq(t, r, http.MethodDelete, "/api/account/sessions/"+id, tokenA, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("下线状态码=%d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodGet, "/api/me", tokenB, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("被下线的会话应 401，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, http.MethodGet, "/api/me", tokenA, nil); rec.Code != http.StatusOK {
		t.Fatalf("当前会话应仍有效，实际 %d", rec.Code)
	}
	// 重复下线同一条：已不存在
	if rec := doReq(t, r, http.MethodDelete, "/api/account/sessions/"+id, tokenA, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("重复下线应 404，实际 %d", rec.Code)
	}
}

func TestDeleteSessionCannotTouchOthersSessions(t *testing.T) {
	a, _, tokenA, _ := accountFixture(t)
	ctx := context.Background()
	bob, err := a.st.CreateUser(ctx, "bob", mustHash(t, "bob-password"))
	if err != nil {
		t.Fatal(err)
	}
	bobToken, err := a.st.CreateSessionWithUA(ctx, bob.ID, "bob-device/1.0")
	if err != nil {
		t.Fatal(err)
	}
	r := a.Router()
	rec := doReq(t, r, http.MethodDelete, "/api/account/sessions/"+store.SessionID(bobToken), tokenA, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("下线别人的会话应 404，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, http.MethodGet, "/api/me", bobToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("别人的会话不该被动到，实际 %d", rec.Code)
	}
}

func TestPasswordChangeInvalidatesOtherSessions(t *testing.T) {
	a, _, tokenA, tokenB := accountFixture(t)
	r := a.Router()
	path := "/api/account/password"

	// 旧密码不对：400（不是 401，否则前端会当成会话失效全局登出）
	rec := doReq(t, r, http.MethodPost, path, tokenA, map[string]any{"current": "wrong", "new": "new-password"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("旧密码错状态码=%d, want 400: %s", rec.Code, rec.Body.String())
	}
	// 新密码太短
	rec = doReq(t, r, http.MethodPost, path, tokenA, map[string]any{"current": "old-password", "new": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("新密码过短状态码=%d, want 400", rec.Code)
	}
	rec = doReq(t, r, http.MethodPost, path, tokenA, map[string]any{"current": "old-password", "new": "new-password"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("改密状态码=%d, want 204: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodGet, "/api/me", tokenB, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("改密后其它会话应 401，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, http.MethodGet, "/api/me", tokenA, nil); rec.Code != http.StatusOK {
		t.Fatalf("改密后当前会话应保留，实际 %d", rec.Code)
	}
	// 新密码可登录、旧密码不可
	if rec := doReqUA(t, a, http.MethodPost, "/api/login", "", "device-three/1.0",
		map[string]any{"username": "alice", "password": "old-password"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应登录失败，实际 %d", rec.Code)
	}
	loginAs(t, a, "alice", "new-password", "device-three/1.0")
}

func TestTouchSessionUpdatesLastSeen(t *testing.T) {
	a, u, tokenA, _ := accountFixture(t)
	// 造一条 last_seen 为空的老会话（本版本之前登录的），一次请求后应被刷上时间
	ctx := context.Background()
	legacy, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := a.Router()
	if rec := doReq(t, r, http.MethodGet, "/api/me", legacy, nil); rec.Code != http.StatusOK {
		t.Fatalf("老会话请求状态码=%d", rec.Code)
	}
	sessions := decodeSessions(t, doReq(t, r, http.MethodGet, "/api/account/sessions", tokenA, nil).Body.Bytes())
	for _, s := range sessions {
		if s.ID != store.SessionID(legacy) {
			continue
		}
		if s.LastSeen == nil || s.LastSeen.IsZero() {
			t.Fatalf("请求后应刷上最近活跃时间: %+v", s)
		}
		return
	}
	t.Fatalf("老会话未出现在列表里: %+v", sessions)
}
