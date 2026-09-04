package lite

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// 两个 m= 段的样例：第二段无候选（BUNDLE 下 pion 就是这样，见 TestBundleCandidatesOnlyInFirstSection）。
const sampleSDP = "v=0\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"a=mid:0\r\n" +
	"a=candidate:1487732461 1 udp 2130706431 fd08::1 51973 typ host\r\n" +
	"a=candidate:2889737919 1 udp 2130706431 192.168.50.4 51973 typ host\r\n" +
	"a=candidate:2889737919 2 udp 2130706431 192.168.50.4 51973 typ host\r\n" +
	"a=end-of-candidates\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
	"a=mid:1\r\n"

func srflxLines(t *testing.T, sdp string) []ice.Candidate {
	t.Helper()
	var out []ice.Candidate
	for _, l := range strings.Split(sdp, "\r\n") {
		raw, ok := strings.CutPrefix(l, "a=")
		if !ok || !strings.Contains(l, "typ srflx") {
			continue
		}
		c, err := ice.UnmarshalCandidate(raw)
		if err != nil {
			t.Fatalf("追加的候选 %q 解析失败: %v", l, err)
		}
		if c.Type() != ice.CandidateTypeServerReflexive {
			t.Fatalf("候选类型应为 srflx，实际 %v", c.Type())
		}
		out = append(out, c)
	}
	return out
}

func TestAppendMappedCandidate(t *testing.T) {
	ext := netip.MustParseAddrPort("203.0.113.5:33445")
	local := netip.MustParseAddrPort("192.168.50.4:51973")
	got := AppendMappedCandidate(sampleSDP, ext, local)

	cands := srflxLines(t, got)
	if len(cands) != 1 {
		t.Fatalf("应只追加 1 条候选，实际 %d 条:\n%s", len(cands), got)
	}
	c := cands[0]
	if c.Address() != "203.0.113.5" || c.Port() != 33445 {
		t.Fatalf("外部地址应为 %v，实际 %s:%d", ext, c.Address(), c.Port())
	}
	if r := c.RelatedAddress(); r == nil || r.Address != "192.168.50.4" || r.Port != 51973 {
		t.Fatalf("raddr/rport 应为 %v，实际 %+v", local, r)
	}
	// 插在最后一行候选之后、end-of-candidates 之前，且不碰无候选的 m= 段
	lines := strings.Split(got, "\r\n")
	at, eoc := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "typ srflx") {
			at = i
		}
		if l == "a=end-of-candidates" {
			eoc = i
		}
	}
	if at < 0 || eoc < 0 || at != eoc-1 {
		t.Fatalf("srflx 应紧邻 end-of-candidates 之前:\n%s", got)
	}
	if strings.Count(got, "m=video 9") != 1 || strings.HasSuffix(strings.TrimRight(got, "\r\n"), "typ srflx") {
		t.Fatalf("无候选的 m= 段不应被改动:\n%s", got)
	}
}

func TestAppendMappedCandidateFoundationNoConflict(t *testing.T) {
	got := AppendMappedCandidate(sampleSDP,
		netip.MustParseAddrPort("203.0.113.5:33445"), netip.MustParseAddrPort("192.168.50.4:51973"))
	seen := map[string]bool{}
	for _, l := range strings.Split(got, "\r\n") {
		c, ok := parseCandidate(l)
		if !ok {
			continue
		}
		key := c.foundation + "/" + c.component
		if seen[key] {
			t.Fatalf("foundation %s 与已有候选冲突:\n%s", c.foundation, got)
		}
		seen[key] = true
	}
}

// 实测：BUNDLE 下 pion 只把候选放进第一个 m= 段；且候选要等 GatheringCompletePromise 之后
// 才出现在 LocalDescription 里（SetLocalDescription 返回时 SDP 里一条候选都没有）。
// Announce 的调用方因此都应在 gathering 完成后再取 LocalDescription。
func TestBundleCandidatesOnlyInFirstSection(t *testing.T) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("RegisterDefaultCodecs: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo} {
		if _, err := pc.AddTransceiverFromKind(kind); err != nil {
			t.Fatalf("AddTransceiver: %v", err)
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	if strings.Contains(pc.LocalDescription().SDP, "a=candidate:") {
		t.Fatal("候选提前出现了：本测试记录的前提是要等 gathering complete")
	}
	<-done

	sdp := pc.LocalDescription().SDP
	sections := strings.Split(sdp, "\r\nm=")
	if len(sections) != 3 { // 会话头 + audio + video
		t.Fatalf("应为两个 m= 段，实际 %d:\n%s", len(sections)-1, sdp)
	}
	if !strings.Contains(sections[1], "a=candidate:") {
		t.Fatalf("第一个 m= 段应有候选:\n%s", sdp)
	}
	if strings.Contains(sections[2], "a=candidate:") {
		t.Fatalf("BUNDLE 下第二个 m= 段不应有候选（结论变了要同步改 appendSrflx 注释）:\n%s", sdp)
	}

	// 追加到真实 SDP 上：只有第一段被改，且行能被解析成 srflx
	got := AppendMappedCandidate(sdp,
		netip.MustParseAddrPort("203.0.113.5:33445"), netip.MustParseAddrPort("127.0.0.1:1"))
	if n := len(srflxLines(t, got)); n != 1 {
		t.Fatalf("真实 SDP 上应只追加 1 条，实际 %d", n)
	}
}

// 回归：追加的 srflx 行必须不影响对端建连——pion 客户端拿到带追加候选的 answer 后，
// 那条候选不通（TEST-NET 地址）也要正常回落到 host 候选完成 ICE。
// SDP 出口插纯文本是绕开「pion 改写规则改不了端口」的手段，代价不能是把 SDP 弄坏。
func TestAppendedCandidateDoesNotBreakPeer(t *testing.T) {
	server, err := webrtc.NewAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("服务端 PC: %v", err)
	}
	defer server.Close()
	client, err := webrtc.NewAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("客户端 PC: %v", err)
	}
	defer client.Close()
	if _, err := client.CreateDataChannel("probe", nil); err != nil {
		t.Fatalf("DataChannel: %v", err)
	}
	connected := make(chan struct{})
	client.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatalf("客户端 offer: %v", err)
	}
	clientGather := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatalf("客户端 SetLocal: %v", err)
	}
	<-clientGather
	if err := server.SetRemoteDescription(*client.LocalDescription()); err != nil {
		t.Fatalf("服务端 SetRemote: %v", err)
	}
	answer, err := server.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("服务端 answer: %v", err)
	}
	serverGather := webrtc.GatheringCompletePromise(server)
	if err := server.SetLocalDescription(answer); err != nil {
		t.Fatalf("服务端 SetLocal: %v", err)
	}
	<-serverGather

	sdp := AppendMappedCandidate(server.LocalDescription().SDP,
		netip.MustParseAddrPort("203.0.113.5:33445"), netip.MustParseAddrPort("127.0.0.1:1"))
	if !strings.Contains(sdp, "typ srflx") {
		t.Fatalf("应已追加候选:\n%s", sdp)
	}
	if err := client.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		t.Fatalf("客户端 SetRemote（追加候选后的 answer）: %v", err)
	}

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("追加候选后 10s 内未完成 ICE 连接")
	}
}
