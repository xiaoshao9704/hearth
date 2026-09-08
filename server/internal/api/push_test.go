package api

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"hearth/server/internal/store"
)

// ---- 假推送网关 ----

// fakeGateway 一个收推送的假网关：把每次 POST 的请求头与密文交给 reqs。
// status 可改，用来演 404/410/500 三种处置。
type fakeGateway struct {
	srv    *httptest.Server
	reqs   chan pushReq
	status int
}

type pushReq struct {
	ttl      string
	auth     string
	encoding string
	body     []byte
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{reqs: make(chan pushReq, 8), status: http.StatusCreated}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		g.reqs <- pushReq{
			ttl: r.Header.Get("TTL"), auth: r.Header.Get("Authorization"),
			encoding: r.Header.Get("Content-Encoding"), body: body,
		}
		w.WriteHeader(g.status)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func readAll(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// next 等一条推送到达（异步投递用）；超时即失败。
func (g *fakeGateway) next(t *testing.T) pushReq {
	t.Helper()
	select {
	case req := <-g.reqs:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到推送")
		return pushReq{}
	}
}

// none 断言这一小会儿没有任何推送到达。
func (g *fakeGateway) none(t *testing.T) {
	t.Helper()
	select {
	case <-g.reqs:
		t.Fatal("不该收到推送")
	case <-time.After(200 * time.Millisecond):
	}
}

// ---- 订阅密钥与解密（验证载荷形状）----

type subKeys struct {
	priv   *ecdh.PrivateKey
	p256dh string // base64url 的未压缩公钥
	auth   string // base64url 的 16 字节 auth secret
	secret []byte
}

func newSubKeys(t *testing.T) subKeys {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return subKeys{
		priv:   priv,
		p256dh: base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		auth:   base64.RawURLEncoding.EncodeToString(secret),
		secret: secret,
	}
}

// decryptPush 按 RFC 8188/8291 解开推送密文，取出我们发的 JSON 载荷。
// 只有真解得开才能证明"payload 形状正确"——密文里看不出字段。
func decryptPush(t *testing.T, body []byte, k subKeys) []byte {
	t.Helper()
	if len(body) < 22 {
		t.Fatalf("推送体过短: %d 字节", len(body))
	}
	salt := body[:16]
	idlen := int(body[20])
	if len(body) < 21+idlen {
		t.Fatal("推送体头部不完整")
	}
	asPubRaw := body[21 : 21+idlen]
	ciphertext := body[21+idlen:]
	asPub, err := ecdh.P256().NewPublicKey(asPubRaw)
	if err != nil {
		t.Fatalf("发送方公钥无效: %v", err)
	}
	shared, err := k.priv.ECDH(asPub)
	if err != nil {
		t.Fatalf("ECDH 失败: %v", err)
	}
	info := append([]byte("WebPush: info\x00"), k.priv.PublicKey().Bytes()...)
	info = append(info, asPubRaw...)
	ikm, err := hkdf.Key(sha256.New, shared, k.secret, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	// 明文 = JSON + 记录分隔符 0x02 + 补零；JSON 里不会出现 0x02
	if at := bytes.IndexByte(plain, 0x02); at >= 0 {
		plain = plain[:at]
	}
	return plain
}

// ---- 夹具 ----

// pushFixture 一个频道、发送者 alice（super）与接收者 bob，另返回假网关。
type pushEnv struct {
	a      *API
	gw     *fakeGateway
	ch     *store.Channel
	alice  *store.User
	bob    *store.User
	keys   subKeys
	dbPath string
}

func pushFixture(t *testing.T) *pushEnv {
	t.Helper()
	a, dbPath := testAPIWithDB(t)
	gw := newFakeGateway(t)
	a.pushHTTP = gw.srv.Client() // 注入点：投递走假网关的客户端
	ctx := context.Background()
	alice, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := a.st.CreateUser(ctx, "bob", "x")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.st.CreateChannel(ctx, "general", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	keys := newSubKeys(t)
	if err := a.st.SavePushSubscription(ctx, bob.ID, "sess-bob", gw.srv.URL+"/push", keys.p256dh, keys.auth, "ua"); err != nil {
		t.Fatal(err)
	}
	return &pushEnv{a: a, gw: gw, ch: ch, alice: alice, bob: bob, keys: keys, dbPath: dbPath}
}

// notify 同步跑一轮推送判定与投递（不经 HTTP handler 的 goroutine，断言不用等）。
func (e *pushEnv) notify(t *testing.T, m *store.Message, mentions []int64) {
	t.Helper()
	e.a.notifyMessage(context.Background(), e.ch.Name, m, mentions, "hearth.example.com")
}

func (e *pushEnv) post(t *testing.T, uid int64, content string, replyTo *int64) *store.Message {
	t.Helper()
	m, err := e.a.st.AddMessage(context.Background(), e.ch.ID, uid, store.KindText, content, nil, replyTo)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *pushEnv) subCount(t *testing.T, uid int64) int {
	t.Helper()
	n, err := e.a.st.CountPushSubscriptions(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// rawMeta 直读消息的 meta 列（接口不回显它，只能开一条连接看）。
func (e *pushEnv) rawMeta(t *testing.T, id int64) string {
	t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer db.Close()
	var meta sql.NullString
	if err := db.QueryRow("SELECT meta FROM messages WHERE id = ?", id).Scan(&meta); err != nil {
		t.Fatalf("读 meta 失败: %v", err)
	}
	return meta.String
}

// ---- 提及校验（安全边界）----

func TestMentionsUsernameBoundaries(t *testing.T) {
	cases := []struct {
		content, username string
		want              bool
	}{
		{"@bob 你好", "bob", true},
		{"@bob", "bob", true},
		{"你好 @bob，来一下", "bob", true},
		{"（@bob）", "bob", true},
		{"@abc 在吗", "ab", false},       // 互为前缀：@abc 不是在提 ab
		{"@ab 在吗", "abc", false},       // 反向也不行
		{"mail@bob.com", "bob", false}, // 邮箱里的 @ 不算提及
		{"a@bob", "bob", false},
		{"@bob-2 来", "bob", false}, // 名字含 - ，边界不匹配就不算
		{"", "bob", false},
		{"@bob", "", false},
	}
	for _, c := range cases {
		if got := mentionsUsername(c.content, c.username); got != c.want {
			t.Errorf("mentionsUsername(%q, %q) = %v, want %v", c.content, c.username, got, c.want)
		}
	}
}

func TestValidMentionsDropsUnmentioned(t *testing.T) {
	e := pushFixture(t)
	ctx := context.Background()
	// 正文根本没提 bob：客户端硬塞 uid 也不算
	if got := e.a.validMentions(ctx, "今天天气不错", []int64{e.bob.ID}); len(got) != 0 {
		t.Fatalf("未在正文出现的 uid 应被丢弃: %v", got)
	}
	// 不存在的用户、非法 uid 一律丢
	if got := e.a.validMentions(ctx, "@nobody 在吗", []int64{9999, 0, -1}); len(got) != 0 {
		t.Fatalf("不存在的 uid 应被丢弃: %v", got)
	}
	// 真提到了才算，且去重
	got := e.a.validMentions(ctx, "@bob 在吗", []int64{e.bob.ID, e.bob.ID})
	if len(got) != 1 || got[0] != e.bob.ID {
		t.Fatalf("正文提到的 uid 应保留一次: %v", got)
	}
}

// ---- 目标集合 ----

func TestPushTargetsExcludesSenderAndMuted(t *testing.T) {
	e := pushFixture(t)
	ctx := context.Background()
	// 发送者自己被算进 mentions 也不推给自己
	if err := e.a.st.SavePushSubscription(ctx, e.alice.ID, "sess-alice", e.gw.srv.URL+"/alice", e.keys.p256dh, e.keys.auth, "ua"); err != nil {
		t.Fatal(err)
	}
	m := e.post(t, e.alice.ID, "@alice @bob 看这里", nil)
	targets, err := e.a.pushTargets(ctx, m, []int64{e.alice.ID, e.bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := targets[e.alice.ID]; ok {
		t.Fatal("发送者不该在推送目标里")
	}
	if targets[e.bob.ID] != "mention" {
		t.Fatalf("被@的人应是 mention 目标: %v", targets)
	}
	// bob 静音该频道后整条判定为空
	if err := e.a.st.MuteChannel(ctx, e.bob.ID, e.ch.ID); err != nil {
		t.Fatal(err)
	}
	targets, err = e.a.pushTargets(ctx, m, []int64{e.bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("静音的人不该在推送目标里: %v", targets)
	}
}

func TestPushTargetsIncludesReplyAuthor(t *testing.T) {
	e := pushFixture(t)
	quoted := e.post(t, e.bob.ID, "原话", nil)
	reply := e.post(t, e.alice.ID, "接着说", &quoted.ID)
	targets, err := e.a.pushTargets(context.Background(), reply, nil)
	if err != nil {
		t.Fatal(err)
	}
	if targets[e.bob.ID] != "reply" {
		t.Fatalf("被回复的作者应是 reply 目标: %v", targets)
	}
}

// ---- 投递 ----

func TestPushDeliveryPayload(t *testing.T) {
	e := pushFixture(t)
	m := e.post(t, e.alice.ID, "@bob 你好", nil)
	e.notify(t, m, []int64{e.bob.ID})
	req := e.gw.next(t)
	if req.encoding != "aes128gcm" || req.ttl != "600" {
		t.Fatalf("推送请求头不符: encoding=%q ttl=%q", req.encoding, req.ttl)
	}
	if len(req.auth) < 6 || req.auth[:6] != "vapid " {
		t.Fatalf("缺少 VAPID 授权头: %q", req.auth)
	}
	var got pushPayload
	if err := json.Unmarshal(decryptPush(t, req.body, e.keys), &got); err != nil {
		t.Fatalf("解析推送载荷失败: %v", err)
	}
	want := pushPayload{Kind: "mention", Channel: "general", ChannelID: e.ch.ID,
		MessageID: m.ID, From: "alice", Preview: "@bob 你好"}
	if got != want {
		t.Fatalf("推送载荷不符:\n got %+v\nwant %+v", got, want)
	}
	e.gw.none(t) // 一个目标一条订阅，只该有一条
}

func TestPushDeliveryDropsGoneSubscription(t *testing.T) {
	e := pushFixture(t)
	e.gw.status = http.StatusGone // 410：这个地址作废了
	m := e.post(t, e.alice.ID, "@bob 你好", nil)
	e.notify(t, m, []int64{e.bob.ID})
	e.gw.next(t)
	if n := e.subCount(t, e.bob.ID); n != 0 {
		t.Fatalf("410 后订阅应被删除，实际还剩 %d 条", n)
	}
}

func TestPushDeliveryFailCountDeletes(t *testing.T) {
	e := pushFixture(t)
	e.gw.status = http.StatusInternalServerError
	ctx := context.Background()
	subs, err := e.a.st.SubscriptionsOf(ctx, []int64{e.bob.ID})
	if err != nil || len(subs) != 1 {
		t.Fatalf("取订阅失败: %v %v", subs, err)
	}
	// 先攒到上限前一次，再失败一次即删（避免真发十轮）
	for i := 0; i < pushMaxFail-1; i++ {
		if _, err := e.a.st.IncPushFailure(ctx, subs[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	m := e.post(t, e.alice.ID, "@bob 你好", nil)
	e.notify(t, m, []int64{e.bob.ID})
	e.gw.next(t)
	if n := e.subCount(t, e.bob.ID); n != 0 {
		t.Fatalf("失败累计到上限后订阅应被删除，实际还剩 %d 条", n)
	}
	// 成功一次要清零计数：换一条新订阅走通路验证
	e.gw.status = http.StatusCreated
	if err := e.a.st.SavePushSubscription(ctx, e.bob.ID, "sess-bob", e.gw.srv.URL+"/push2", e.keys.p256dh, e.keys.auth, "ua"); err != nil {
		t.Fatal(err)
	}
	e.notify(t, m, []int64{e.bob.ID})
	e.gw.next(t)
	if n := e.subCount(t, e.bob.ID); n != 1 {
		t.Fatalf("成功投递不该删订阅，实际 %d 条", n)
	}
}

// ---- 端点 ----

func TestPushEndpointsAndMute(t *testing.T) {
	e := pushFixture(t)
	r := e.a.Router()
	ctx := context.Background()
	token, err := e.a.st.CreateSession(ctx, e.bob.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 公钥：首次访问即生成落库
	rec := doReq(t, r, http.MethodGet, "/api/push/vapid", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("取公钥状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var vapid struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &vapid); err != nil || vapid.PublicKey == "" {
		t.Fatalf("公钥响应不符: %s (%v)", rec.Body.String(), err)
	}
	if stored, err := e.a.st.GetSetting(ctx, "cfg_webpush_vapid_public"); err != nil || stored != vapid.PublicKey {
		t.Fatalf("公钥应落库: %q %v", stored, err)
	}
	// 未登录一律 401
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/push/vapid"},
		{http.MethodPost, "/api/push/subscribe"},
		{http.MethodPut, "/api/channels/general/mute"},
		{http.MethodDelete, "/api/channels/general/mute"},
	} {
		if rec := doReq(t, r, tc.method, tc.path, "", map[string]any{}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录状态码=%d, want 401", tc.method, tc.path, rec.Code)
		}
	}

	// 订阅：形状校验 + 落库 + 同一 endpoint 重复提交是刷新
	if rec := doReq(t, r, http.MethodPost, "/api/push/subscribe", token,
		map[string]any{"endpoint": "", "keys": map[string]string{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("空订阅状态码=%d, want 400", rec.Code)
	}
	if rec := doReq(t, r, http.MethodPost, "/api/push/subscribe", token,
		map[string]any{"endpoint": "ftp://x/y", "keys": map[string]string{"p256dh": "a", "auth": "b"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("非 http 地址状态码=%d, want 400", rec.Code)
	}
	body := map[string]any{
		"endpoint": e.gw.srv.URL + "/push-via-api",
		"keys":     map[string]string{"p256dh": e.keys.p256dh, "auth": e.keys.auth},
	}
	if rec := doReq(t, r, http.MethodPost, "/api/push/subscribe", token, body); rec.Code != http.StatusNoContent {
		t.Fatalf("订阅状态码=%d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, "/api/push/subscribe", token, body); rec.Code != http.StatusNoContent {
		t.Fatalf("重复订阅状态码=%d", rec.Code)
	}
	if n := e.subCount(t, e.bob.ID); n != 2 { // 夹具里那条 + 这条
		t.Fatalf("订阅条数=%d, want 2", n)
	}
	// 退订
	if rec := doReq(t, r, http.MethodDelete, "/api/push/subscribe", token,
		map[string]any{"endpoint": body["endpoint"]}); rec.Code != http.StatusNoContent {
		t.Fatalf("退订状态码=%d", rec.Code)
	}
	if n := e.subCount(t, e.bob.ID); n != 1 {
		t.Fatalf("退订后订阅条数=%d, want 1", n)
	}

	// 静音端点 + GET /api/channels 的 muted 翻转
	if muted := channelMuted(t, r, token); muted {
		t.Fatal("默认不该是静音")
	}
	if rec := doReq(t, r, http.MethodPut, "/api/channels/general/mute", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("静音状态码=%d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPut, "/api/channels/general/mute", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("重复静音应幂等，状态码=%d", rec.Code)
	}
	if !channelMuted(t, r, token) {
		t.Fatal("静音后 muted 应为 true")
	}
	if rec := doReq(t, r, http.MethodDelete, "/api/channels/general/mute", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("取消静音状态码=%d", rec.Code)
	}
	if channelMuted(t, r, token) {
		t.Fatal("取消静音后 muted 应为 false")
	}
	// 不存在的频道 404（同路径的禁言管制不受影响）
	if rec := doReq(t, r, http.MethodPut, "/api/channels/nope/mute", token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("未知频道静音状态码=%d, want 404", rec.Code)
	}

	// 退出登录：这条会话留下的订阅一起删
	if rec := doReq(t, r, http.MethodPost, "/api/logout", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("退出登录状态码=%d", rec.Code)
	}
	if n := e.subCount(t, e.bob.ID); n != 1 { // 夹具那条绑的是别的会话指纹，不受影响
		t.Fatalf("退出登录后订阅条数=%d, want 1", n)
	}
}

// TestPushSubscriptionDroppedOnLogout 订阅绑在会话上：这条会话退出即删它自己的订阅。
func TestPushSubscriptionDroppedOnLogout(t *testing.T) {
	e := pushFixture(t)
	r := e.a.Router()
	ctx := context.Background()
	token, err := e.a.st.CreateSession(ctx, e.bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.st.SavePushSubscription(ctx, e.bob.ID, store.SessionID(token), e.gw.srv.URL+"/only", e.keys.p256dh, e.keys.auth, "ua"); err != nil {
		t.Fatal(err)
	}
	if n := e.subCount(t, e.bob.ID); n != 2 {
		t.Fatalf("订阅条数=%d, want 2", n)
	}
	if rec := doReq(t, r, http.MethodPost, "/api/logout", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("退出登录状态码=%d", rec.Code)
	}
	if n := e.subCount(t, e.bob.ID); n != 1 {
		t.Fatalf("退出登录后应只删本会话的订阅，实际剩 %d 条", n)
	}
}

// channelMuted 从频道列表里读 general 的 muted。
func channelMuted(t *testing.T, r *chi.Mux, token string) bool {
	t.Helper()
	rec := doReq(t, r, http.MethodGet, "/api/channels", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("取频道列表状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Channels []store.Channel `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析频道列表失败: %v", err)
	}
	for _, c := range resp.Channels {
		if c.Name == "general" {
			return c.Muted
		}
	}
	t.Fatal("频道列表里没有 general")
	return false
}

// TestPostMessagePushesAsync 走完整 HTTP 路径：发一条带 @ 的消息 → 假网关收到一次推送；
// 目标静音后同一条路径不再推。
func TestPostMessagePushesAsync(t *testing.T) {
	e := pushFixture(t)
	r := e.a.Router()
	ctx := context.Background()
	aliceToken, err := e.a.st.CreateSession(ctx, e.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/channels/general/messages"
	rec := doReq(t, r, http.MethodPost, path, aliceToken,
		map[string]any{"content": "@bob 你好", "mentions": []int64{e.bob.ID}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("发消息状态码=%d: %s", rec.Code, rec.Body.String())
	}
	req := e.gw.next(t)
	var got pushPayload
	if err := json.Unmarshal(decryptPush(t, req.body, e.keys), &got); err != nil {
		t.Fatalf("解析推送载荷失败: %v", err)
	}
	if got.Kind != "mention" || got.From != "alice" || got.Preview != "@bob 你好" {
		t.Fatalf("推送载荷不符: %+v", got)
	}
	// 校验过的提及落进了消息 meta
	msgs, err := e.a.st.RecentMessages(ctx, e.ch.ID, 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("取消息失败: %v", err)
	}
	if meta := e.rawMeta(t, msgs[0].ID); meta != `{"mentions":[`+strconv.FormatInt(e.bob.ID, 10)+`]}` {
		t.Fatalf("meta 应记下校验过的提及，实际 %s", meta)
	}

	// 没被提及也不是回复：不推
	if rec := doReq(t, r, http.MethodPost, path, aliceToken, map[string]any{"content": "自言自语"}); rec.Code != http.StatusCreated {
		t.Fatalf("发消息状态码=%d", rec.Code)
	}
	e.gw.none(t)

	// bob 静音后即使被 @ 也不推
	if err := e.a.st.MuteChannel(ctx, e.bob.ID, e.ch.ID); err != nil {
		t.Fatal(err)
	}
	if rec := doReq(t, r, http.MethodPost, path, aliceToken,
		map[string]any{"content": "@bob 再说一次", "mentions": []int64{e.bob.ID}}); rec.Code != http.StatusCreated {
		t.Fatalf("发消息状态码=%d", rec.Code)
	}
	e.gw.none(t)
}
