package portmap

import (
	"errors"
	"net"
	"net/netip"
	"slices"
)

// errNoGateway 路由表里没有可用的 IPv4 默认路由（含容器 bridge 网络里网关是 docker0 的情况——
// 那种情况下地址本身能读到，只是申请必然失败，由 Mapper 的诊断文案兜底）。
var errNoGateway = errors.New("未找到 IPv4 默认网关")

// defaultGateway 返回默认路由（0.0.0.0/0）的下一跳地址，各平台走系统原生途径：
// linux 读 /proc/net/route、darwin 读路由表 RIB、windows 调 GetAdaptersAddresses。
// 见 gateway_linux.go / gateway_darwin.go / gateway_windows.go / gateway_other.go。

// defaultGateway6 返回 IPv6 默认路由的下一跳，仅当它是 GUA 才返回 true——PCP v6 pinhole
// 的反欺骗要求源、目的都是 GUA（真机核实：发链路本地 nexthop 无回应），链路本地 nexthop
// 返回 false 让发现退到 UPnP。各平台实现见 gateway_*.go。

// isGlobalUnicastV6 是不是可全局路由的 IPv6 单播地址（GUA）：排除 ULA(fc00::/7)、
// 链路本地(fe80::/10) 与非单播。文档地址段 2001:db8::/32 不是 ULA，按 GUA 对待。
func isGlobalUnicastV6(a netip.Addr) bool {
	return a.Is6() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLinkLocalUnicast()
}

// globalUnicastV6 本机所有 GUA：每个都要单独放行（一台多 GUA 时临时/隐私地址各是一条）。
// 结果去重并排序，Mapper 据此判断 GUA 集合是否变化，稳定顺序才不会每轮误判成变化。
func globalUnicastV6() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		na, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		na = na.Unmap()
		if isGlobalUnicastV6(na) {
			out = append(out, na)
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}
