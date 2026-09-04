// 推流令牌链路测试：admitIngest 全分支、令牌 API 三端点、三条 /w 路径
// （进程内 bellows 真 Gateway / 远端 bellows httptest 上游 / livekit-ingress httptest 上游）、
// Location 改写、游标 v3 数据迁移。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/pion/webrtc/v4"

	"hearth/server/internal/chat"
	"hearth/server/internal/config"
	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// seedIngestUser 造用户 + 频道 + 推流令牌（tag=obs）。
func seedIngestUser(t *testing.T, a *API, username, channel string) (*store.User, *store.IngestToken) {
	t.Helper()
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, username, "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if _, err := a.st.CreateChannel(ctx, channel, u.ID); err != nil {
		t.Fatalf("建频道失败: %v", err)
	}
	it, err := a.st.CreateIngestToken(ctx, u.ID, "obs")
	if err != nil {
		t.Fatalf("建令牌失败: %v", err)
	}
	return u, it
}

// admitIngest 全分支：令牌 404 / 频道 404 / 封禁 403 / 禁言 403 / 正常（identity=u{id}-{标签}，meta 带 uid/用户名）。
func TestAdmitIngestBranches(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "alice", "chan1")

	call := func(channel, token string) (int, ingestAdmission, bool) {
		rec := httptest.NewRecorder()
		adm, ok := a.admitIngest(ctx, rec, "bellows", channel, token)
		return rec.Code, adm, ok
	}

	if code, _, ok := call("chan1", "nope"); ok || code != 404 {
		t.Fatalf("未知令牌应 404，实际 %d ok=%v", code, ok)
	}
	if code, _, ok := call("", it.Token); ok || code != 404 {
		t.Fatalf("空频道应 404，实际 %d ok=%v", code, ok)
	}
	if code, _, ok := call("nosuch", it.Token); ok || code != 404 {
		t.Fatalf("未知频道应 404，实际 %d ok=%v", code, ok)
	}
	if err := a.st.Ban(ctx, 1, u.ID); err != nil { // chan1 是第一个频道，id=1
		t.Fatalf("封禁失败: %v", err)
	}
	if code, _, ok := call("chan1", it.Token); ok || code != 403 {
		t.Fatalf("封禁应 403，实际 %d ok=%v", code, ok)
	}
	a.st.Unban(ctx, 1, u.ID)
	if err := a.st.Gag(ctx, 1, u.ID); err != nil {
		t.Fatalf("禁言失败: %v", err)
	}
	if code, _, ok := call("chan1", it.Token); ok || code != 403 {
		t.Fatalf("禁言应 403，实际 %d ok=%v", code, ok)
	}
	a.st.Ungag(ctx, 1, u.ID)
	code, adm, ok := call("chan1", it.Token)
	if !ok || adm.Room != "chan1" || adm.Identity != rtc.Identity(u.ID, "obs") ||
		adm.Meta.UID != u.ID || adm.Meta.Username != "alice" || adm.Meta.Kind != "ingest" || adm.Meta.Tag != "obs" {
		t.Fatalf("正常推流判定不符: code=%d ok=%v adm=%+v", code, ok, adm)
	}
}

