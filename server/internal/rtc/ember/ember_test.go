package ember

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"hearth/server/internal/rtc"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// 回归测试覆盖的 bug：同一 identity 重连（切 tab/刷新）时，旧连接的收尾（HandleJoin 阻塞
// 结束后那一段）会在新连接正在做初始 WebRTC 握手时，把重协商 offer 塞进同一个 pc，
// 撞坏 signaling state，导致新连接握手失败 → 客户端重连 → 又被上一代收尾撞 → 自持循环
//（生产上表现为同一 identity 每 ~15s 重入会一次、持续数分钟）。

// ---- 测试用 httptest 信令服务器 ----

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatalf("取空闲 UDP 端口失败: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	port := freeUDPPort(t)
	cfg := func(_ context.Context, name string) string {
		switch name {
		case "ember_udp_port":
			return strconv.Itoa(port)
		case "ember_public_ip", "ember_stun_servers":
			return ""
		}
		return ""
	}
	return New(cfg, nil)
}

// newTestServer 起一个 /voice 信令端点，直接照 internal/api/voice.go 的接线方式调用 HandleJoin。
// meta 从 query 取（uid/tag/username），测试用假名，不出现真实用户名/设备标签。
func newTestServer(t *testing.T, p *Provider, room string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/voice", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		uid, _ := strconv.ParseInt(q.Get("uid"), 10, 64)
		meta := rtc.Meta{UID: uid, Username: q.Get("username"), Tag: q.Get("tag")}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		p.HandleJoin(context.WithoutCancel(r.Context()), room, meta, false, conn)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---- 测试用 pion 客户端 ----

type testClient struct {
	t    *testing.T
	name string // 仅测试日志用（不写真实身份信息）

	conn    *websocket.Conn
	writeMu sync.Mutex

	pc *webrtc.PeerConnection

	connectedOnce sync.Once
	connectedCh   chan struct{}

	answerOnce sync.Once
	answerCh   chan struct{}

	byeOnce   sync.Once
	byeCh     chan struct{}
	byeReason string

	closedOnce sync.Once
	closedCh   chan struct{}

	trackMu     sync.Mutex
	trackStream map[string]int // streamID(=identity) -> 当前非空 remote track 数

	stopWrite chan struct{} // 停止上行音频写入 goroutine
	closeOnce sync.Once
}

func dialTestClient(t *testing.T, wsURL string, uid int64, tag, name string) *testClient {
	t.Helper()
	u := fmt.Sprintf("%s?uid=%d&tag=%s&username=%s", wsURL, uid, tag, name)
	conn, _, err := websocket.Dial(context.Background(), u, nil)
	if err != nil {
		t.Fatalf("%s ws dial: %v", name, err)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("%s NewPeerConnection: %v", name, err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", name)
	if err != nil {
		t.Fatalf("%s NewTrackLocalStaticSample: %v", name, err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		t.Fatalf("%s AddTransceiverFromTrack: %v", name, err)
	}

	c := &testClient{
		t: t, name: name, conn: conn, pc: pc,
		connectedCh: make(chan struct{}), answerCh: make(chan struct{}), byeCh: make(chan struct{}),
		closedCh: make(chan struct{}), trackStream: make(map[string]int),
		stopWrite: make(chan struct{}),
	}

	// 持续写点音频样本：pion 的 OnTrack 是靠收到首个 RTP 包才触发的，光谈妥 SDP 不发包
	// 对端永远看不到轨——内容是什么不重要（不解码），只为了让 RTP 真的流起来。
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		payload := make([]byte, 160)
		for {
			select {
			case <-ticker.C:
				_ = track.WriteSample(media.Sample{Data: payload, Duration: 20 * time.Millisecond})
			case <-c.stopWrite:
				return
			}
		}
	}()

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			c.connectedOnce.Do(func() { close(c.connectedCh) })
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		c.trackMu.Lock()
		c.trackStream[tr.StreamID()]++
		c.trackMu.Unlock()
		buf := make([]byte, 1500)
		for {
			if _, _, err := tr.Read(buf); err != nil {
				return
			}
		}
	})

	go c.readLoop()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("%s CreateOffer: %v", name, err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("%s SetLocalDescription: %v", name, err)
	}
	waitGather(pc)
	if err := c.writeMsg(sigMsg{Type: "offer", SDP: pc.LocalDescription().SDP}); err != nil {
		t.Fatalf("%s send offer: %v", name, err)
	}
	return c
}

// waitGather 客户端向 ICE-Lite 服务端发 offer 不必等 gathering complete（部分环境永不完成），
// 最多等 1s。
func waitGather(pc *webrtc.PeerConnection) {
	done := webrtc.GatheringCompletePromise(pc)
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func (c *testClient) writeMsg(m sigMsg) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return wsjson.Write(ctx, c.conn, m)
}

func (c *testClient) readLoop() {
	for {
		var m sigMsg
		if err := wsjson.Read(context.Background(), c.conn, &m); err != nil {
			c.closedOnce.Do(func() { close(c.closedCh) })
			return
		}
		switch m.Type {
		case "answer":
			if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
				c.t.Logf("%s SetRemoteDescription(answer): %v", c.name, err)
				continue
			}
			c.answerOnce.Do(func() { close(c.answerCh) })
		case "offer": // 服务端重协商
			if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}); err != nil {
				c.t.Logf("%s SetRemoteDescription(offer): %v", c.name, err)
				continue
			}
			answer, err := c.pc.CreateAnswer(nil)
			if err != nil {
				continue
			}
			if err := c.pc.SetLocalDescription(answer); err != nil {
				continue
			}
			waitGather(c.pc)
			if err := c.writeMsg(sigMsg{Type: "answer", SDP: c.pc.LocalDescription().SDP}); err != nil {
				c.t.Logf("%s send answer: %v", c.name, err)
			}
		case "bye":
			c.byeOnce.Do(func() { c.byeReason = m.Reason; close(c.byeCh) })
		}
	}
}

