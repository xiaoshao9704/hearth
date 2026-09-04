// 推流链路测试：admitIngest 全分支（含「alias 必须是当前舞台实例」门禁）、
// 令牌 API 三端点、/w 路径（livekit 系实例的换票反代见 livekit_whip_test/lkembed_whip_test）、
// 游标 v3/v4 数据迁移。
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
	"testing"

	"github.com/go-chi/chi/v5"

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
// alias 用默认舞台实例 lkembed（门禁见 TestAdmitIngestStageGate）。
func TestAdmitIngestBranches(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "alice", "chan1")

	call := func(channel, token string) (int, ingestAdmission, bool) {
		rec := httptest.NewRecorder()
		adm, ok := a.admitIngest(ctx, rec, AliasLkembed, channel, token)
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

// 「推流进当前舞台实例」门禁：alias 段不是 stage_provider 选中的实例一律 definitive 404
//（内建 bellows、非舞台的 livekit 实例、未知 alias 同规则），stage=none 时任何推流 404。
// 真令牌也照 404——判定不看令牌有效性，门禁先于令牌反查。
func TestAdmitIngestStageGate(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	_, it := seedIngestUser(t, a, "alice", "chan1")
	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk2", Type: TypeLivekit,
		Params: map[string]string{"livekit_api_url": "http://x", "livekit_api_key": "k", "livekit_api_secret": "s"}}); err != nil {
		t.Fatalf("注册实例失败: %v", err)
	}
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	post := func(alias string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/"+alias+"/w/chan1/"+it.Token, strings.NewReader("sdp")))
		return rec.Code
	}

	// 默认舞台 lkembed：bellows（内建但不再是推流入口）、lk2（非当前舞台）、未知 alias 全 404
	for _, alias := range []string{TypeBellows, "lk2", "nope"} {
		if code := post(alias); code != 404 {
			t.Fatalf("alias %s 应 404，实际 %d", alias, code)
		}
	}
	// 当前舞台实例：过门禁后才查令牌（坏令牌 404 证明走到了令牌反查）
	if code := post(AliasLkembed); code == 404 {
		t.Fatalf("当前舞台实例 + 真令牌不应被门禁拦下（上游未起会 502，但不该 404）")
	}

	// stage=none：连当前舞台实例都没有，推流一律 404
	a.st.SetSetting(ctx, "cfg_stage_provider", "none")
	if code := post(AliasLkembed); code != 404 {
		t.Fatalf("stage=none 时推流应 404，实际 %d", code)
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
	if body["base"] != "http://example.com/providers/lkembed/w/" {
		t.Fatalf("base 应为当前舞台实例的同源推流基地址，实际 %q", body["base"])
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
	if _, ok := a.admitIngest(ctx, wrec, AliasLkembed, "chan1", body["token"]); ok || wrec.Code != 404 {
		t.Fatalf("旧令牌应立即 404，实际 %d ok=%v", wrec.Code, ok)
	}
}

// ---- livekit-ingress 假上游（供游标 v4 迁移测试用）----

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
		w.Header().Set("Location", "http://"+r.Host+"/w/sessions/upstream-rid")
		w.WriteHeader(201)
	}))
	t.Cleanup(whip.Close)
	return twirp.URL, whip.URL, calls, whipReqs
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

// 令牌接口回报推流入口可用性：enabled = 当前舞台实例的推流面可用（配置齐且在跑），
// 舞台线关闭时 base 给空串——前端据此提示，免得用户填进 OBS 推起来才撞 404。
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
	// 默认 stage=lkembed 但进程内 LiveKit 未启动（测试不拉内核）：地址照给，enabled=false
	body := get()
	if body["base"] != "http://example.com/providers/lkembed/w/" {
		t.Fatalf("base 应为 lkembed 的推流基地址，实际 %q", body["base"])
	}
	if body["enabled"] != false {
		t.Fatalf("内核未在跑时 enabled 应为 false，实际 %v", body["enabled"])
	}
	// 舞台线显式关闭（stage=none）：没有推流入口，base 空、enabled=false
	a.st.SetSetting(ctx, "cfg_stage_provider", "none")
	body = get()
	if body["base"] != "" || body["enabled"] != false {
		t.Fatalf("stage=none 时 base 应为空且 enabled=false: %v", body)
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