// 令牌 API：GET 自动创建（tag=obs，base 为同源 /providers/{alias}/w/ 绝对地址）、
// PUT 改标签（校验规则）、reset 换值且旧令牌立即 404。
func TestIngestTokenAPI(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	sess, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	r := a.Router()

	get := func() (int, map[string]string) {
		rec := doReq(t, r, "GET", "/api/ingest/token", sess, nil)
		var body map[string]string
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, body := get()
	if code != 200 || body["token"] == "" || body["tag"] != "obs" {
		t.Fatalf("GET 应自动创建令牌: %d %v", code, body)
	}
	if body["base"] != "http://example.com/providers/bellows/w/" {
		t.Fatalf("base 应为同源推流基地址，实际 %q", body["base"])
	}
	// 再 GET 不重建
	code2, body2 := get()
	if code2 != 200 || body2["token"] != body["token"] {
		t.Fatalf("重复 GET 不应换令牌: %q → %q", body["token"], body2["token"])
	}
	// 未登录 401
	if rec := doReq(t, r, "GET", "/api/ingest/token", "", nil); rec.Code != 401 {
		t.Fatalf("未登录应 401，实际 %d", rec.Code)
	}

	// PUT 改标签：非法标签 400，合法生效且令牌值不变
	if rec := doReq(t, r, "PUT", "/api/ingest/token", sess, map[string]string{"tag": "Bad_Tag"}); rec.Code != 400 {
		t.Fatalf("非法标签应 400，实际 %d", rec.Code)
	}
	rec := doReq(t, r, "PUT", "/api/ingest/token", sess, map[string]string{"tag": "cam-1"})
	var putBody map[string]string
	json.Unmarshal(rec.Body.Bytes(), &putBody)
	if rec.Code != 200 || putBody["tag"] != "cam-1" || putBody["token"] != body["token"] {
		t.Fatalf("改标签应生效且令牌不变: %d %v", rec.Code, putBody)
	}

	// reset：令牌换值，旧令牌立即失效
	rec = doReq(t, r, "POST", "/api/ingest/token/reset", sess, nil)
	var resetBody map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resetBody)
	if rec.Code != 200 || resetBody["token"] == "" || resetBody["token"] == body["token"] {
		t.Fatalf("reset 应换新令牌: %d %v", rec.Code, resetBody)
	}
	if resetBody["tag"] != "cam-1" {
		t.Fatalf("reset 应保留标签，实际 %q", resetBody["tag"])
	}
	wrec := httptest.NewRecorder()
	if _, ok := a.admitIngest(ctx, wrec, "bellows", "chan1", body["token"]); ok || wrec.Code != 404 {
		t.Fatalf("旧令牌应立即 404，实际 %d ok=%v", wrec.Code, ok)
	}
}

// ---- 进程内 bellows 路径（真 Gateway + 假舞台 Publisher）----

// fakeStagePublisher 实现 rtc.StageProvider + rtc.Publisher，作为内建 bellows 的发布出口。
type fakeStagePublisher struct {
	mu    sync.Mutex
	calls []string // identity
}

func (f *fakeStagePublisher) Name() string { return "fake-stage" }
func (f *fakeStagePublisher) JoinCredentials(context.Context, string, rtc.Meta, bool) (rtc.Credentials, error) {
	return rtc.Credentials{}, nil
}
func (f *fakeStagePublisher) RoomCounts(context.Context) (map[string]int, error) { return nil, nil }
func (f *fakeStagePublisher) ListParticipants(context.Context, string) ([]rtc.Participant, error) {
	return nil, nil
}
func (f *fakeStagePublisher) RemoveParticipantsOf(context.Context, string, int64, string) (int, error) {
	return 0, nil
}
func (f *fakeStagePublisher) MuteUserAudio(context.Context, string, int64, bool) error { return nil }
func (f *fakeStagePublisher) SignalProxyUpstream(context.Context) string               { return "" }
func (f *fakeStagePublisher) PublishRemote(_ context.Context, _, identity, _ string, _ rtc.Meta, _ *webrtc.TrackRemote) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, identity)
	return func() {}, nil
}