func (c *testClient) close() {
	c.closeOnce.Do(func() {
		close(c.stopWrite)
		c.pc.Close()
		c.conn.Close(websocket.StatusNormalClosure, "")
	})
}

// remoteTrackCount 当前从某 identity 收到过的非空 remote track 累计次数（OnTrack 触发次数，
// 不是"当前存活"计数——用于用例里断言"只订阅了一次"）。
func (c *testClient) remoteTrackCount(identity string) int {
	c.trackMu.Lock()
	defer c.trackMu.Unlock()
	return c.trackStream[identity]
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return cond()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func wsURLFor(srv *httptest.Server) string {
	return "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/voice"
}

// ---- 用例 1：同 identity 重连不应触发自持重连循环 ----

func TestRejoinSameIdentityDoesNotLoop(t *testing.T) {
	p := newTestProvider(t)
	srv := newTestServer(t, p, "room-rejoin")
	wsURL := wsURLFor(srv)

	const rounds = 3
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			a1 := dialTestClient(t, wsURL, 1, "dev-a", "alice")
			select {
			case <-a1.connectedCh:
			case <-time.After(12 * time.Second):
				t.Fatalf("round%d: A1 未在 12s 内 connected", round)
			}

			// 同 identity 立即重入会：让第二次入会尽量与第一次的收尾窗口重叠
			// （旧连接 close() 内部有 150ms 送达延迟，是竞态的时间窗）。
			a2 := dialTestClient(t, wsURL, 1, "dev-a", "alice")
			defer a2.close()

			select {
			case <-a2.answerCh:
			case <-time.After(12 * time.Second):
				t.Fatalf("round%d: A2 未在 12s 内收到 answer", round)
			}
			select {
			case <-a2.connectedCh:
			case <-time.After(12 * time.Second):
				t.Fatalf("round%d: A2 的 PC 未在 12s 内 connected", round)
			}

			select {
			case <-a1.byeCh:
				if a1.byeReason != "duplicate" {
					t.Fatalf("round%d: A1 收到 bye 但 reason=%q，期望 duplicate", round, a1.byeReason)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("round%d: A1 未在 5s 内收到 bye", round)
			}
			a1.close()

			// 至少 5 秒内 A2 不应被服务端关闭、PC 不应进 failed/closed。
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-a2.closedCh:
					t.Fatalf("round%d: A2 的 ws 在稳定期内被关闭", round)
				default:
				}
				switch a2.pc.ConnectionState() {
				case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
					t.Fatalf("round%d: A2 的 PC 在稳定期内进入 %s", round, a2.pc.ConnectionState())
				}
				time.Sleep(50 * time.Millisecond)
			}

			a2.close()
			// 等房间清空再进入下一轮，避免残留状态互相干扰。
			if !waitFor(3*time.Second, func() bool {
				counts, _ := p.RoomCounts(context.Background())
				return counts["room-rejoin"] == 0
			}) {
				t.Fatalf("round%d: 房间未在 3s 内清空", round)
			}
		})
	}
}

