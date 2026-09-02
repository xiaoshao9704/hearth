// WHIP 信令握手测试：h264 offer → 201 + answer SDP + Content-Length；
// 未知密钥 404；反查故障 503；非白名单编码 400；DELETE → 204。
package bellows

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testGateway(udpPort string) *Gateway {
	cfg := func(_ context.Context, name string) string {
		switch name {
		case "bellows_udp_port":
			return udpPort
		case "bellows_public_ip":
			return "127.0.0.1" // 避免外网探测
		}
		return ""
	}
	resolve := func(_ context.Context, key string) (string, string, error) {
		switch key {
		case "good":
			return "chan1", "alice", nil
		case "boom":
			return "", "", errors.New("db down") // 模拟瞬时故障
		}
		return "", "", ErrUnknownKey
	}
	return New(cfg, resolve)
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
a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00
a=setup:actpass
a=candidate:1 1 UDP 2130706431 127.0.0.1 9 typ host
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:1
a=sendonly
a=rtcp-mux
a=ice-ufrag:test
a=ice-pwd:testtesttesttesttesttest
a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00
a=setup:actpass
a=candidate:1 1 UDP 2130706431 127.0.0.1 9 typ host
`

func TestWHIPHandshakeH264(t *testing.T) {
	g := testGateway("47719")
	offer := testOffer("a=rtpmap:96 H264/90000\na=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f\na=rtcp-fb:96 nack pli")
	req := httptest.NewRequest("POST", "/w/good", strings.NewReader(offer))
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
	// 资源地址必须是不透明会话 id 的相对形式：bearer 模式的密钥不能经 Location 泄进 URL/日志
	if strings.Contains(loc, "/") || strings.Contains(loc, "good") {
		t.Fatalf("Location 应为相对会话 id 且不含推流密钥，实际 %q", loc)
	}
	rid := strings.TrimPrefix(loc, "/w/")
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

	// DELETE 资源地址清理会话 → 204，且幂等（Location 是相对 rid，客户端按请求路径解析后回发）
	for i := 0; i < 2; i++ {
		dreq := httptest.NewRequest("DELETE", "/w/"+rid, nil)
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

// 同 key 重推顶替旧会话：曾因 close 持锁重入 g.mu 死锁（含超时保护防复发）。
func TestWHIPRepushDisplaces(t *testing.T) {
	g := testGateway("47721")
	post := func() (int, string) {
		offer := testOffer("a=rtpmap:96 H264/90000\na=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f")
		req := httptest.NewRequest("POST", "/w/good", strings.NewReader(offer))
		rec := httptest.NewRecorder()
		g.ServeWHIP(rec, req, "good")
		return rec.Code, strings.TrimPrefix(rec.Header().Get("Location"), "/w/")
	}
	_, rid1 := post()
	done := make(chan struct{})
	var code2 int
	var rid2 string
	go func() { code2, rid2 = post(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("同 key 重推卡死（疑似 close 重入 g.mu 死锁回归）")
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

func TestWHIPUnknownKey(t *testing.T) {
	g := testGateway("47719")
	req := httptest.NewRequest("POST", "/w/bad", strings.NewReader(testOffer("a=rtpmap:96 H264/90000")))
	rec := httptest.NewRecorder()
	g.ServeWHIP(rec, req, "bad")
	if rec.Code != 404 {
		t.Fatalf("未知密钥期望 404，实际 %d", rec.Code)
	}
	// 反查瞬时故障 ≠ 密钥无效：503 而非 404，避免误导用户重置密钥
	req = httptest.NewRequest("POST", "/w/boom", strings.NewReader(testOffer("a=rtpmap:96 H264/90000")))
	rec = httptest.NewRecorder()
	g.ServeWHIP(rec, req, "boom")
	if rec.Code != 503 {
		t.Fatalf("反查故障期望 503，实际 %d", rec.Code)
	}
}

func TestWHIPUnsupportedCodec(t *testing.T) {
	g := testGateway("47720")
	// 视频只有 VP8：不在白名单（h264/h265/av1），应拒绝
	req := httptest.NewRequest("POST", "/w/good", strings.NewReader(testOffer("a=rtpmap:96 VP8/90000")))
	rec := httptest.NewRecorder()
	g.ServeWHIP(rec, req, "good")
	if rec.Code != 400 {
		t.Fatalf("非白名单编码期望 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWHIPPatchNoop(t *testing.T) {
	g := testGateway("47719")
	req := httptest.NewRequest("PATCH", "/w/good", strings.NewReader("a=ice-ufrag:test"))
	rec := httptest.NewRecorder()
	g.ServeWHIP(rec, req, "good")
	if rec.Code != 204 {
		t.Fatalf("PATCH 期望 204，实际 %d", rec.Code)
	}
}