// 可解析的 H264 WHIP offer（audio/opus + video/h264，sendonly；字面量 \n 书写，返回前转 CRLF）。
func whipOfferH264() string {
	const offer = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0 1
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 0.0.0.0
a=mid:0
a=sendonly
a=rtcp-mux
a=rtpmap:111 opus/48000/2
a=ice-ufrag:test
a=ice-pwd:testtesttesttesttesttest
a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00
a=setup:actpass
a=candidate:1 1 UDP 2130706431 127.0.0.1 9 typ host
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:1
a=sendonly
a=rtcp-mux
a=rtpmap:96 H264/90000
a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f
a=ice-ufrag:test
a=ice-pwd:testtesttesttesttesttest
a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00
a=setup:actpass
a=candidate:1 1 UDP 2130706431 127.0.0.1 9 typ host
`
	return strings.ReplaceAll(offer, "\n", "\r\n")
}

// 进程内 bellows：POST 经 admitIngest → ctx 传四元组 → 真 Gateway 建会话（201），
// Location 改写为 /providers/bellows/w/sessions/{rid}；DELETE 按归属路由到内建网关；
// 令牌 reset 后 RevokeToken 掐断在推会话、旧令牌 404。
func TestWhipProcessInternalBellows(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "alice", "chan1")
	sess, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	// 发布出口：注册假舞台实例并选中（stagePublisherSink 每次发布时取当前舞台线）
	pub := &fakeStagePublisher{}
	a.providersMu.Lock()
	a.providers["fakestage"] = &ProviderInstance{Alias: "fakestage", Type: "fake", Stage: pub}
	a.providerOrder = append(a.providerOrder, "fakestage")
	a.providersMu.Unlock()
	a.st.SetSetting(ctx, "cfg_stage_provider", "fakestage")
	a.st.SetSetting(ctx, "cfg_bellows_udp_port", "47733")
	a.st.SetSetting(ctx, "cfg_bellows_public_ip", "127.0.0.1")

	r := a.Router()
	a.RegisterProxies(r)
	gw := a.instance(TypeBellows).Ingest.(rtc.WHIPServer)

	post := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(whipOfferH264())))
		return rec
	}

	// 未知令牌：404（admitIngest 拦截，不进网关）
	if rec := post("/providers/bellows/w/chan1/badtoken"); rec.Code != 404 {
		t.Fatalf("未知令牌应 404，实际 %d", rec.Code)
	}

	// 正常推流：201，Location 改写，会话在内建网关在册
	rec := post("/providers/bellows/w/chan1/" + it.Token)
	if rec.Code != 201 {
		t.Fatalf("进程内推流应 201，实际 %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/providers/bellows/w/sessions/") || strings.Contains(loc, it.Token) {
		t.Fatalf("Location 应改写且不含令牌，实际 %q", loc)
	}
	rid := strings.TrimPrefix(loc, "/providers/bellows/w/sessions/")
	if !gw.HasSession(rid) {
		t.Fatal("会话应在内建 bellows 在册")
	}

	// DELETE 按 rid 归属路由到内建网关 → 204
	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, httptest.NewRequest("DELETE", loc, nil))
	if drec.Code != 204 || gw.HasSession(rid) {
		t.Fatalf("DELETE 应 204 且会话移除: %d", drec.Code)
	}

	// 令牌 reset：进行中的会话被 RevokeToken 掐断，旧令牌 404
	rec = post("/providers/bellows/w/chan1/" + it.Token)
	if rec.Code != 201 {
		t.Fatalf("重推应 201，实际 %d", rec.Code)
	}
	rid2 := strings.TrimPrefix(rec.Header().Get("Location"), "/providers/bellows/w/sessions/")
	doReq(t, r, "POST", "/api/ingest/token/reset", sess, nil)
	if gw.HasSession(rid2) {
		t.Fatal("reset 后在推会话应被掐断")
	}
	if rec := post("/providers/bellows/w/chan1/" + it.Token); rec.Code != 404 {
		t.Fatalf("reset 后旧令牌应 404，实际 %d", rec.Code)
	}
}

// ---- livekit-ingress 路径 ----

// twirpCall 假 LiveKit Twirp API 记录的一次调用。
type twirpCall struct {
	op   string
	body map[string]any
}

// newFakeLivekit 起两个 httptest：Twirp API（CreateIngress/UpdateIngress/DeleteIngress）
// 与 WHIP 上游（记录 Bearer 与路径）。
func newFakeLivekit(t *testing.T) (twirpURL, whipURL string, calls *([]twirpCall), whipReqs *([][2]string)) {
	t.Helper()
	calls = &[]twirpCall{}
	twirp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		var op string
		switch {
		case strings.HasSuffix(r.URL.Path, "/CreateIngress"):
			op = "create"
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ingressId":"in1","streamKey":"sk1"}`))
		case strings.HasSuffix(r.URL.Path, "/UpdateIngress"):
			op = "update"
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ingressId":"in1"}`))
		case strings.HasSuffix(r.URL.Path, "/DeleteIngress"):
			op = "delete"
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		default:
			t.Errorf("未知 Twirp 路径: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		*calls = append(*calls, twirpCall{op, body})
	}))
	t.Cleanup(twirp.Close)
	whipReqs = &[][2]string{}
	whip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*whipReqs = append(*whipReqs, [2]string{r.Header.Get("Authorization"), r.URL.Path})
		// 真实 ingress 的 Location 形态不受我们控制，这里用绝对 URL（上游主机对客户端不可达）
		w.Header().Set("Location", "http://"+r.Host+"/w/sessions/upstream-rid")
		w.WriteHeader(201)
	}))
	t.Cleanup(whip.Close)
	return twirp.URL, whip.URL, calls, whipReqs
}

