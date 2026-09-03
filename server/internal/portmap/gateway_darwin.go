package portmap

import (
	"net/netip"
	"syscall"

	"golang.org/x/net/route"
)

func defaultGateway() (netip.Addr, error) {
	rib, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return netip.Addr{}, err
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return netip.Addr{}, err
	}
	const want = syscall.RTF_UP | syscall.RTF_GATEWAY
	for _, msg := range msgs {
		rm, ok := msg.(*route.RouteMessage)
		if !ok || rm.Flags&want != want {
			continue
		}
		if len(rm.Addrs) <= syscall.RTAX_GATEWAY {
			continue
		}
		dst, ok := rm.Addrs[syscall.RTAX_DST].(*route.Inet4Addr)
		if !ok || dst.IP != [4]byte{} { // 只认 0.0.0.0/0
			continue
		}
		gw, ok := rm.Addrs[syscall.RTAX_GATEWAY].(*route.Inet4Addr)
		if !ok {
			continue // 默认路由也可能指向链路（点对点接口），那种没有网关地址
		}
		addr := netip.AddrFrom4(gw.IP)
		if addr.IsUnspecified() {
			continue
		}
		return addr, nil
	}
	return netip.Addr{}, errNoGateway
}
