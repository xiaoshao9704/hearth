// Package lite 是进程内 ICE-Lite 内核（ember / bellows）共用的传输基建：
// UDP 单端口 mux + ICE-Lite + 公网 IP 通告。各内核只带自己的 MediaEngine 与配置键。
package lite

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// ProbePublicIP 经第三方回显服务探测本机公网 IP，全部失败返回空。
func ProbePublicIP() string {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, u := range []string{"http://ip.3322.net", "https://api.ipify.org"} {
		if resp, err := client.Get(u); err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
			resp.Body.Close()
			ip := strings.TrimSpace(string(b))
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	return ""
}

// NewAPI 监听 UDP 单端口并构建 ICE-Lite API：服务器公网直达，连通性检查由对端发起；
// publicIP 非空时以 host 候选通告。监听失败原样返回（调用方决定是否重试）。
func NewAPI(port int, publicIP string, m *webrtc.MediaEngine) (*webrtc.API, error) {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("媒体端口 %d 监听失败: %w", port, err)
	}
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpConn))
	if publicIP != "" {
		se.SetNAT1To1IPs([]string{publicIP}, webrtc.ICECandidateTypeHost)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se)), nil
}