// livekit-ingress：首次推流 EnsureEndpoint 建端点（惰性，每令牌每实例）+ BindRoom 到 URL 频道，
// Bearer 改写为上游 stream key、路径规范为 /w 后反代；换频道再推只 BindRoom 不重建端点；
// reset 删端点（DeleteEndpoint）且旧令牌 404。
func TestWhipLivekitIngress(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "alice", "chan1")
	if _, err := a.st.CreateChannel(ctx, "chan2", u.ID); err != nil {
		t.Fatalf("建频道失败: %v", err)
	}
	sess, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	twirpURL, whipURL, calls, whipReqs := newFakeLivekit(t)
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "ing1", Type: TypeLivekitIngress, Params: map[string]string{
		"livekit_api_url": twirpURL, "livekit_api_key": "k", "livekit_api_secret": "s",
		"ingress_upstream_url": whipURL}})
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	post := func(channel, token string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/ing1/w/"+channel+"/"+token, strings.NewReader("sdp")))
		return rec
	}

	if rec := post("chan1", "badtoken"); rec.Code != 404 || len(*calls) != 0 || len(*whipReqs) != 0 {
		t.Fatalf("未知令牌应 404 且不打上游: %d calls=%d whip=%d", rec.Code, len(*calls), len(*whipReqs))
	}

	first := post("chan1", it.Token)
	if first.Code != 201 {
		t.Fatalf("ingress 推流应 201，实际 %d: %s", first.Code, first.Body.String())
	}
	// 会话资源地址必须改写回同源代理路径，否则客户端的 PATCH/DELETE 会打到已删除的 /w/...
	if loc := first.Header().Get("Location"); loc != "/providers/ing1/w/sessions/upstream-rid" {
		t.Fatalf("Location 应改写为同源代理路径，实际 %q", loc)
	}
	// 控制面：CreateIngress（identity=u{id}-obs，name=用户名）+ UpdateIngress 换房到 chan1
	if len(*calls) != 2 || (*calls)[0].op != "create" || (*calls)[1].op != "update" {
		t.Fatalf("首推应 create+update: %+v", *calls)
	}
	if (*calls)[0].body["participant_identity"] != rtc.Identity(u.ID, "obs") ||
		(*calls)[0].body["participant_name"] != "alice" {
		t.Fatalf("CreateIngress 身份不符: %+v", (*calls)[0].body)
	}
	if (*calls)[1].body["room_name"] != "chan1" {
		t.Fatalf("BindRoom 应为 chan1: %+v", (*calls)[1].body)
	}
	// 反代：Bearer 改写为上游 stream key，路径规范为 /w
	if len(*whipReqs) != 1 || (*whipReqs)[0][0] != "Bearer sk1" || (*whipReqs)[0][1] != "/w" {
		t.Fatalf("上游应收到 Bearer sk1 与精确 /w: %+v", *whipReqs)
	}

	// 稳态同频道：零控制面调用
	if rec := post("chan1", it.Token); rec.Code != 201 || len(*calls) != 2 {
		t.Fatalf("稳态推流不应触发控制面调用: %d calls=%d", rec.Code, len(*calls))
	}
	// 换频道：只 BindRoom 不重建端点
	if rec := post("chan2", it.Token); rec.Code != 201 || len(*calls) != 3 || (*calls)[2].op != "update" ||
		(*calls)[2].body["room_name"] != "chan2" {
		t.Fatalf("换频道应只 BindRoom: %d %+v", rec.Code, *calls)
	}

	// reset：端点删除（DeleteIngress）+ 记录清空，旧令牌 404
	doReq(t, r, "POST", "/api/ingest/token/reset", sess, nil)
	if len(*calls) != 4 || (*calls)[3].op != "delete" || (*calls)[3].body["ingress_id"] != "in1" {
		t.Fatalf("reset 应 DeleteIngress(in1): %+v", *calls)
	}
	newTok, err := a.st.IngestTokenByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("reset 后应能查到新令牌: %v", err)
	}
	if ep, err := a.st.IngestEndpoint(ctx, newTok.ID, "ing1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reset 后端点记录应清空: %+v %v", ep, err)
	}
	if rec := post("chan1", it.Token); rec.Code != 404 {
		t.Fatalf("reset 后旧令牌应 404，实际 %d", rec.Code)
	}
}

