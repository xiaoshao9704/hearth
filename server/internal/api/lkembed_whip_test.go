// lkembed 推流面的端到端测试：起真实的进程内 LiveKit，用 pion 写的 WHIP 客户端经
// hearth 的 /providers/lkembed/w 推流，lksdk 订阅端验证轨与参与者元数据，
// 再验换频道重推顶替、DELETE 收尾与被禁言用户 403。
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"hearth/server/internal/lktoken"
	"hearth/server/internal/rtc"
)

// stageAPI 造一个选中 lkembed 舞台线的 API，并把进程内 LiveKit 真的拉起来。
// 返回 API、hearth 的 httptest 服务地址与进程内 LiveKit 的回环端口。
func stageAPI(t *testing.T) (*API, string, int) {
	t.Helper()
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	httpPort, udpPort := freeTestPort(t, "tcp"), freeTestPort(t, "udp")
	for k, v := range map[string]string{
		"cfg_lkembed_port":     strconv.Itoa(httpPort),
		"cfg_lkembed_udp_port": strconv.Itoa(udpPort),
		"cfg_stage_provider":   AliasLkembed,
	} {
		if err := a.st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("写配置 %s 失败: %v", k, err)
		}
	}
	a.EnsureStageKernel(ctx)
	t.Cleanup(a.StopStageKernel)
	if !a.stageKernelRunning(ctx) {
		t.Fatal("进程内 LiveKit 未启动")
	}
	r := a.Router()
	a.RegisterProxies(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return a, srv.URL, httpPort
}

// whipTestClient 一个最小 WHIP 推流端：H264 + opus 两条轨，POST 拿 answer 后持续写假样本
//（SFU 与订阅端都不解码，视频每帧都按 IDR 发，保证有关键帧可转发）。
type whipTestClient struct {
	pc       *webrtc.PeerConnection
	code     int
	location string
	link     []string
	body     string
	stop     chan struct{}
}

func (c *whipTestClient) close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	if c.pc != nil {
		c.pc.Close()
	}
}

func whipPush(t *testing.T, base, path string) *whipTestClient {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("注册 opus: %v", err)
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("注册 h264: %v", err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("建 PC: %v", err)
	}
	c := &whipTestClient{pc: pc, stop: make(chan struct{})}
	audio, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "whip")
	if err != nil {
		t.Fatalf("建音轨: %v", err)
	}
	video, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "whip")
	if err != nil {
		t.Fatalf("建视轨: %v", err)
	}
	if _, err := pc.AddTrack(audio); err != nil {
		t.Fatalf("加音轨: %v", err)
	}
	if _, err := pc.AddTrack(video); err != nil {
		t.Fatalf("加视轨: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gather

	req, err := http.NewRequest("POST", base+path, strings.NewReader(pc.LocalDescription().SDP))
	if err != nil {
		t.Fatalf("建请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("WHIP POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	c.code, c.location, c.link, c.body = resp.StatusCode, resp.Header.Get("Location"), resp.Header.Values("Link"), string(body)
	if c.code != http.StatusCreated {
		return c
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: c.body}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		n := 0
		for {
			select {
			case <-c.stop:
				return
			case <-tick.C:
				audio.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe, 1, 2, 3, 4}, Duration: 20 * time.Millisecond})
				if n++; n%2 == 0 {
					video.WriteSample(media.Sample{
						Data:     append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, make([]byte, 64)...),
						Duration: 40 * time.Millisecond,
					})
				}
			}
		}
	}()
	return c
}

// whipSubscriber 用 lksdk 直连进程内 LiveKit 订阅房间，统计收到的 RTP 与推流参与者元数据。
type whipSubscriber struct {
	room     *lksdk.Room
	audio    atomic.Int64
	video    atomic.Int64
	seenMeta atomic.Value // [identity, 元数据原文]
}

func subscribeStage(t *testing.T, a *API, lkPort int, channel string) *whipSubscriber {
	t.Helper()
	ctx := context.Background()
	key, secret := a.dynVal(ctx, "lkembed_api_key"), a.dynVal(ctx, "lkembed_api_secret")
	if key == "" || secret == "" {
		t.Fatal("进程内 LiveKit 密钥未生成")
	}
	s := &whipSubscriber{}
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		s.seenMeta.Store([2]string{rp.Identity(), rp.Metadata()})
		go func() {
			for {
				if _, _, err := track.ReadRTP(); err != nil {
					return
				}
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					s.audio.Add(1)
				} else {
					s.video.Add(1)
				}
			}
		}()
	}
	tok, err := lktoken.Sign(key, secret, channel, rtc.Meta{UID: 9999, Username: "viewer", Tag: "web"}, false)
	if err != nil {
		t.Fatalf("签订阅令牌: %v", err)
	}
	room, err := lksdk.ConnectToRoomWithToken(fmt.Sprintf("ws://127.0.0.1:%d", lkPort), tok, cb)
	if err != nil {
		t.Fatalf("订阅端连接失败: %v", err)
	}
	s.room = room
	t.Cleanup(room.Disconnect)
	return s
}

