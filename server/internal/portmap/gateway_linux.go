package portmap

import (
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// /proc/net/route 的 Flags 位：置位表示该路由有下一跳网关。
const rtfGateway = 0x0002

// defaultGateway6 读 /proc/net/ipv6_route：字段依次是 目的(32hex) 目的前缀 源 源前缀
// 下一跳(32hex) …… 默认路由是目的全零、前缀 0 的那条，取它的下一跳。仅当下一跳是 GUA
// 才返回 true（链路本地 nexthop → UPnP 兜底）。
func defaultGateway6() (netip.Addr, bool) {
	b, err := os.ReadFile("/proc/net/ipv6_route")
	if err != nil {
		return netip.Addr{}, false
	}
	return parseIPv6Route(string(b))
}

func parseIPv6Route(text string) (netip.Addr, bool) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		destLen, err := strconv.ParseUint(f[1], 16, 8)
		if err != nil || destLen != 0 || strings.Trim(f[0], "0") != "" {
			continue // 只认目的全零、前缀长度 0 的默认路由
		}
		raw, err := hex.DecodeString(f[4])
		if err != nil || len(raw) != 16 {
			continue
		}
		nh := netip.AddrFrom16([16]byte(raw))
		if isGlobalUnicastV6(nh) {
			return nh, true
		}
	}
	return netip.Addr{}, false
}

func defaultGateway() (netip.Addr, error) {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	for _, line := range strings.Split(string(b), "\n")[1:] { // 跳过表头
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		dest, err := strconv.ParseUint(f[1], 16, 32)
		if err != nil || dest != 0 {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&rtfGateway == 0 {
			continue
		}
		gw, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			continue
		}
		// 字段是主机序（小端）的十六进制，按小端还原成点分四段的字节序。
		var a [4]byte
		binary.LittleEndian.PutUint32(a[:], uint32(gw))
		addr := netip.AddrFrom4(a)
		if addr.IsUnspecified() {
			continue
		}
		return addr, nil
	}
	return netip.Addr{}, errNoGateway
}