// ---- 游标 v3 数据迁移 ----

// 多频道多密钥合并为一把（取最近创建的）、ingresses 表消失、游标推进且不重复执行（幂等）。
func TestMigrateIngestTokensV3(t *testing.T) {
	maskProviderEnv(t)
	path := t.TempDir() + "/test.db"
	s, err := store.Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	u1, err := s.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	u2, err := s.CreateUser(ctx, "bob", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	c1, _ := s.CreateChannel(ctx, "chan1", u1.ID)
	c2, _ := s.CreateChannel(ctx, "chan2", u1.ID)
	// 旧 ingresses 表塞旧数据：alice 在两个频道各有一把（合并后应保留 id 最大的 key-new），bob 一把
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开原始连接失败: %v", err)
	}
	for _, row := range []struct {
		uid, cid int64
		iid, key string
	}{
		{u1.ID, c1.ID, "in1", "key-old"},
		{u1.ID, c2.ID, "in2", "key-new"},
		{u2.ID, c1.ID, "in3", "key-bob"},
	} {
		if _, err := raw.Exec(
			"INSERT INTO ingresses (user_id, channel_id, ingress_id, stream_key) VALUES (?, ?, ?, ?)",
			row.uid, row.cid, row.iid, row.key); err != nil {
			t.Fatalf("塞旧数据失败: %v", err)
		}
	}
	raw.Close()

	a := New(s, config.Load(), chat.NewHub(s, ""), nil) // New 内跑 v1+v2+v3
	it, err := s.IngestTokenByUser(ctx, u1.ID)
	if err != nil || it.Token != "key-new" || it.Tag != "obs" {
		t.Fatalf("多频道密钥应合并为最近创建的一把: %+v %v", it, err)
	}
	if it, err := s.IngestTokenByUser(ctx, u2.ID); err != nil || it.Token != "key-bob" {
		t.Fatalf("bob 的密钥应保留: %+v %v", it, err)
	}
	// ingresses 表消失（重读返回空而非报错）
	if legacy, err := s.LegacyIngressTokens(ctx); err != nil || len(legacy) != 0 {
		t.Fatalf("ingresses 表应已 DROP: %v %v", legacy, err)
	}
	if v, _ := s.MigrationVersion(ctx); v != 5 {
		t.Fatalf("游标应为最新版本 5，实际 %d", v)
	}
	// 幂等：重跑不覆盖用户后续改动（改标签后重跑，令牌与标签都保持）
	if err := s.UpdateIngestTokenTag(ctx, u1.ID, "cam"); err != nil {
		t.Fatalf("改标签失败: %v", err)
	}
	a.runMigrations(ctx)
	it, err = s.IngestTokenByUser(ctx, u1.ID)
	if err != nil || it.Token != "key-new" || it.Tag != "cam" {
		t.Fatalf("迁移重跑不应覆盖已有令牌: %+v %v", it, err)
	}
}

// 空库不建令牌，且 v3 在全新库上正常推进（ingresses 空表直接 DROP）。
func TestMigrateV3EmptyDB(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	if v, _ := a.st.MigrationVersion(ctx); v != 5 {
		t.Fatalf("空库游标应为最新版本 5，实际 %d", v)
	}
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if _, err := a.st.IngestTokenByUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("空库不应自动建令牌: %v", err)
	}
	a.runMigrations(ctx) // 重跑（旧表已删）不报错、不建令牌
	if _, err := a.st.IngestTokenByUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("重跑后仍不应有令牌: %v", err)
	}
}

