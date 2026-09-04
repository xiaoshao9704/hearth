package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hearth/server/internal/store"
)

// 分发：未知 alias 404；livekit /rtc 反代到实例 api_url
func TestProviderDispatch(t *testing.T) {
	a := testAPI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Path", r.URL.Path)
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	ctx := context.Background()
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk1", Type: TypeLivekit, Params: map[string]string{
		"livekit_api_url": upstream.URL, "livekit_api_key": "k", "livekit_api_secret": "s"}})
	a.reloadProviders(ctx)

	r := a.Router()
	a.RegisterProxies(r)

	// 未知 alias
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/nope/rtc/validate", nil))
	if rec.Code != 404 {
		t.Fatalf("未知 alias 应 404，实际 %d", rec.Code)
	}
	// livekit 信令反代：/providers/lk1/rtc/validate → 上游 /rtc/validate
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/lk1/rtc/validate", nil))
	if rec.Code != 200 || rec.Header().Get("X-Upstream-Path") != "/rtc/validate" {
		t.Fatalf("livekit 反代路径错误: %d %q", rec.Code, rec.Header().Get("X-Upstream-Path"))
	}
}

// WHIP 按 alias：推流只进当前舞台实例，注册表里不是当前舞台的实例即使存在，
// POST 也被 admitIngest 的门禁 definitive 404，请求不到达其上游。
func TestWhipPerAlias(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if _, err := a.st.CreateChannel(ctx, "chan1", u.ID); err != nil {
		t.Fatalf("建频道失败: %v", err)
	}
	it, err := a.st.CreateIngestToken(ctx, u.ID, "obs")
	if err != nil {
		t.Fatalf("建令牌失败: %v", err)
	}

	reached := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(201)
	}))
	defer upstream.Close()
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "r1", Type: TypeLivekit, Params: map[string]string{
		"livekit_api_url": upstream.URL, "livekit_api_key": "k", "livekit_api_secret": "s"}})
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	// 真令牌也 404（r1 不是当前舞台实例），且不到达上游
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r1/w/chan1/"+it.Token, strings.NewReader("sdp")))
	if rec.Code != 404 || reached != 0 {
		t.Fatalf("非当前舞台实例应 404 且不到达上游: %d reached=%d", rec.Code, reached)
	}
}

// sessions/revoke 是 WHIP 路径保留字（/w/sessions/{rid} 会话收尾、/w/revoke/{token} 远端撤销），
// 创建频道须在 channelNameRe 之后拒绝这两个字面值。
func TestReservedChannelNames(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	tok, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	r := a.Router()
	for _, c := range []struct {
		name string
		want int
	}{
		{"sessions", 400},
		{"revoke", 400},
		{"normal-chan", 201},
	} {
		req := httptest.NewRequest("POST", "/api/channels", strings.NewReader(`{"name":"`+c.name+`"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("频道 %q 期望 %d，实际 %d: %s", c.name, c.want, rec.Code, rec.Body.String())
		}
	}
}

// Location 改写要认三种形态：上游返什么由它自己决定（外部 LiveKit 不受我们控制），
// 只有纯相对形式本就落在代理路径下、不该动。
func TestRewriteWHIPLocation(t *testing.T) {
	const prefix = "/providers/ing1"
	cases := []struct {
		name, in, want string
	}{
		{"根相对", "/w/sessions/rid9", "/providers/ing1/w/sessions/rid9"},
		{"绝对 URL 只取路径", "http://upstream:8080/w/abc", "/providers/ing1/w/abc"},
		{"绝对 URL 带 query", "https://up.example.com/w/abc?k=1", "/providers/ing1/w/abc?k=1"},
		{"纯相对不动", "sessions/rid9", "sessions/rid9"},
		{"非 /w/ 路径不动", "/other/abc", "/other/abc"},
		{"绝对 URL 非 /w/ 不动", "http://upstream:8080/other/abc", "http://upstream:8080/other/abc"},
		{"空值不动", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteWHIPLocation(c.in, prefix); got != c.want {
				t.Fatalf("rewriteWHIPLocation(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}
