package portmap

import (
	"encoding/binary"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// /proc/net/route 的 Flags 位：置位表示该路由有下一跳网关。
const rtfGateway = 0x0002

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