// 分发路由里 /w 子路径对无推流能力的实例 404（回归保护：serveProvider 分支顺序）。
func TestWhipNoIngestCap(t *testing.T) {
	a := testAPI(t)
	r := chi.NewRouter()
	a.RegisterProxies(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/ember/w/chan1/x", strings.NewReader("sdp")))
	if rec.Code != 404 {
		t.Fatalf("无推流能力的实例应 404，实际 %d", rec.Code)
	}
}

// ---- 改名/并发/可用性回归 ----

// 改用户名必须让上游端点失效重建：端点的 name/metadata 在建端点时固化，复用旧端点会让
// 推流顶着旧用户名显示。identity 换成 u{id} 后归属本身不再受改名影响（管制照常命中），
// 这里保的是展示信息的一致性。
func TestRenameTearsDownIngestEndpoint(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "alice", "chan1")
	sess, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	twirpURL, whipURL, calls, _ := newFakeLivekit(t)
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "ing1", Type: TypeLivekitIngress, Params: map[string]string{
		"livekit_api_url": twirpURL, "livekit_api_key": "k", "livekit_api_secret": "s",
		"ingress_upstream_url": whipURL}})
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	post := func(channel, token string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/ing1/w/"+channel+"/"+token, strings.NewReader("sdp")))
		return rec
	}
	if rec := post("chan1", it.Token); rec.Code != 201 {
		t.Fatalf("首推应 201，实际 %d", rec.Code)
	}
	if (*calls)[0].body["participant_identity"] != rtc.Identity(u.ID, "obs") ||
		(*calls)[0].body["participant_name"] != "alice" {
		t.Fatalf("首推身份不符: %+v", (*calls)[0].body)
	}

	// 改名 → 端点应被删除（DeleteIngress）且记录清空
	if rec := doReq(t, r, "POST", "/api/account/username", sess, map[string]string{"username": "bob"}); rec.Code != 200 {
		t.Fatalf("改名应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(*calls); n != 3 || (*calls)[2].op != "delete" {
		t.Fatalf("改名应触发 DeleteIngress: %+v", *calls)
	}
	if ep, eerr := a.st.IngestEndpoint(ctx, it.ID, "ing1"); !errors.Is(eerr, store.ErrNotFound) {
		t.Fatalf("改名后端点记录应清空: %+v %v", ep, eerr)
	}

	// 令牌不变（令牌是用户维度凭证，与用户名无关），下次推流按新用户名重建端点
	if rec := post("chan1", it.Token); rec.Code != 201 {
		t.Fatalf("改名后同一令牌应仍可推流，实际 %d", rec.Code)
	}
	created := (*calls)[3]
	// identity 只认 user_id：改名前后不变（归属稳定）；展示名与元数据按新用户名重建
	if created.op != "create" || created.body["participant_identity"] != rtc.Identity(u.ID, "obs") ||
		created.body["participant_name"] != "bob" {
		t.Fatalf("重建端点应保持 identity、换新展示名: %+v", created.body)
	}
}

// 并发首推只建一个上游端点：两个请求同时进来时，先建的那个若不落库就会带着有效
// stream key 永久残留（重置令牌也删不到）。
func TestConcurrentFirstPushCreatesOneEndpoint(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	_, it := seedIngestUser(t, a, "alice", "chan1")
	twirpURL, whipURL, calls, _ := newFakeLivekit(t)
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "ing1", Type: TypeLivekitIngress, Params: map[string]string{
		"livekit_api_url": twirpURL, "livekit_api_key": "k", "livekit_api_secret": "s",
		"ingress_upstream_url": whipURL}})
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	const n = 6
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest("POST",
				"/providers/ing1/w/chan1/"+it.Token, strings.NewReader("sdp")))
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 201 {
			t.Fatalf("并发推流 #%d 应 201，实际 %d", i, c)
		}
	}
	creates := 0
	for _, c := range *calls {
		if c.op == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("并发首推应只建一个上游端点，实际 %d 次 create: %+v", creates, *calls)
	}
}

