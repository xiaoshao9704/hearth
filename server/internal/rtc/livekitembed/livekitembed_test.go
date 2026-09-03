package livekitembed

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"hearth/server/internal/lktoken"
	"hearth/server/internal/rtc"
)

const (
	testAPIKey    = "hearthembedtest"
	testAPISecret = "hearth-embed-selftest-secret-0123456789ab" // ValidateKeys 要求 >= 32 字符
	testRoom      = "embed-selftest"
)

// 端到端：进程内起一个回环 LiveKit，一个客户端发音轨、另一个订阅并读到 RTP，
// Stop 后 HTTP 与 UDP 端口都能重新 bind。
func TestEmbeddedRoundTrip(t *testing.T) {
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
	stopped := false
	defer func() {
		if !stopped {
			srv.Stop()
		}
	}()

	url := fmt.Sprintf("ws://127.0.0.1:%d", httpPort)

	var gotRTP atomic.Bool
	subCB := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				if track.Kind() != webrtc.RTPCodecTypeAudio {
					return
				}
				go func() {
					_ = track.SetReadDeadline(time.Now().Add(10 * time.Second))
					if _, _, err := track.ReadRTP(); err == nil {
						gotRTP.Store(true)
					}
				}()
			},
		},
	}
	sub := connect(t, url, 1, "sub", subCB)
	defer sub.Disconnect()

	pub := connect(t, url, 2, "pub", nil)
	defer pub.Disconnect()

	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		t.Fatalf("NewLocalSampleTrack: %v", err)
	}
	if _, err := pub.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "selftest"}); err != nil {
		t.Fatalf("PublishTrack: %v", err)
	}

	// 假 opus 负载：SFU 与订阅端都不解码，只要 RTP 流动即可。
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_ = track.WriteSample(media.Sample{
					Data:     []byte{0xf8, 0xff, 0xfe, 0x00, 0x01, 0x02, 0x03, 0x04},
					Duration: 20 * time.Millisecond,
				}, nil)
			}
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for !gotRTP.Load() {
		if time.Now().After(deadline) {
			t.Fatal("订阅端 20s 内没有读到 RTP")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 先等客户端真的断干净：LiveKit 的关闭序列要等参与者收尾，否则 Stop 会拖到超时
	pub.Disconnect()
	sub.Disconnect()
	waitDisconnected(t, pub, sub)

	srv.Stop()
	stopped = true
	if !portFree(t, "tcp", httpPort) {
		t.Errorf("Stop 后 HTTP 端口 %d 仍被占用", httpPort)
	}
	if !portFree(t, "udp", udpPort) {
		t.Errorf("Stop 后 UDP 端口 %d 仍被占用", udpPort)
	}
}

func TestStartRejectsMissingKeys(t *testing.T) {
	if _, err := Start(context.Background(), Options{HTTPPort: 1, UDPPort: 1}); err == nil {
		t.Fatal("缺少密钥时应当报错")
	}
}

func waitDisconnected(t *testing.T, rooms ...*lksdk.Room) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, room := range rooms {
		for room.ConnectionState() != lksdk.ConnectionStateDisconnected {
			if time.Now().After(deadline) {
				t.Fatalf("客户端 10s 内未断开: %s", room.ConnectionState())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func connect(t *testing.T, url string, uid int64, tag string, cb *lksdk.RoomCallback) *lksdk.Room {
	t.Helper()
	meta := rtc.Meta{UID: uid, Username: "u" + strconv.FormatInt(uid, 10), Tag: tag}
	token, err := lktoken.Sign(testAPIKey, testAPISecret, testRoom, meta, true)
	if err != nil {
		t.Fatalf("签发令牌: %v", err)
	}
	room, err := lksdk.ConnectToRoomWithToken(url, token, cb)
	if err != nil {
		t.Fatalf("连接 %s: %v", tag, err)
	}
	return room
}

// freePort 借一个空闲端口号：绑上再放掉，避免测试之间撞固定端口。
func freePort(t *testing.T, proto string) int {
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

func portFree(t *testing.T, proto string, port int) bool {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if proto == "udp" {
		pc, err := net.ListenPacket("udp", ":"+strconv.Itoa(port))
		if err != nil {
			t.Log(err)
			return false
		}
		pc.Close()
		return true
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Log(err)
		return false
	}
	ln.Close()
	return true
}
