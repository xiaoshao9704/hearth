// WHIP 信令握手测试：h264 offer → 201 + answer SDP + Content-Length + /w/sessions/{rid} 资源地址；
// 未知令牌 404；反查故障 503；非白名单编码 400；DELETE → 204；同令牌换房间顶替旧会话；
// Publisher 透传 identity/name/meta；RevokeToken 幂等。
package bellows

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"hearth/server/internal/rtc"
)

// publishCall 假 Publisher 记录的一次发布。
type publishCall struct {
	room, identity, name string
	meta                 rtc.Meta
}

type fakePublisher struct {
	mu    sync.Mutex
	calls []publishCall
	lost  func() // 最近一次发布时 ctx 上挂的「发布出口已断」回执
}

func (f *fakePublisher) PublishRemote(ctx context.Context, room, identity, name string, meta rtc.Meta, _ *webrtc.TrackRemote) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, publishCall{room, identity, name, meta})
	f.lost = rtc.PublishLost(ctx)
	return func() {}, nil
}

func (f *fakePublisher) lostFn() func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lost
}

func (f *fakePublisher) last() publishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return publishCall{}
	}
	return f.calls[len(f.calls)-1]
}

// testGateway 进程内形态：resolve 按令牌反查（room 取 roomFor 闭包，模拟接入层按 URL 频道组好的结果），
// sink 返回假 Publisher。
func testGateway(udpPort string, pub *fakePublisher, roomFor func(token string) string) *Gateway {
	cfg := func(_ context.Context, name string) string {
		switch name {
		case "bellows_udp_port":
			return udpPort
		case "bellows_public_ip":
			return "127.0.0.1" // 避免外网探测
		}
		return ""
	}
	resolve := func(_ context.Context, token string) (string, string, rtc.Meta, error) {
		switch token {
		case "good":
			return roomFor(token), rtc.Identity(7, "obs"),
				rtc.Meta{UID: 7, Username: "alice", Kind: "ingest", Tag: "obs"}, nil
		case "boom":
			return "", "", rtc.Meta{}, errors.New("db down") // 模拟瞬时故障
		}
		return "", "", rtc.Meta{}, ErrUnknownKey
	}
	return New(cfg, resolve, func(context.Context) rtc.Publisher { return pub }, nil)
}

// post 向 /w/{channel}/{token} 发 WHIP POST。
func post(g *Gateway, channel, token, offer string) (int, string) {
	req := httptest.NewRequest("POST", "/w/"+channel+"/"+token, strings.NewReader(offer))
	rec := httptest.NewRecorder()
	g.ServeWHIP(rec, req, token)
	return rec.Code, strings.TrimPrefix(rec.Header().Get("Location"), "/w/sessions/")
}

// videoCodec 可替换的 WHIP 风格 offer（audio/opus + video 单编码，sendonly）。
// 字面量里用 \n 书写，返回前统一转 CRLF（SDP 规范行结束符）。
func testOffer(videoCodec string) string {
	return strings.ReplaceAll(testOfferHead+videoCodec+"\n", "\n", "\r\n")
}

const testOfferHead = `v=0
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
a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00
a=setup:actpass
a=candidate:1 1 UDP 2130706431 127.0.0.1 9 typ host
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:1
a=sendonly
a=rtcp-mux
a=ice-ufrag:test
a=ice-pwd:testtesttesttesttesttest
a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00
a=setup:actpass
a=candidate:1 1 UDP 2130706431 127.0.0.1 9 typ host
`