// 令牌接口回报推流入口可用性：舞台线关闭时内建 bellows 取不到发布出口（Enabled=false），
// 地址照给但前端要能提示——否则用户填进 OBS 推起来才撞 503。
func TestIngestTokenReportsEnabled(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	sess, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	r := a.Router()
	get := func() map[string]any {
		rec := doReq(t, r, "GET", "/api/ingest/token", sess, nil)
		if rec.Code != 200 {
			t.Fatalf("GET 令牌应 200，实际 %d", rec.Code)
		}
		var body map[string]any
		if uerr := json.Unmarshal(rec.Body.Bytes(), &body); uerr != nil {
			t.Fatalf("解析失败: %v", uerr)
		}
		return body
	}
	// 舞台线显式关闭（stage=none）：进程内 bellows 无发布出口
	//（默认部署 stage=lkembed，发布出口恒在，enabled 恒为 true）
	a.st.SetSetting(ctx, "cfg_stage_provider", "none")
	if got := get()["enabled"]; got != false {
		t.Fatalf("舞台线关闭时 enabled 应为 false，实际 %v", got)
	}
	// 接上舞台线的 Publisher 后转为可用
	a.providersMu.Lock()
	a.providers["fakestage"] = &ProviderInstance{Alias: "fakestage", Type: "fake", Stage: &fakeStagePublisher{}}
	a.providerOrder = append(a.providerOrder, "fakestage")
	a.providersMu.Unlock()
	a.st.SetSetting(ctx, "cfg_stage_provider", "fakestage")
	if got := get()["enabled"]; got != true {
		t.Fatalf("发布出口可用时 enabled 应为 true，实际 %v", got)
	}
}

// 迁移 v4：identity 主体由用户名改成 user_id 后，存量上游端点里固化的身份全部过期——
// 逐实例 DeleteEndpoint 后清空 ingest_endpoints，下次推流按新 identity 重建。
func TestMigrateEndpointIdentityV4(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	_, it := seedIngestUser(t, a, "alice", "chan1")
	twirpURL, whipURL, calls, _ := newFakeLivekit(t)
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "ing1", Type: TypeLivekitIngress, Params: map[string]string{
		"livekit_api_url": twirpURL, "livekit_api_key": "k", "livekit_api_secret": "s",
		"ingress_upstream_url": whipURL}})
	a.reloadProviders(ctx)

	// 造一条「旧 identity」的存量端点
	if err := a.st.UpsertIngestEndpoint(ctx, &store.IngestEndpoint{
		TokenID: it.ID, Alias: "ing1", IngressID: "old-in", UpstreamKey: "old-sk", BoundRoom: "chan1"}); err != nil {
		t.Fatalf("造端点失败: %v", err)
	}
	// 归属实例已注销的端点：内核侧删不了，但记录同样要清（不能挡住迁移）
	if err := a.st.UpsertIngestEndpoint(ctx, &store.IngestEndpoint{
		TokenID: it.ID, Alias: "gone", IngressID: "orphan", UpstreamKey: "k2"}); err != nil {
		t.Fatalf("造孤儿端点失败: %v", err)
	}

	a.st.SetMigrationVersion(ctx, 3) // 回到 v4 之前
	a.runMigrations(ctx)

	if v, _ := a.st.MigrationVersion(ctx); v != 5 {
		t.Fatalf("游标应推进到最新版本 5，实际 %d", v)
	}
	deleted := 0
	for _, c := range *calls {
		if c.op == "delete" && c.body["ingress_id"] == "old-in" {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("应对在册实例的旧端点调一次 DeleteIngress: %+v", *calls)
	}
	for _, alias := range []string{"ing1", "gone"} {
		if ep, err := a.st.IngestEndpoint(ctx, it.ID, alias); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("端点 %s 应已清空: %+v %v", alias, ep, err)
		}
	}
	// 幂等：空表重跑不报错、不再发删除
	before := len(*calls)
	a.st.SetMigrationVersion(ctx, 3)
	a.runMigrations(ctx)
	if len(*calls) != before {
		t.Fatalf("空表重跑不应再调控制面: %+v", *calls)
	}
}

