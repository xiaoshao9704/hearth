package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hearth/server/internal/store"
)

// 分发：未知 alias 404；livekit /rtc 反代到实例 api_url；ember /voice 无 token → 401；
// WHIP POST 按路径 alias 裁决（bellows-remote 实例：假 key 404、真 key 带 grant 头反代）
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
	// ember WS 信令：无 token 时 voiceWS 在校验会话处返回精确 401（未走到 WS 升级）
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/ember/voice", nil))
	if rec.Code != 401 {
		t.Fatalf("ember /voice 无 token 应 401，实际 %d", rec.Code)
	}
}

// WHIP 按 alias：bellows-remote 实例的 POST 走 definitive 判定 + 签 grant 头
func TestWhipPerAlias(t *testing.T) {
	a := testAPI(t)
	var gotGrant string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGrant = r.Header.Get("X-Bellows-Grant")
		w.WriteHeader(201)
	}))
	defer upstream.Close()
	ctx := context.Background()
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "r1", Type: TypeBellowsRemote, Params: map[string]string{
		"bellows_remote_url": upstream.URL, "bellows_shared_secret": "sec"}})
	// 造一个用户+频道+ingress 记录（provider 存 alias "r1"）
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	c, err := a.st.CreateChannel(ctx, "chan1", u.ID)
	if err != nil {
		t.Fatalf("造频道失败: %v", err)
	}
	rec0, err := a.st.CreateIngress(ctx, u.ID, c.ID, "ing1", "goodkey", "r1")
	if err != nil {
		t.Fatalf("造 ingress 失败: %v", err)
	}
	key := rec0.StreamKey
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	// 假 key → 404（不到达上游）
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r1/w/badkey", strings.NewReader("sdp")))
	if rec.Code != 404 {
		t.Fatalf("假 key 应 404，实际 %d", rec.Code)
	}
	// 真 key → 反代且带 grant
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r1/w/"+key, strings.NewReader("sdp")))
	if rec.Code != 201 || gotGrant == "" {
		t.Fatalf("应 201 且带 grant: %d grant=%q", rec.Code, gotGrant)
	}

	// 归属校验：r1 的 key 经其他实例路径 → 404（密钥是全局命名空间，跨实例串流必须拒）
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "r2", Type: TypeBellowsRemote, Params: map[string]string{
		"bellows_remote_url": upstream.URL, "bellows_shared_secret": "sec2"}})
	a.reloadProviders(ctx)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r2/w/"+key, strings.NewReader("sdp")))
	if rec.Code != 404 {
		t.Fatalf("key 归属与路径实例不符应 404（definitive），实际 %d", rec.Code)
	}
	// fail-open 路径（内建 bellows，无上游）同样校验归属
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/bellows/w/"+key, strings.NewReader("sdp")))
	if rec.Code != 404 {
		t.Fatalf("key 归属与路径实例不符应 404（fail-open），实际 %d", rec.Code)
	}
}
