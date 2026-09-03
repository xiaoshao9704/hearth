package portmap

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// IPv6 pinhole 与 v4 端口映射的本质区别（详见 docs/plan-portmap.md 第十二节）：
// v6 无 NAT，pinhole 是防火墙「放行」而非端口翻译。外部端口 == 内部端口 == Want.Port，
// 外部地址 == 本机自己的 GUA（不是网关地址）。因此它不进 Lookup / UDPExternal（MappedFunc）：
// 那是给 v4 srflx 候选宣告外部地址用的，而 v6 host 候选进程本就已经在宣告，pinhole 只是
// 让它真的可达。每条 (Want, 本机 GUA) 是一条独立放行；一台多 GUA（临时/隐私地址）时对每个
// global-unicast v6 都放行。失败一律静默（日志一次），绝不影响启动、不影响 v4、不让 healthz 非 200。

// Pinhole 一条已建立的 v6 防火墙放行，给管理后台/日志回显。
type Pinhole struct {
	Proto  string
	Port   int
	GUA    netip.Addr
	Method string // "pcp6" | "upnp6"
}

// pinholeClient 单一途径的 v6 pinhole 客户端，与 v4 的 client 并列。
// 实现：pcp.go（pcp6Client，复用 PCP MAP 报文）、upnp.go（upnp6Client，WANIPv6FirewallControl）。
type pinholeClient interface {
	Method() string
	// Open 放行 gua 上的 (proto, port) 入站，须幂等（同一条重发即续租）。
	Open(ctx context.Context, proto string, port int, gua netip.Addr, lifetime time.Duration) (pinhole, error)
	// Close 撤销放行；已不存在不算错误。
	Close(ctx context.Context, h pinhole) error
}

// pinhole 一条放行的自认领信息：PCP 靠 (proto, port, gua) + 客户端里存的 nonce 认领，
// UPnP 靠网关返回的 UniqueID 认领。客户端知道自己的类型，各取所需字段。
type pinhole struct {
	proto  string
	port   int
	gua    netip.Addr
	method string
	id     uint16 // UPnP UniqueID
	hasID  bool
}

// pinKey (协议, 内部端口, 本机 GUA) 唯一标识一条放行。
type pinKey struct {
	proto string
	port  int
	gua   netip.Addr
}

// pinNonceKey PCP nonce 与 UPnP UniqueID 在客户端内按 (协议, 端口, GUA) 存留：
// 续租与删除复用它认领同一条放行。
func pinNonceKey(proto string, port int, gua netip.Addr) string {
	return strings.ToLower(proto) + "/" + strconv.Itoa(port) + "/" + gua.String()
}
