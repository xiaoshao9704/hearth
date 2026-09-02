// Package lite 是进程内 ICE-Lite 内核（ember / bellows）共用的传输基建：
// UDP 单端口 mux + ICE-Lite + 公网 IP 通告。各内核只带自己的 MediaEngine 与配置键。
package lite

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
)

// DefaultSTUNServers 默认 STUN 列表（国内可达的 miwifi + Google）；不可达时靠各内核的
// *_stun_servers 配置覆盖。
var DefaultSTUNServers = []string{"stun.miwifi.com:3478", "stun.l.google.com:19302", "stun1.l.google.com:19302"}

// LocalIPs 全部本机网卡地址（跳过回环与链路本地）。多网卡机器（物理网卡/VPN 虚拟网卡/
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

// stunQuery 从 localIP 绑定向单个 STUN 服务器发 Binding 请求，取 XOR-MAPPED-ADDRESS；
// 失败返回空。
func stunQuery(localIP, server string, timeout time.Duration) string {
	raddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return ""
	}
	conn, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP(localIP)}, raddr)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw); err != nil {
		return ""
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return ""
	}
	res := &stun.Message{Raw: buf[:n]}
	if res.Decode() != nil {
		return ""
	}
	var xor stun.XORMappedAddress
	if xor.GetFrom(res) != nil {
		return ""
	}
	return xor.IP.String()
}

// stunProbe 从 localIP 并发探测各 STUN 服务器，首个成功即返回，全部失败返回空。
// 服务器之间并发：默认列表里部分不可达时不必逐台串行等超时。参考 LiveKit getExternalIP
// 的 per-NIC 探测。
func stunProbe(localIP string, servers []string, timeout time.Duration) string {
	ch := make(chan string, len(servers))
	for _, srv := range servers {
		go func(s string) { ch <- stunQuery(localIP, s, timeout) }(srv)
	}
	deadline := time.After(timeout)
	for range servers {
		select {
		case ip := <-ch:
			if ip != "" {
				return ip
			}
		case <-deadline:
			return ""
		}
	}
	return ""
}

// probeAllSTUN 并发探测各网卡的公网映射，返回 local→external（探测失败的网卡不在结果里）。
func probeAllSTUN(locals, servers []string, timeout time.Duration) map[string]string {
	type mapping struct{ local, external string }
	ch := make(chan mapping, len(locals))
	var wg sync.WaitGroup
	for _, ip := range locals {
		wg.Add(1)
		go func(local string) {
			defer wg.Done()
			if ext := stunProbe(local, servers, timeout); ext != "" {
				ch <- mapping{local, ext}
			}
		}(ip)
	}
	wg.Wait()
	close(ch)
	out := make(map[string]string, len(locals))
	for m := range ch {
		out[m.local] = m.external
	}
	return out
}

// appendRule 内网 candidate 保留、公网映射追加（对应 LiveKit 的 advertise_internal_ip 语义）。
func appendRule(external, local string) webrtc.ICEAddressRewriteRule {
	return webrtc.ICEAddressRewriteRule{
		External:        []string{external},
		Local:           local,
		AsCandidateType: webrtc.ICECandidateTypeHost,
		Mode:            webrtc.ICEAddressRewriteAppend,
	}
}

// AnnounceRules 计算 ICE 地址改写规则（复刻 LiveKit ingress 的 SetNAT1To1AddressRewriteRules
// 思路：网卡地址照常自动收集宣告，探到的公网映射按 append 追加，对端各取可达者）：
//   - 显式配置 publicIP：catch-all 替换规则，所有 host candidate 改写成它（覆盖语义）；
//   - 留空：每张网卡并发 STUN 探测公网映射（servers 逗号分隔，空用 DefaultSTUNServers），
//     探到的以 append 规则追加。
//
// 探测用的是临时端口的映射，媒体端口的公网映射假定同 IP——1:1 NAT/端口转发生效，
// 对称 NAT 不成立（与 LiveKit 同一假设）。
// 返回 nil 表示无需改写（网卡直连公网或探测全失败），candidate 按网卡地址原样宣告。
func AnnounceRules(publicIP, stunServers string) []webrtc.ICEAddressRewriteRule {
	servers := splitTrim(stunServers)
	if len(servers) == 0 {
		servers = DefaultSTUNServers
	}
	return announceRules(publicIP, LocalIPs(), servers, probeAllSTUN)
}

// announceRules 纯组合逻辑，探测函数注入以便离线单测。语义见 AnnounceRules 注释。
// 单网卡最坏耗时 = 单服务器超时（服务器并发）。
func announceRules(publicIP string, locals, servers []string,
	probe func(locals, servers []string, timeout time.Duration) map[string]string) []webrtc.ICEAddressRewriteRule {

	if publicIP != "" {
		return []webrtc.ICEAddressRewriteRule{{
			External:        []string{publicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			// Mode 零值 = 替换：所有 host candidate 都改写成该地址
		}}
	}
	if len(locals) == 0 {
		return nil
	}
	mapping := probe(locals, servers, 2*time.Second)
	var rules []webrtc.ICEAddressRewriteRule
	seen := make(map[string]bool, len(mapping))
	for _, local := range locals {
		ext, ok := mapping[local]
		// 未探到 / 网卡直连公网（candidate 本身已正确）/ 多网卡同出口映射重复
		if !ok || ext == local || seen[ext] {
			continue
		}
		seen[ext] = true
		rules = append(rules, appendRule(ext, local))
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
