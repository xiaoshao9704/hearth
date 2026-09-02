package lite

import (
	"fmt"
	"net"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// Transport 持有 ICE-Lite 的长期资源：UDP 单端口 mux 与 MediaEngine。
// pion 的地址改写规则挂在 SettingEngine 上、webrtc.API 建好不可改，所以长期资源
// 与 API 组装分离：每个 PeerConnection 用当时的规则组装一个 API（纯结构体组装，
// 不开 socket、不触网），宣告规则刷新只影响新 PC，不打扰在途会话。
//
// mux 必须只建一次、全 API 共用：UDPMuxDefault 构造即起 connWorker 读循环，
// 同一 socket 上多个 mux 会互相抢包（旧 mux 把不属于它的连通性检查丢弃），
// 表现为第一个会话之后的新会话 ICE 全部失败。
type Transport struct {
	mux ice.UDPMux
	m   *webrtc.MediaEngine
}

// NewTransport 监听 UDP 单端口并建共享 mux。监听失败原样返回（调用方决定是否重试）。
func NewTransport(port int, m *webrtc.MediaEngine) (*Transport, error) {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("媒体端口 %d 监听失败: %w", port, err)
	}
	return &Transport{mux: webrtc.NewICEUDPMux(nil, udpConn), m: m}, nil
}

// NewAPI 用给定宣告规则组装一个 ICE-Lite API：服务器公网直达，连通性检查由对端发起；
// rules 非空时按规则改写/追加 host candidate 的宣告地址。
func (t *Transport) NewAPI(rules []webrtc.ICEAddressRewriteRule) (*webrtc.API, error) {
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetICEUDPMux(t.mux)
	if len(rules) > 0 {
		if err := se.SetICEAddressRewriteRules(rules...); err != nil {
			return nil, fmt.Errorf("地址改写规则: %w", err)
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(t.m), webrtc.WithSettingEngine(se)), nil
}
