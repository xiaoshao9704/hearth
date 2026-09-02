package lite

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// 回归：Transport 上第二个（及后续）API 组装的 PC 必须能完成 ICE 连通。
// 若 mux 被重复创建（同一 socket 多个读循环抢包），第一个会话之后的新会话会全部饿死。
func TestTransportLaterPCConnects(t *testing.T) {
	tr, err := NewTransport(0, &webrtc.MediaEngine{})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}

	// 第一个 API 的 PC：只建不连（生产里在途会话的 API 永不关闭，其 mux 读循环一直活着）
	api1, err := tr.NewAPI(nil)
	if err != nil {
		t.Fatalf("NewAPI #1: %v", err)
	}
	if _, err := api1.NewPeerConnection(webrtc.Configuration{}); err != nil {
		t.Fatalf("第一个 PC: %v", err)
	}

	api2, err := tr.NewAPI(nil)
	if err != nil {
		t.Fatalf("NewAPI #2: %v", err)
	}
	server, err := api2.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("第二个 PC: %v", err)
	}
	defer server.Close()

	// pion 客户端全 ICE 对 ICE-Lite 服务端：DataChannel 强制走完 ICE+DTLS
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
	gatherDone := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatalf("客户端 SetLocal: %v", err)
	}
	<-gatherDone

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
	if err := client.SetRemoteDescription(*server.LocalDescription()); err != nil {
		t.Fatalf("客户端 SetRemote: %v", err)
	}

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("第二个 PC 10s 内未完成 ICE 连接（mux 被重复创建，包被旧读循环抢走）")
	}
}