func TestWHIPHandshakeH264(t *testing.T) {
	g := testGateway("47719", &fakePublisher{}, func(string) string { return "chan1" })
	offer := testOffer("a=rtpmap:96 H264/90000\na=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f\na=rtcp-fb:96 nack pli")
	req := httptest.NewRequest("POST", "/w/chan1/good", strings.NewReader(offer))
	req.Header.Set("Content-Type", "application/sdp")
	rec := httptest.NewRecorder()
	g.ServeWHIP(rec, req, "good")

	if rec.Code != 201 {
		t.Fatalf("期望 201，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/sdp" {
		t.Fatalf("Content-Type 应为 application/sdp，实际 %q", ct)
	}
	loc := rec.Header().Get("Location")
	// 资源地址必须是不透明会话 id：bearer 模式的令牌不能经 Location 泄进 URL/日志
	if !strings.HasPrefix(loc, "/w/sessions/") || strings.Contains(loc, "good") {
		t.Fatalf("Location 应为 /w/sessions/{会话id} 且不含推流令牌，实际 %q", loc)
	}
	rid := strings.TrimPrefix(loc, "/w/sessions/")
	if !g.HasSession(rid) {
		t.Fatalf("HasSession(%q) 应为 true", rid)
	}
	cl, err := strconv.Atoi(rec.Header().Get("Content-Length"))
	if err != nil || cl != rec.Body.Len() {
		t.Fatalf("Content-Length 必须显式设置且与 body 一致（ffmpeg WHIP muxer 读不了 chunked）: header=%q body=%d",
			rec.Header().Get("Content-Length"), rec.Body.Len())
	}
	sdp := rec.Body.String()
	if !strings.Contains(sdp, "H264") {
		t.Fatal("answer 应协商出 H264")
	}
	if !strings.Contains(sdp, "a=ice-lite") {
		t.Fatal("answer 应带 ice-lite 标记")
	}

	// DELETE 资源地址清理会话 → 204，且幂等
	for i := 0; i < 2; i++ {
		dreq := httptest.NewRequest("DELETE", loc, nil)
		drec := httptest.NewRecorder()
		g.ServeWHIP(drec, dreq, rid)
		if drec.Code != 204 {
			t.Fatalf("DELETE 期望 204，实际 %d", drec.Code)
		}
	}
	if g.HasSession(rid) {
		t.Fatal("DELETE 后会话应已移除")
	}
}

// 同令牌换房间：新 POST（无论目标房间）顶掉旧会话——一台设备同时只推一个房间。
// 曾因 close 持锁重入 g.mu 死锁（含超时保护防复发）。
func TestWHIPRepushDisplaces(t *testing.T) {
	room := "chan1"
	g := testGateway("47721", &fakePublisher{}, func(string) string { return room })
	offer := testOffer("a=rtpmap:96 H264/90000\na=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f")

	_, rid1 := post(g, "chan1", "good", offer)
	room = "chan2" // 同一把令牌改推另一个房间
	done := make(chan struct{})
	var code2 int
	var rid2 string
	go func() { code2, rid2 = post(g, "chan2", "good", offer); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("同令牌重推卡死（疑似 close 重入 g.mu 死锁回归）")
	}
	if code2 != 201 {
		t.Fatalf("重推期望 201，实际 %d", code2)
	}
	if g.HasSession(rid1) {
		t.Fatal("旧会话应已被顶掉")
	}
	if !g.HasSession(rid2) {
		t.Fatal("新会话应在册")
	}
}

func TestWHIPUnknownToken(t *testing.T) {
	g := testGateway("47719", &fakePublisher{}, func(string) string { return "chan1" })
	code, _ := post(g, "chan1", "bad", testOffer("a=rtpmap:96 H264/90000"))
	if code != 404 {
		t.Fatalf("未知令牌期望 404，实际 %d", code)
	}
	// 反查瞬时故障 ≠ 令牌无效：503 而非 404，避免误导用户重置令牌
	code, _ = post(g, "chan1", "boom", testOffer("a=rtpmap:96 H264/90000"))
	if code != 503 {
		t.Fatalf("反查故障期望 503，实际 %d", code)
	}
}

func TestWHIPUnsupportedCodec(t *testing.T) {
	g := testGateway("47720", &fakePublisher{}, func(string) string { return "chan1" })
	// 视频只有 VP8：不在白名单（h264/h265/av1），应拒绝
	req := httptest.NewRequest("POST", "/w/chan1/good", strings.NewReader(testOffer("a=rtpmap:96 VP8/90000")))
	rec := httptest.NewRecorder()
	g.ServeWHIP(rec, req, "good")
	if rec.Code != 400 {
		t.Fatalf("非白名单编码期望 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWHIPPatchNoop(t *testing.T) {
	g := testGateway("47719", &fakePublisher{}, func(string) string { return "chan1" })
	req := httptest.NewRequest("PATCH", "/w/sessions/rid1", strings.NewReader("a=ice-ufrag:test"))
	rec := httptest.NewRecorder()
	g.ServeWHIP(rec, req, "rid1")
	if rec.Code != 204 {
		t.Fatalf("PATCH 期望 204，实际 %d", rec.Code)
	}
}

// Publisher 透传：handleTrack 把 room/identity/name/meta 原样交给出口（meta 至少含
// username、kind=ingest、tag）；假 Publisher 的 unpublish 在会话关闭时被调用。
func TestPublisherPassthrough(t *testing.T) {
	pub := &fakePublisher{}
	g := testGateway("47725", pub, func(string) string { return "chan1" })
	code, rid := post(g, "chan1", "good", testOffer("a=rtpmap:96 H264/90000"))
	if code != 201 {
		t.Fatalf("建会话期望 201，实际 %d", code)
	}
	g.mu.Lock()
	s := g.sessions[rid]
	g.mu.Unlock()
	if s == nil {
		t.Fatal("会话应在册")
	}

	s.handleTrack(nil) // tr 仅透传给 Publisher，假实现不解引用
	got := pub.last()
	if got.room != "chan1" || got.identity != "u7-obs" || got.name != "alice" {
		t.Fatalf("透传身份不符: %+v", got)
	}
	if got.meta.UID != 7 || got.meta.Username != "alice" || got.meta.Kind != "ingest" || got.meta.Tag != "obs" {
		t.Fatalf("meta 不符: %+v", got.meta)
	}
}

// RevokeToken 幂等：掐断该令牌名下会话；无会话也返回 nil。
func TestRevokeToken(t *testing.T) {
	g := testGateway("47726", &fakePublisher{}, func(string) string { return "chan1" })
	code, rid := post(g, "chan1", "good", testOffer("a=rtpmap:96 H264/90000"))
	if code != 201 {
		t.Fatalf("建会话期望 201，实际 %d", code)
	}
	for i := 0; i < 2; i++ {
		if err := g.RevokeToken(context.Background(), "good"); err != nil {
			t.Fatalf("RevokeToken 应幂等返回 nil: %v", err)
		}
	}
	if g.HasSession(rid) {
		t.Fatal("RevokeToken 后会话应已掐断")
	}
}

// 进程内形态 Enabled 取决于发布出口：sink 取得到 Publisher 才可用（不再查 livekit_*）。
func TestEnabledBySink(t *testing.T) {
	cfg := func(_ context.Context, name string) string {
		if name == "bellows_public_ip" {
			return "127.0.0.1"
		}
		return ""
	}
	ctx := context.Background()
	if New(cfg, nil, nil, nil).Enabled(ctx) {
		t.Fatal("无 sink 应 Enabled=false")
	}
	if New(cfg, nil, func(context.Context) rtc.Publisher { return nil }, nil).Enabled(ctx) {
		t.Fatal("sink 取不到 Publisher 应 Enabled=false")
	}
	if !New(cfg, nil, func(context.Context) rtc.Publisher { return &fakePublisher{} }, nil).Enabled(ctx) {
		t.Fatal("sink 可用应 Enabled=true")
	}
}

// 发布出口断开必须拆掉推流会话：已建立的会话不会再产生新轨去触发重连，
// 光靠 Publisher 侧自愈的话轨会一直写进死连接（OBS 显示正常、观众永久黑屏）。
func TestPublishLostClosesSession(t *testing.T) {
	pub := &fakePublisher{}
	g := testGateway("47727", pub, func(string) string { return "chan1" })
	code, rid := post(g, "chan1", "good", testOffer("a=rtpmap:96 H264/90000"))
	if code != 201 {
		t.Fatalf("建会话期望 201，实际 %d", code)
	}
	g.mu.Lock()
	s := g.sessions[rid]
	g.mu.Unlock()
	if s == nil {
		t.Fatal("会话应在册")
	}
	s.handleTrack(nil) // 触发一次发布，Publisher 借此拿到回执
	lost := pub.lostFn()
	if lost == nil {
		t.Fatal("bellows 必须经 ctx 给出发布中断回执")
	}
	lost() // 模拟舞台内核断线
	if g.HasSession(rid) {
		t.Fatal("发布出口断开后会话应被拆掉")
	}
}
