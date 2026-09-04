// Package lite 是进程内内核的宣告设施（Announcer：探测缓存 + SDP 出口追加候选）。
// lkembed 的 LiveKit 自己建 PeerConnection，只用这里的 Announcer 做宣告探测；
// cmd/stage 同。
package lite

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun/v3"
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

// stunExternals 探测各网卡的公网映射，返回 local→external。探测函数注入以便离线单测。
// 两类结果不入表：探不到的网卡；以及 ext == local 的网卡（它直连公网，host 候选本身就是对的，
// 再追加一条只是重复）。返回 nil 表示无公网映射可宣告。
//
// 探测走的是临时端口的 socket，探到的映射属于那个临时端口，与媒体端口无关，
// 所以 STUN 只给得出**公网 IP**，给不出可用的外部端口——宣告时把它与媒体端口拼在一起
// 是「NAT 端口保持」的假设，只在 1:1 NAT（服务器直接绑公网 IP）成立。
// 准确的外部端口只有显式的端口映射能给（见 Announcer.Announce 的优先级）。
// 单网卡最坏耗时 = 单服务器超时（服务器之间并发）。
func stunExternals(locals, servers []string,
	probe func(locals, servers []string, timeout time.Duration) map[string]string) map[string]string {

	if len(locals) == 0 {
		return nil
	}
	out := map[string]string{}
	for local, ext := range probe(locals, servers, 2*time.Second) {
		if ext == "" || ext == local {
			continue
		}
		out[local] = ext
	}
	if len(out) == 0 {
		return nil
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
