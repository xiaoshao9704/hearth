// Package lite 是进程内 ICE-Lite 内核（ember / bellows）共用的传输基建：
// UDP 单端口 mux + ICE-Lite + 公网 IP 通告。各内核只带自己的 MediaEngine 与配置键。
package lite

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
)

// DefaultSTUNServers 默认 STUN 列表；国内不可达，靠各内核的 *_stun_servers 配置覆盖，
// 全部探测失败时回落 HTTP 回显探测兜底。
var DefaultSTUNServers = []string{"stun.l.google.com:19302", "stun1.l.google.com:19302"}

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

// LocalIPs 全部本机网卡地址（跳过回环与链路本地）。多网卡机器（LAN/tailscale/
// docker 网桥并存）上每张网卡都可能服务一类对端，只取出口网卡会漏掉其余可达路径。
func LocalIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		ips = append(ips, ip.String())
	}
	return ips
}

// EgressIP 默认出口网卡 IP：UDP Dial 不发包，只借路由表选网卡。
func EgressIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// stunProbe 从 localIP 绑定发起到各 STUN 服务器的 Binding 请求，取 XOR-MAPPED-ADDRESS；
// 依次尝试、首个成功即返回，全部失败返回空。参考 LiveKit getExternalIP 的 per-NIC 探测。
func stunProbe(localIP string, servers []string, timeout time.Duration) string {
	for _, srv := range servers {
		raddr, err := net.ResolveUDPAddr("udp", srv)
		if err != nil {
			continue
		}
		conn, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP(localIP)}, raddr)
		if err != nil {
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
		ip := ""
		if _, err := conn.Write(stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw); err == nil {
			buf := make([]byte, 1500)
			if n, err := conn.Read(buf); err == nil {
				res := &stun.Message{Raw: buf[:n]}
				if res.Decode() == nil {
					var xor stun.XORMappedAddress
					if xor.GetFrom(res) == nil {
						ip = xor.IP.String()
					}
				}
			}
		}
		conn.Close()
		if ip != "" {
			return ip
		}
	}
	return ""
}

// AnnounceRules 计算 ICE 地址改写规则（复刻 LiveKit ingress 的 SetNAT1To1AddressRewriteRules
// 思路：网卡地址照常自动收集宣告，探到的公网映射按 append 追加，对端各取可达者）：
//   - 显式配置 publicIP：catch-all 替换规则，所有 host candidate 改写成它（覆盖语义）；
//   - 留空：每张网卡并发 STUN 探测公网映射（servers 逗号分隔，空用 DefaultSTUNServers），
//     探到的以 append 规则追加；STUN 全部失败（如默认列表国内不可达）时回落 HTTP 回显
//     探测，映射到默认出口网卡。
//
// 返回 nil 表示无需改写（网卡直连公网或探测全失败），candidate 按网卡地址原样宣告。
func AnnounceRules(publicIP, stunServers string) []webrtc.ICEAddressRewriteRule {
	if publicIP != "" {
		return []webrtc.ICEAddressRewriteRule{{
			External:        []string{publicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			// Mode 零值 = 替换：所有 host candidate 都改写成该地址
		}}
	}
	locals := LocalIPs()
	if len(locals) == 0 {
		return nil
	}
	servers := splitTrim(stunServers)
	if len(servers) == 0 {
		servers = DefaultSTUNServers
	}

	// per-NIC 并发探测；单网卡最坏耗时 = 单服务器超时 × 服务器数
	type mapping struct{ local, external string }
	ch := make(chan mapping, len(locals))
	var wg sync.WaitGroup
	for _, ip := range locals {
		wg.Add(1)
		go func(local string) {
			defer wg.Done()
			if ext := stunProbe(local, servers, 2*time.Second); ext != "" {
				ch <- mapping{local, ext}
			}
		}(ip)
	}
	wg.Wait()
	close(ch)

	var rules []webrtc.ICEAddressRewriteRule
	resolved := 0
	for m := range ch {
		resolved++
		if m.external == m.local {
			continue // 网卡直连公网，candidate 本身已正确
		}
		rules = append(rules, webrtc.ICEAddressRewriteRule{
			External:        []string{m.external},
			Local:           m.local,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteAppend, // 内网 candidate 保留，公网映射追加
		})
	}
	if resolved == 0 {
		if pub := ProbePublicIP(); pub != "" {
			if eg := EgressIP(); eg != "" && eg != pub {
				rules = append(rules, webrtc.ICEAddressRewriteRule{
					External:        []string{pub},
					Local:           eg,
					AsCandidateType: webrtc.ICECandidateTypeHost,
					Mode:            webrtc.ICEAddressRewriteAppend,
				})
			}
		}
	}
	return rules
}

// RuleExternals 提取规则里的外部地址，仅用于日志。
func RuleExternals(rules []webrtc.ICEAddressRewriteRule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.External...)
	}
	return out
}

func splitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// NewAPI 监听 UDP 单端口并构建 ICE-Lite API：服务器公网直达，连通性检查由对端发起；
// rules 非空时按规则改写/追加 host candidate 的宣告地址。监听失败原样返回（调用方决定是否重试）。
func NewAPI(port int, rules []webrtc.ICEAddressRewriteRule, m *webrtc.MediaEngine) (*webrtc.API, error) {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("媒体端口 %d 监听失败: %w", port, err)
	}
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpConn))
	if len(rules) > 0 {
		if err := se.SetICEAddressRewriteRules(rules...); err != nil {
			udpConn.Close()
			return nil, fmt.Errorf("地址改写规则: %w", err)
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se)), nil
}