// ---- 用例 2：重连后其他参与者的订阅保持唯一，不重复不残留 ----

func TestRejoinKeepsOthersSubscribed(t *testing.T) {
	p := newTestProvider(t)
	srv := newTestServer(t, p, "room-keep")
	wsURL := wsURLFor(srv)

	b := dialTestClient(t, wsURL, 2, "dev-b", "bob")
	defer b.close()
	select {
	case <-b.connectedCh:
	case <-time.After(12 * time.Second):
		t.Fatal("B 未在 12s 内 connected")
	}

	aIdentity := rtc.Identity(1, "dev-a")
	a1 := dialTestClient(t, wsURL, 1, "dev-a", "alice")
	select {
	case <-a1.connectedCh:
	case <-time.After(12 * time.Second):
		t.Fatal("A1 未在 12s 内 connected")
	}
	// 等 B 订阅到 A1 的下行轨，确认初始状态已经稳定。
	if !waitFor(5*time.Second, func() bool { return b.remoteTrackCount(aIdentity) >= 1 }) {
		t.Fatal("B 未在 5s 内收到 A1 的下行轨")
	}

	a2 := dialTestClient(t, wsURL, 1, "dev-a", "alice")
	defer a2.close()
	select {
	case <-a2.connectedCh:
	case <-time.After(12 * time.Second):
		t.Fatal("A2 未在 12s 内 connected")
	}
	select {
	case <-a1.byeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("A1 未在 5s 内收到 bye")
	}
	a1.close()

	// 等稳定：B 的 PC 上来自 A 这条 streamID 的当前非空 receiver 数量应该收敛到 1
	//（新 incarnation 的轨；GetReceivers 反映"此刻"状态，不是累计 OnTrack 次数）。
	if !waitFor(5*time.Second, func() bool { return currentTrackCount(b, aIdentity) == 1 }) {
		t.Fatalf("B 对 A 的当前订阅数未收敛到 1，实际=%d", currentTrackCount(b, aIdentity))
	}

	// 白盒校验：B 参与者 senders[A.identity].owner 必须是新 part（A2），不是已关闭的 A1。
	p.mu.Lock()
	room := p.rooms["room-keep"]
	p.mu.Unlock()
	if room == nil {
		t.Fatal("房间已消失")
	}
	room.mu.Lock()
	bPart := room.parts["u2-dev-b"]
	aPart := room.parts[aIdentity]
	room.mu.Unlock()
	if bPart == nil || aPart == nil {
		t.Fatalf("参与者缺失: bPart=%v aPart=%v", bPart, aPart)
	}
	bPart.sndMu.Lock()
	entry := bPart.senders[aIdentity]
	bPart.sndMu.Unlock()
	if entry == nil {
		t.Fatal("B 的 senders 里没有 A 的订阅")
	}
	if entry.owner != aPart {
		t.Fatal("B 的 senders[A.identity].owner 不是新 incarnation（A2）")
	}
}

// currentTrackCount 数 c 的 PC 上来自 fromIdentity（streamID）的、当前非空的 receiver 数——
// 用 GetReceivers 而不是累计 OnTrack 次数，反映"此刻"的订阅状态（旧 receiver 被换轨/摘除后
// Track() 会变 nil 或整个 receiver 被移除，不会计入）。
func currentTrackCount(c *testClient, fromIdentity string) int {
	n := 0
	for _, recv := range c.pc.GetReceivers() {
		if tr := recv.Track(); tr != nil && tr.StreamID() == fromIdentity {
			n++
		}
	}
	return n
}
