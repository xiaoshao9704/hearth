// WHIP 信令握手测试：h264 offer → 201 + answer SDP + Content-Length；
// 未知密钥 404；非白名单编码 400；DELETE → 204。
package pionwhip

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"hearth/server/internal/store"
)

func testGateway(udpPort string) *Gateway {
	cfg := func(_ context.Context, name string) string {
		switch name {
		case "pionwhip_udp_port":
			return udpPort
		case "pionwhip_public_ip":
			return "127.0.0.1" // 避免外网探测
		}
		return ""
	}
	resolve := func(_ context.Context, key string) (string, string, error) {
		if key == "good" {
			return "chan1", "alice", nil
		}
		return "", "", store.ErrNotFound
	}
	return New(cfg, resolve)
}

// videoCodec 可替换的 WHIP 风格 offer（audio/opus + video 单编码，sendonly）。
// 字面量里用 \n 书写，返回前统一转 CRLF（SDP 规范行结束符）。
func testOffer(videoCodec string) string {
	return strings.ReplaceAll(testOfferLF(videoCodec), "\n", "\r\n")
}

func testOfferLF(videoCodec string) string {
	return testOfferHead + videoCodec + "\n"
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
	if loc := rec.Header().Get("Location"); loc != "/w/good" {
		t.Fatalf("Location 应为 /w/good，实际 %q", loc)
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

	// DELETE 清理会话 → 204，且幂等
	for i := 0; i < 2; i++ {
		dreq := httptest.NewRequest("DELETE", "/w/good", nil)
		drec := httptest.NewRecorder()
		g.ServeWHIP(drec, dreq, "good")
		if drec.Code != 204 {
			t.Fatalf("DELETE 期望 204，实际 %d", drec.Code)
		}
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
