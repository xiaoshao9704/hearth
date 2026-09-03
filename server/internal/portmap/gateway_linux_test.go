package portmap

import (
	"strings"
	"testing"
)

// /proc/net/ipv6_route 每行：目的(32hex) 目的前缀 源(32hex) 源前缀 下一跳(32hex) metric refcnt use flags iface
func ipv6RouteLine(dest, destLen, nexthop, iface string) string {
	return strings.Join([]string{dest, destLen, strings.Repeat("0", 32), "00", nexthop, "00000400", "00000000", "00000000", "00000003", iface}, " ")
}

func TestParseIPv6RouteGUANexthop(t *testing.T) {
	// 默认路由（目的全零、前缀 0）的下一跳是 GUA（2001:db8::1），应取到。
	gua := "20010db8000000000000000000000001"
	other := ipv6RouteLine("20010db8000000000000000000000000", "40", strings.Repeat("0", 32), "eth0") // 一条非默认路由
	def := ipv6RouteLine(strings.Repeat("0", 32), "00", gua, "eth0")
	nh, ok := parseIPv6Route(other + "\n" + def + "\n")
	if !ok {
		t.Fatal("应从默认路由取到 GUA 下一跳")
	}
	if nh.String() != "2001:db8::1" {
		t.Fatalf("下一跳 = %v，应为 2001:db8::1", nh)
	}
}

func TestParseIPv6RouteLinkLocalNexthopRejected(t *testing.T) {
	// 默认路由的下一跳是链路本地（fe80::1）：PCP v6 要 GUA，链路本地应返回 false 让发现退 UPnP。
	ll := "fe800000000000000000000000000001"
	def := ipv6RouteLine(strings.Repeat("0", 32), "00", ll, "eth0")
	if _, ok := parseIPv6Route(def + "\n"); ok {
		t.Fatal("链路本地下一跳应返回 false")
	}
}

func TestParseIPv6RouteNoDefault(t *testing.T) {
	// 没有默认路由（只有带前缀的直连路由）：返回 false。
	line := ipv6RouteLine("20010db8000000000000000000000000", "40", strings.Repeat("0", 32), "eth0")
	if _, ok := parseIPv6Route(line + "\n"); ok {
		t.Fatal("没有默认路由时应返回 false")
	}
}
