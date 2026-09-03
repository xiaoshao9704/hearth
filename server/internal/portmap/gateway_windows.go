package portmap

import (
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

func defaultGateway() (netip.Addr, error) {
	const flags = windows.GAA_FLAG_INCLUDE_GATEWAYS |
		windows.GAA_FLAG_SKIP_UNICAST |
		windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST |
		windows.GAA_FLAG_SKIP_DNS_SERVER |
		windows.GAA_FLAG_SKIP_FRIENDLY_NAME

	// 缓冲区大小要问一次再取一次：适配器数量随时可能变，重试几轮即可。
	size := uint32(15000)
	var buf []byte
	for range 4 {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_INET, flags, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return netip.Addr{}, err
		}
		buf = nil
	}
	if buf == nil {
		return netip.Addr{}, errNoGateway
	}

	for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		if aa.OperStatus != windows.IfOperStatusUp {
			continue
		}
		for ga := aa.FirstGatewayAddress; ga != nil; ga = ga.Next {
			ip := ga.Address.IP()
			if ip == nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if !addr.Is4() || addr.IsUnspecified() {
				continue
			}
			return addr, nil
		}
	}
	return netip.Addr{}, errNoGateway
}

// defaultGateway6 取 AF_INET6 适配器的 FirstGatewayAddress，仅当是 GUA 才返回 true
// （链路本地 nexthop → 让发现退到 UPnP）。
func defaultGateway6() (netip.Addr, bool) {
	const flags = windows.GAA_FLAG_INCLUDE_GATEWAYS |
		windows.GAA_FLAG_SKIP_UNICAST |
		windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST |
		windows.GAA_FLAG_SKIP_DNS_SERVER |
		windows.GAA_FLAG_SKIP_FRIENDLY_NAME

	size := uint32(15000)
	var buf []byte
	for range 4 {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_INET6, flags, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return netip.Addr{}, false
		}
		buf = nil
	}
	if buf == nil {
		return netip.Addr{}, false
	}

	for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		if aa.OperStatus != windows.IfOperStatusUp {
			continue
		}
		for ga := aa.FirstGatewayAddress; ga != nil; ga = ga.Next {
			ip := ga.Address.IP()
			if ip == nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			if isGlobalUnicastV6(addr) {
				return addr, true
			}
		}
	}
	return netip.Addr{}, false
}
