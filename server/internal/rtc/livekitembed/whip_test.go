package livekitembed

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"hearth/server/internal/lktoken"
	"hearth/server/internal/rtc"
)

// whipLatencyBudget 是「多网卡环境下 WHIP POST 应该多快拿到 201」的验收线，见
// docs 里对进程内 LiveKit 的 WHIP 时延排查记录：修复前实测稳定 13s（回落到内置
// google/twilio STUN、真机不可达，等 gathering 超时才应答），修复后本地在 1s 内。
const whipLatencyBudget = 2 * time.Second

// newWhipTestClient 建一个只用回环接口的最小 pion WHIP 发布端：只注册 opus，
// 避免真实网卡候选把测试拖进真实网络。
func newWhipTestClient(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("注册 opus: %v", err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetInterfaceFilter(func(name string) bool { return name == "lo0" || name == "lo" })
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "whiptest")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	return pc
}

// whipPublish 用 pc 生成 offer、POST 到进程内 LiveKit 的 WHIP 端点、把 answer 灌回 pc，
// 返回 HTTP 响应、answer SDP 正文与 POST 耗时。room 各自独立，identity 相同也不会互踢。
func whipPublish(t *testing.T, httpPort int, room string, pc *webrtc.PeerConnection) (*http.Response, string, time.Duration) {
	t.Helper()
	meta := rtc.Meta{UID: 1, Username: "whiptest", Tag: "obs"}
	tok, err := lktoken.Sign(testAPIKey, testAPISecret, room, meta, true)
	if err != nil {
		t.Fatalf("签发令牌: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gatherComplete

	url := fmt.Sprintf("http://127.0.0.1:%d/whip/v1", httpPort)
	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte(pc.LocalDescription().SDP)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+tok)

	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("WHIP POST 出错（耗时 %s）: %v", elapsed, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("WHIP POST 状态码=%d body=%s", resp.StatusCode, string(body))
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	return resp, string(body), elapsed
}

// waitICEConnected 轮询 pc 的 ICE 连接状态，用于确认 WHIP 发布端在拿到 answer 后
// 真的建连成功（而不只是 201 但连不上）。
func waitICEConnected(t *testing.T, pc *webrtc.PeerConnection, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		switch pc.ICEConnectionState() {
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 内 ICE 未进入 connected，当前状态 %s", timeout, pc.ICEConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sdpCandidateLines 取 answer SDP 里所有 a=candidate 行。
func sdpCandidateLines(sdp string) []string {
	var out []string
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "a=candidate") {
			out = append(out, line)
		}
	}
	return out
}

// TestWhipLatency 是 buildYAML 里 use_ice_lite: true 修复的验收：进程内 LiveKit 的
// WHIP 一次性信令曾经因为「stun_servers: [] 被上游回落成内置 google/twilio STUN，
// GetAnswer 等 gathering 完成时卡在不可达的 STUN 超时上」稳定卡 13s（见
// buildYAML 上方注释），本地环境应在 whipLatencyBudget 内拿到 201。
func TestWhipLatency(t *testing.T) {
	httpPort := freePort(t, "tcp")
	udpPort := freePort(t, "udp")

	srv, err := Start(context.Background(), Options{
		HTTPPort:  httpPort,
		UDPPort:   udpPort,
		APIKey:    testAPIKey,
		APISecret: testAPISecret,
		LogSink:   func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	pc := newWhipTestClient(t)
	defer pc.Close()

	_, _, elapsed := whipPublish(t, httpPort, "whip-latency", pc)
	t.Logf("WHIP POST -> 201 耗时 %s", elapsed)
	if elapsed > whipLatencyBudget {
		t.Fatalf("WHIP POST 耗时 %s，超过预算 %s", elapsed, whipLatencyBudget)
	}
}

// TestWhipExternalIPPerTransport 验证补丁二「每建一个 PeerConnection 取一次当前外部
// IPv4」的语义：ExternalIPs 回调在两次 WHIP POST 之间切换返回值，第一个 PC 的候选
// 必须固定在建连时取到的地址上，不因回调后续改变而被动改写；同时两次 answer 都要保留
// 本机 LAN host 候选（includeInternal=true 没有把 host 候选砍掉）。
func TestWhipExternalIPPerTransport(t *testing.T) {
	httpPort := freePort(t, "tcp")
	udpPort := freePort(t, "udp")

	const ip1 = "203.0.113.7" // TEST-NET-3，RFC 5737 文档保留地址
	const ip2 = "203.0.113.8"
	var current atomic.Value
	current.Store(ip1)

	srv, err := Start(context.Background(), Options{
		HTTPPort:    httpPort,
		UDPPort:     udpPort,
		APIKey:      testAPIKey,
		APISecret:   testAPISecret,
		ExternalIPs: func() []string { return []string{current.Load().(string)} },
		LogSink:     func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	pc1 := newWhipTestClient(t)
	defer pc1.Close()
	_, body1, _ := whipPublish(t, httpPort, "whip-ext-1", pc1)
	assertCandidatesContainIP(t, body1, ip1)
	assertCandidatesExcludeIP(t, body1, ip2)
	assertHasLANHostCandidate(t, body1, ip1)
	waitICEConnected(t, pc1, 10*time.Second)

	current.Store(ip2)

	pc2 := newWhipTestClient(t)
	defer pc2.Close()
	_, body2, _ := whipPublish(t, httpPort, "whip-ext-2", pc2)
	assertCandidatesContainIP(t, body2, ip2)
	assertCandidatesExcludeIP(t, body2, ip1)
	assertHasLANHostCandidate(t, body2, ip2)

	// 第一个 PC 在第二次 POST（回调返回值已切换）之后仍应保持 connected：补丁二的
	// 映射地址是建连时烙进那个 transport 自己的 SettingEngine 副本，不受后续调用影响。
	if s := pc1.ICEConnectionState(); s != webrtc.ICEConnectionStateConnected && s != webrtc.ICEConnectionStateCompleted {
		t.Fatalf("第二次 POST 后 pc1 应仍处于 connected，实际 %s", s)
	}
}

func assertCandidatesContainIP(t *testing.T, sdp, ip string) {
	t.Helper()
	for _, line := range sdpCandidateLines(sdp) {
		if strings.Contains(line, ip) {
			return
		}
	}
	t.Fatalf("answer 候选里没找到 %s:\n%s", ip, strings.Join(sdpCandidateLines(sdp), "\n"))
}

func assertCandidatesExcludeIP(t *testing.T, sdp, ip string) {
	t.Helper()
	for _, line := range sdpCandidateLines(sdp) {
		if strings.Contains(line, ip) {
			t.Fatalf("answer 候选不该包含 %s，实际出现:\n%s", ip, line)
		}
	}
}

// assertHasLANHostCandidate 确认候选里除了 externalIP 那条改写候选外，还留着一条
// 真实本机地址的 host 候选（没被映射规则整体替换掉）。
func assertHasLANHostCandidate(t *testing.T, sdp, externalIP string) {
	t.Helper()
	for _, line := range sdpCandidateLines(sdp) {
		if !strings.Contains(line, " typ host") {
			continue
		}
		if !strings.Contains(line, externalIP) {
			return
		}
	}
	t.Fatalf("answer 候选里没有非 %s 的本机 host 候选:\n%s", externalIP, strings.Join(sdpCandidateLines(sdp), "\n"))
}
