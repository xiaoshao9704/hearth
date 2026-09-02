package lite

import (
	"fmt"
	"net"

	"github.com/pion/webrtc/v4"
)

// Transport 持有 ICE-Lite 的长期资源：UDP 单端口 socket 与 MediaEngine。
// pion 的地址改写规则挂在 SettingEngine 上、webrtc.API 建好不可改，所以长期资源
// 与 API 组装分离：每个 PeerConnection 用当时的规则组装一个 API（纯结构体组装，
// 不开 socket、不触网），宣告规则刷新只影响新 PC，不打扰在途会话。
type Transport struct {
	udpConn *net.UDPConn
	m       *webrtc.MediaEngine
}

// NewTransport 监听 UDP 单端口。监听失败原样返回（调用方决定是否重试）。
func NewTransport(port int, m *webrtc.MediaEngine) (*Transport, error) {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("媒体端口 %d 监听失败: %w", port, err)
	}
	return &Transport{udpConn: udpConn, m: m}, nil
}

// NewAPI 用给定宣告规则组装一个 ICE-Lite API：服务器公网直达，连通性检查由对端发起；
// rules 非空时按规则改写/追加 host candidate 的宣告地址。
func (t *Transport) NewAPI(rules []webrtc.ICEAddressRewriteRule) (*webrtc.API, error) {
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, t.udpConn))
	if len(rules) > 0 {
		if err := se.SetICEAddressRewriteRules(rules...); err != nil {
			return nil, fmt.Errorf("地址改写规则: %w", err)
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(t.m), webrtc.WithSettingEngine(se)), nil
}