// 管理操作的目标是 user_id：改名后对新 user_id 的禁言/踢出照常命中同一批参与者，
// 且不会误伤「用户名与他人 identity 同形」的第三方（旧的前缀匹配会）。
func TestModerationTargetsUserID(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	owner, err := a.st.CreateUser(ctx, "owner", "x")
	if err != nil {
		t.Fatalf("建房主失败: %v", err)
	}
	c, err := a.st.CreateChannel(ctx, "chan1", owner.ID)
	if err != nil {
		t.Fatalf("建频道失败: %v", err)
	}
	target, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建目标用户失败: %v", err)
	}
	sess, err := a.st.CreateSession(ctx, owner.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	r := a.Router()

	// 按 user_id 禁言 → 落库
	rec := doReq(t, r, "POST", "/api/channels/chan1/mute", sess, map[string]any{"user_id": target.ID})
	if rec.Code != 200 {
		t.Fatalf("禁言应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	gagged, err := a.st.IsGagged(ctx, c.ID, target.ID)
	if err != nil || !gagged {
		t.Fatalf("禁言应落库: %v %v", gagged, err)
	}

	// 改名后同一 user_id 仍能解禁（旧实现按用户名发，改名即失联）
	if err := a.st.UpdateUsername(ctx, target.ID, "bob"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	rec = doReq(t, r, "POST", "/api/channels/chan1/unmute", sess, map[string]any{"user_id": target.ID})
	if rec.Code != 200 {
		t.Fatalf("改名后解禁应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if gagged, _ := a.st.IsGagged(ctx, c.ID, target.ID); gagged {
		t.Fatal("解禁应生效")
	}

	// 不存在的 user_id → 404；对自己操作 → 400
	if rec := doReq(t, r, "POST", "/api/channels/chan1/ban", sess, map[string]any{"user_id": 99999}); rec.Code != 404 {
		t.Fatalf("未知 user_id 应 404，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, "POST", "/api/channels/chan1/ban", sess, map[string]any{"user_id": owner.ID}); rec.Code != 400 {
		t.Fatalf("对自己操作应 400，实际 %d", rec.Code)
	}

	// 设备级踢出的归属校验按 user_id：别人的 identity 不接受
	rec = doReq(t, r, "POST", "/api/channels/chan1/kick", sess,
		map[string]any{"user_id": target.ID, "identity": rtc.Identity(owner.ID, "mac")})
	if rec.Code != 400 {
		t.Fatalf("设备不属于目标用户应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// v4 必须在生产启动顺序下也真的删掉上游端点：runMigrationSteps 跑在 New() 的
// reloadProviders 之前，注册表此刻只有内建实例——迁移不自己重建的话，
// livekit-ingress 类实例一个都取不到，端点连同有效 stream key 全部残留在上游。
func TestMigrateEndpointIdentityV4ProductionOrdering(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	_, it := seedIngestUser(t, a, "alice", "chan1")
	twirpURL, whipURL, calls, _ := newFakeLivekit(t)
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "ing1", Type: TypeLivekitIngress, Params: map[string]string{
		"livekit_api_url": twirpURL, "livekit_api_key": "k", "livekit_api_secret": "s",
		"ingress_upstream_url": whipURL}})
	if err := a.st.UpsertIngestEndpoint(ctx, &store.IngestEndpoint{
		TokenID: it.ID, Alias: "ing1", IngressID: "old-in", UpstreamKey: "old-sk", BoundRoom: "chan1"}); err != nil {
		t.Fatalf("造端点失败: %v", err)
	}

	// 把注册表退回「只有内建实例」的启动态：不预先 reload，复现 New() 的真实顺序
	a.providersMu.Lock()
	a.providers = map[string]*ProviderInstance{}
	a.providerOrder = nil
	for _, inst := range a.builtinInstances() {
		a.providers[inst.Alias] = inst
		a.providerOrder = append(a.providerOrder, inst.Alias)
	}
	a.providersMu.Unlock()

	a.st.SetMigrationVersion(ctx, 3)
	a.runMigrations(ctx)

	deleted := 0
	for _, c := range *calls {
		if c.op == "delete" && c.body["ingress_id"] == "old-in" {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("生产启动顺序下 v4 应删掉上游端点，实际 delete 次数 = %d（记录清空但上游残留）", deleted)
	}
	if ep, err := a.st.IngestEndpoint(ctx, it.ID, "ing1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("端点记录应清空: %+v %v", ep, err)
	}
}
