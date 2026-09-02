package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// 分发：未知 alias 404；livekit /rtc 反代到实例 api_url；ember /voice 无 token → 401
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

// WHIP 按 alias：bellows-remote 实例的 POST 先过 admitIngest（definitive），签 grant
// 随反代带给远端；应答 Location /w/sessions/{rid} 改写成 /providers/{alias}/w/sessions/{rid}；
// 未知令牌 404 且不到达上游；PATCH/DELETE 按 rid 路由反代。
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

	type whipReq struct {
		method, path, grant, auth string
		body                      string
	}
	var got []whipReq
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, whipReq{r.Method, r.URL.Path, r.Header.Get("X-Bellows-Grant"), r.Header.Get("Authorization"), string(b)})
		w.Header().Set("Location", "/w/sessions/rid9")
		w.WriteHeader(201)
	}))
	defer upstream.Close()
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "r1", Type: TypeBellowsRemote, Params: map[string]string{
		"bellows_remote_url": upstream.URL, "bellows_shared_secret": "sec"}})
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	// 未知令牌：404 且不到达上游
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r1/w/chan1/badtoken", strings.NewReader("sdp")))
	if rec.Code != 404 || len(got) != 0 {
		t.Fatalf("未知令牌应 404 且不到达上游: %d reached=%d", rec.Code, len(got))
	}

	// 路径模式：admitIngest 通过 → 带 grant 反代，Location 改写
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r1/w/chan1/"+it.Token, strings.NewReader("sdp")))
	if rec.Code != 201 {
		t.Fatalf("真令牌应反代成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/providers/r1/w/sessions/rid9" {
		t.Fatalf("Location 应改写为 /providers/r1/w/sessions/rid9，实际 %q", loc)
	}
	if len(got) != 1 || got[0].path != "/w/chan1/"+it.Token || got[0].body != "sdp" {
		t.Fatalf("上游收到的请求不符: %+v", got)
	}
	// grant payload：字段齐全，identity 主体是 user_id，展示信息在 meta 里
	var p struct {
		Op, Token, Room, Identity, Offer string
		Meta                             rtc.Meta
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(got[0].grant, ".", 2)[0])
	if err != nil || json.Unmarshal(raw, &p) != nil {
		t.Fatalf("grant 解码失败: %v", err)
	}
	sum := sha256.Sum256([]byte("sdp"))
	if p.Op != "publish" || p.Token != it.Token || p.Room != "chan1" || p.Identity != rtc.Identity(u.ID, "obs") ||
		p.Meta.UID != u.ID || p.Meta.Username != "alice" || p.Meta.Kind != "ingest" || p.Meta.Tag != "obs" ||
		p.Offer != hex.EncodeToString(sum[:]) {
		t.Fatalf("grant payload 不符: %+v", p)
	}

	// bearer 模式：令牌在 Authorization，反代时原样带上（远端验签要比对 grant.token）
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/providers/r1/w/chan1", strings.NewReader("sdp"))
	req.Header.Set("Authorization", "Bearer "+it.Token)
	r.ServeHTTP(rec, req)
	if rec.Code != 201 || len(got) != 2 || got[1].path != "/w/chan1" || got[1].auth != "Bearer "+it.Token {
		t.Fatalf("bearer 模式反代不符: %d %+v", rec.Code, got)
	}

	// 会话收尾：PATCH/DELETE 按 /w/sessions/{rid} 反代
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PATCH", "/providers/r1/w/sessions/rid9", strings.NewReader("a=x")))
	if len(got) != 3 || got[2].method != "PATCH" || got[2].path != "/w/sessions/rid9" {
		t.Fatalf("PATCH 应反代到 /w/sessions/rid9: %+v", got)
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

// Location 改写要认三种形态：上游返什么由它自己决定（livekit-ingress 不受我们控制），
// 只有纯相对形式本就落在代理路径下、不该动。
func TestRewriteWHIPLocation(t *testing.T) {
	const prefix = "/providers/ing1"
	cases := []struct {
		name, in, want string
	}{
		{"根相对", "/w/sessions/rid9", "/providers/ing1/w/sessions/rid9"},
		{"绝对 URL 只取路径", "http://ingress:8080/w/abc", "/providers/ing1/w/abc"},
		{"绝对 URL 带 query", "https://up.example.com/w/abc?k=1", "/providers/ing1/w/abc?k=1"},
		{"纯相对不动", "sessions/rid9", "sessions/rid9"},
		{"非 /w/ 路径不动", "/other/abc", "/other/abc"},
		{"绝对 URL 非 /w/ 不动", "http://ingress:8080/other/abc", "http://ingress:8080/other/abc"},
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