// stageParticipants 经舞台实例列参与者（走的就是管理界面用的那条路）。
func stageParticipants(t *testing.T, a *API, channel string) []rtc.Participant {
	t.Helper()
	_, sp := a.stageInstance(context.Background())
	if sp == nil {
		t.Fatal("舞台实例为空")
	}
	ps, err := sp.ListParticipants(context.Background(), channel)
	if err != nil {
		t.Fatalf("列参与者失败: %v", err)
	}
	return ps
}

// waitParticipant 等某 identity 在房间里出现/消失。
func waitParticipant(t *testing.T, a *API, channel, identity string, want bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, p := range stageParticipants(t, a, channel) {
			if p.Identity == identity {
				found = true
			}
		}
		if found == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s 在 %s %s超时", identity, channel, map[bool]string{true: "出现", false: "消失"}[want])
}

var whipLocRe = regexp.MustCompile(`^/providers/lkembed/w/sessions/[0-9a-f]{32}$`)

// 端到端：推流 201 + Location 改写、订阅端收到轨且元数据 kind=ingest、
// 同令牌换频道重推顶掉旧房间的参与者、DELETE 后参与者立刻消失。
func TestLkembedWhipEndToEnd(t *testing.T) {
	a, base, lkPort := stageAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "alice", "chan1")
	if _, err := a.st.CreateChannel(ctx, "chan2", u.ID); err != nil {
		t.Fatalf("建第二个频道失败: %v", err)
	}
	identity := rtc.Identity(u.ID, "obs")

	sub := subscribeStage(t, a, lkPort, "chan1")

	// bellows 是已退场的历史 alias（实例不存在）：definitive 404
	resp0, err := http.Post(base+"/providers/bellows/w/chan1/"+it.Token, "application/sdp", strings.NewReader("sdp"))
	if err != nil {
		t.Fatalf("POST bellows: %v", err)
	}
	resp0.Body.Close()
	if resp0.StatusCode != http.StatusNotFound {
		t.Fatalf("/providers/bellows/w 应 404（非当前舞台实例），实际 %d", resp0.StatusCode)
	}

	c := whipPush(t, base, "/providers/lkembed/w/chan1/"+it.Token)
	defer c.close()
	if c.code != http.StatusCreated {
		t.Fatalf("推流应 201，实际 %d: %s", c.code, c.body)
	}
	if !whipLocRe.MatchString(c.location) {
		t.Fatalf("Location 应为 /providers/lkembed/w/sessions/{会话id}，实际 %q", c.location)
	}
	if strings.Contains(c.location, it.Token) {
		t.Fatalf("Location 不得含推流令牌: %q", c.location)
	}
	if len(c.link) != 0 {
		t.Fatalf("上游的 ice-server Link 不应透传给推流端: %v", c.link)
	}
	if !strings.Contains(c.body, "m=video") || !strings.Contains(c.body, "H264") {
		t.Fatalf("answer 应保留 H264 视频 m-line: %s", c.body)
	}

	// 订阅端收到音视频两条轨，元数据是 hearth 组的 rtc.Meta（kind=ingest）
	deadline := time.Now().Add(20 * time.Second)
	for sub.audio.Load() < 5 || sub.video.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("订阅端 20s 内没收够 RTP（音 %d 视 %d）", sub.audio.Load(), sub.video.Load())
		}
		time.Sleep(100 * time.Millisecond)
	}
	seen, _ := sub.seenMeta.Load().([2]string)
	if seen[0] != identity {
		t.Fatalf("推流参与者 identity 应为 %s，实际 %q", identity, seen[0])
	}
	if !strings.Contains(seen[1], `"kind":"ingest"`) ||
		!strings.Contains(seen[1], `"uid":`+strconv.FormatInt(u.ID, 10)) {
		t.Fatalf("参与者元数据应带 kind=ingest 与 uid，实际 %q", seen[1])
	}
	// 管理面看到的也是同一套（uid/username/kind 由元数据透传）
	var found bool
	for _, p := range stageParticipants(t, a, "chan1") {
		if p.Identity == identity {
			found = true
			if p.UID != u.ID || p.Username != "alice" || p.Kind != "ingest" || p.Tag != "obs" {
				t.Fatalf("参与者信息不符: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("chan1 里应有 %s", identity)
	}

	// 同令牌换频道重推：旧频道的同一发布身份被移出（推流令牌 = 一台设备只推一个房间）
	c2 := whipPush(t, base, "/providers/lkembed/w/chan2/"+it.Token)
	defer c2.close()
	if c2.code != http.StatusCreated {
		t.Fatalf("换频道重推应 201，实际 %d: %s", c2.code, c2.body)
	}
	waitParticipant(t, a, "chan2", identity, true)
	waitParticipant(t, a, "chan1", identity, false)

	// DELETE 收尾：会话按归属路由回内建实例，参与者立刻消失
	req, _ := http.NewRequest("DELETE", base+c2.location, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE 应 204，实际 %d", resp.StatusCode)
	}
	waitParticipant(t, a, "chan2", identity, false)
}

// 禁言在推流中途生效：推流设备被移出房间（收走 CanPublish 会让它永久黑屏，见 lkroom）。
// 解禁对推流设备无事可做，此后重推才恢复——这里只验解禁不报错、也不把它拉回来。
func TestLkembedWhipMuteRemoves(t *testing.T) {
	a, base, _ := stageAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "carol", "chan1")
	identity := rtc.Identity(u.ID, "obs")

	c := whipPush(t, base, "/providers/lkembed/w/chan1/"+it.Token)
	defer c.close()
	if c.code != http.StatusCreated {
		t.Fatalf("推流应 201，实际 %d: %s", c.code, c.body)
	}
	waitParticipant(t, a, "chan1", identity, true)

	_, sp := a.stageInstance(ctx)
	if err := sp.MuteUserAudio(ctx, "chan1", u.ID, true); err != nil {
		t.Fatalf("禁言失败: %v", err)
	}
	waitParticipant(t, a, "chan1", identity, false)
	// 房里已没有该用户的任何参与者，解禁按契约返回 ErrNoParticipant
	if err := sp.MuteUserAudio(ctx, "chan1", u.ID, false); !errors.Is(err, rtc.ErrNoParticipant) {
		t.Fatalf("解禁应返回 ErrNoParticipant，实际 %v", err)
	}
}

// 被禁言用户推流：admitIngest 直接 403，不打上游。
func TestLkembedWhipGagged(t *testing.T) {
	a, base, _ := stageAPI(t)
	ctx := context.Background()
	u, it := seedIngestUser(t, a, "bob", "chan1")
	c, err := a.st.ChannelByName(ctx, "chan1")
	if err != nil {
		t.Fatalf("取频道失败: %v", err)
	}
	if err := a.st.Gag(ctx, c.ID, u.ID); err != nil {
		t.Fatalf("禁言失败: %v", err)
	}
	cl := whipPush(t, base, "/providers/lkembed/w/chan1/"+it.Token)
	defer cl.close()
	if cl.code != http.StatusForbidden {
		t.Fatalf("被禁言用户推流应 403，实际 %d: %s", cl.code, cl.body)
	}
}

// 注册表：lkembed 同时具备 stage 与 ingest 能力，舞台选择器能选中它；
// 推流无独立选择器，推流入口跟随舞台实例（见 TestSelectorResolutionAndFallback）。
func TestLkembedCapabilities(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	inst := a.instance(AliasLkembed)
	if inst == nil || inst.Stage == nil || inst.Ingest == nil {
		t.Fatalf("lkembed 应同时具备 stage 与 ingest 能力: %+v", inst)
	}
	if msg := a.checkSelector(ctx, "stage_provider", AliasLkembed); msg != "" {
		t.Fatalf("stage_provider=lkembed 应合法: %s", msg)
	}
	// 舞台线没选中 lkembed 时进程内 LiveKit 没起，推流入口按不可用回显
	if inst.Ingest.Enabled(ctx) {
		t.Fatal("舞台线未选中 lkembed 时推流入口应为不可用")
	}
	// 推流入口跟随舞台实例：选中 lkembed 即解析到它
	a.st.SetSetting(ctx, "cfg_stage_provider", AliasLkembed)
	if alias, ip := a.ingestInstance(ctx); alias != AliasLkembed || ip == nil {
		t.Fatalf("推流入口应跟随舞台实例 lkembed，实际 %q", alias)
	}
}

// 未知会话资源 id：PATCH 404、DELETE 幂等 204。
func TestLkembedWhipUnknownSession(t *testing.T) {
	a, base, _ := stageAPI(t)
	_ = a
	for method, want := range map[string]int{
		http.MethodPatch:  http.StatusNotFound,
		http.MethodDelete: http.StatusNoContent,
	} {
		req, _ := http.NewRequest(method, base+"/providers/lkembed/w/sessions/"+strings.Repeat("0", 32), strings.NewReader(""))
		req.Header.Set("Content-Type", "application/trickle-ice-sdpfrag")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s 未知会话应 %d，实际 %d", method, want, resp.StatusCode)
		}
	}
}

// freeTestPort 借一个空闲端口号（绑上再放掉），避免测试之间撞固定端口。
func freeTestPort(t *testing.T, proto string) int {
	t.Helper()
	if proto == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("借 udp 端口: %v", err)
		}
		defer pc.Close()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("借 tcp 端口: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
