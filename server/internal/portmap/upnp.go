package portmap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/httpu"
	"github.com/huin/goupnp/soap"
)

// upnpConn 是 IGDv2/v1/PPP 三种 WANConnection SOAP 服务的公共子集：三者的
// AddPortMapping/DeletePortMapping/GetExternalIPAddress 方法签名逐字相同，
// 收敛成一个接口后 Map/Unmap 不必按版本分叉。GetServiceClient 用来取回
// 发现时记录的设备描述 URL（算内部客户端地址要用）。
type upnpConn interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	GetServiceClient() *goupnp.ServiceClient
}

// upnpClient 实现 client 接口。v2 非空时说明发现到的是 IGDv2，
// 才有 AddAnyPortMapping 这个「网关任选端口」动作。
type upnpClient struct {
	conn upnpConn
	v2   *internetgateway2.WANIPConnection2
	// internalClient 覆盖 AddPortMapping 的 NewInternalClient，级联申请时用（见 chain.go）：
	// 报文经内层 NAT 到达上游时源地址已变成内层路由的 WAN 地址，而 miniupnpd 的
	// secure_mode 只允许给请求源地址开洞。零值 = 用 localAddrFor 取到的出口地址。
	internalClient netip.Addr
}

// discoverUPnP 依次尝试 IGDv2 → IGDv1 → WANPPPConnection1，取第一个发现到的。
// IGDv1 是主路径而非回退：不少消费级路由器（如 miniupnpd 的 force_igd_desc_v1）
// 默认以 v1 身份宣告。三路发现共用调用方传入的 ctx，总耗时由它的 deadline 控制。
func discoverUPnP(ctx context.Context) (client, error) {
	if clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0], v2: clients[0]}, nil
	}
	if clients, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0]}, nil
	}
	if clients, _, err := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0]}, nil
	}
	return nil, ErrUnsupported
}

// ssdpPort SSDP 的固定端口；做成变量只为单测把单播搜索指向回环上的假应答器。
var ssdpPort = 1900

// ssdpUnicastTimeout 单播 M-SEARCH 每个 ST 的等待时长：对端就在链路上，
// 应答是即时的，等久了只是白拖（两个 ST 串行，总耗时还受调用方的 ctx 限制）。
const ssdpUnicastTimeout = 1500 * time.Millisecond

// upstreamSearchTargets 单播搜索的 ST：IGD 设备类型 v1 优先（不少设备默认以 v1 身份宣告）。
var upstreamSearchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

// discoverUPnPAt 单播发现指定地址上的 IGD：M-SEARCH 直接发给它（UPnP 1.1 允许单播搜索），
// 绕开「多播出不了本级路由」——级联申请时上游网关不在本机的多播域里（见 chain.go）。
// 返回具体类型而不是 client 接口，调用方要覆盖 internalClient。
func discoverUPnPAt(ctx context.Context, host netip.Addr) (*upnpClient, error) {
	loc, err := ssdpUnicastLocation(ctx, host)
	if err != nil {
		return nil, err
	}
	root, err := goupnp.DeviceByURLCtx(ctx, loc)
	if err != nil {
		return nil, err
	}
	if clients, err := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(root, loc); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0], v2: clients[0]}, nil
	}
	if clients, err := internetgateway2.NewWANIPConnection1ClientsFromRootDevice(root, loc); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0]}, nil
	}
	if clients, err := internetgateway2.NewWANPPPConnection1ClientsFromRootDevice(root, loc); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0]}, nil
	}
	return nil, ErrUnsupported
}

// ssdpUnicastLocation 向 host:1900 单播 M-SEARCH，取第一个应答里的设备描述 URL。
// 按 UPnP 1.1，单播搜索不带 MX（那是给多播应答错峰用的）。
func ssdpUnicastLocation(ctx context.Context, host netip.Addr) (*url.URL, error) {
	hu, err := httpu.NewHTTPUClient()
	if err != nil {
		return nil, err
	}
	defer hu.Close()

	dst := netip.AddrPortFrom(host, uint16(ssdpPort)).String()
	for _, st := range upstreamSearchTargets {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		req := (&http.Request{
			Method: "M-SEARCH",
			Host:   dst,
			URL:    &url.URL{Opaque: "*"},
			// 直接给 Header 赋值避免被标题化：SSDP 的头名是大小写敏感的。
			Header: http.Header{
				"HOST": []string{dst},
				"MAN":  []string{`"ssdp:discover"`},
				"ST":   []string{st},
			},
		}).WithContext(ctx)

		resps, err := hu.Do(req, ssdpUnicastTimeout, 2)
		if err != nil {
			continue
		}
		for _, resp := range resps {
			loc, err := url.Parse(resp.Header.Get("LOCATION"))
			if err == nil && loc.Host != "" {
				return loc, nil
			}
		}
	}
	return nil, ErrUnsupported
}

func (u *upnpClient) Method() string { return "upnp" }

func (u *upnpClient) Map(ctx context.Context, w Want, external int, lifetime time.Duration) (Mapping, error) {
	localAddr := u.internalClient
	if !localAddr.IsValid() {
		var err error
		if localAddr, err = localAddrFor(u.conn.GetServiceClient().Location); err != nil {
			return Mapping{}, fmt.Errorf("确定本机出口地址失败: %w", err)
		}
	}

	proto := strings.ToUpper(w.Proto)
	seconds := uint32(lifetime / time.Second)

	gotExternal := external
	if external == 0 {
		// v1/PPP 没有「网关任选端口」的动作；方案里 v1 的处理方式是提示换端口。
		if u.v2 == nil {
			return Mapping{}, ErrConflict
		}
		// 首选端口仍传内部端口：IGDv2 不允许外部端口通配（716），被占时由网关改派并回传。
		reserved, err := u.v2.AddAnyPortMappingCtx(ctx, "", uint16(w.Port), proto, uint16(w.Port), localAddr.String(), true, w.Desc, seconds)
		if err != nil {
			return Mapping{}, classifyUPnPError(err)
		}
		gotExternal = int(reserved)
	} else {
		if err := u.conn.AddPortMappingCtx(ctx, "", uint16(external), proto, uint16(w.Port), localAddr.String(), true, w.Desc, seconds); err != nil {
			return Mapping{}, classifyUPnPError(err)
		}
	}

	extIPStr, err := u.conn.GetExternalIPAddressCtx(ctx)
	if err != nil {
		return Mapping{}, classifyUPnPError(err)
	}
	extIP, err := netip.ParseAddr(extIPStr)
	if err != nil {
		return Mapping{}, fmt.Errorf("网关返回的外部地址无法解析: %w", err)
	}

	var expiresAt time.Time
	if lifetime > 0 {
		expiresAt = time.Now().Add(lifetime)
	}

	return Mapping{
		Proto:      w.Proto,
		Internal:   w.Port,
		External:   gotExternal,
		ExternalIP: extIP,
		Method:     "upnp",
		ExpiresAt:  expiresAt,
	}, nil
}

func (u *upnpClient) Unmap(ctx context.Context, w Want, external int) error {
	err := u.conn.DeletePortMappingCtx(ctx, "", uint16(external), strings.ToUpper(w.Proto))
	if err == nil {
		return nil
	}
	var fault *soap.SOAPFaultError
	if errors.As(err, &fault) && fault.Detail.UPnPError.Errorcode == 714 {
		// NoSuchEntryInArray：映射本就不存在，视为已删除。
		return nil
	}
	return classifyUPnPError(err)
}

// localAddrFor 取本机面向设备的出口地址：向设备描述 URL 的 host:port 拨一个
// UDP 连接（不实际发包）取内核选定的 LocalAddr，而不是取本机第一块网卡的地址——
// 多网卡/多路由表时两者可能不是同一个。
func localAddrFor(loc *url.URL) (netip.Addr, error) {
	port := loc.Port()
	if port == "" {
		port = "80"
	}
	conn, err := net.Dial("udp", net.JoinHostPort(loc.Hostname(), port))
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(host)
}

// ssdpV6Group SSDP 的 IPv6 组播组（链路本地范围）。
var ssdpV6Group = netip.MustParseAddr("ff02::c")

// ssdpV6Timeout 每个 ST 等应答的时长，与 MX 配套；做成变量只为单测缩短等待。
var ssdpV6Timeout = 1500 * time.Millisecond

// v6SearchTargets v6 pinhole 是 IGDv2 的能力，先问 v2；有的设备只应答 v1 的 ST，
// 描述文档里却照样带 WANIPv6FirewallControl，所以再问一次 v1。
var v6SearchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
}

// ssdpSearchV6 走 IPv6 组播做 SSDP 发现，返回设备描述 URL 与其中的网关地址。
// v6 pinhole 不能复用 v4 的发现结果：miniupnpd 的 secure_mode 只允许给「SOAP 请求的源地址」
// 开洞，走 v4 通道时网关看到的 v6 客户端地址是 ::，AddPinhole 一律 606。而 v6 应答里的
// LOCATION 用的是网关自己的 GUA——这也是取网关 GUA 唯一可靠的来源：默认路由的下一跳
// 通常是链路本地地址，按前缀拼 ::1 只是猜。
func ssdpSearchV6(ctx context.Context) (*url.URL, netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, netip.Addr{}, err
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		src, ok := ifaceGUA(ifi)
		if !ok {
			continue // 没有 GUA 的网卡既没有要放行的候选，也当不了源地址
		}
		// 目的地址带 zone：链路本地范围的组播靠 scope id 选出口网卡，省掉 IPV6_MULTICAST_IF。
		dst := netip.AddrPortFrom(ssdpV6Group.WithZone(ifi.Name), uint16(ssdpPort))
		loc, gw, err := ssdpSearchV6From(ctx, src, dst)
		if err == nil {
			return loc, gw, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, netip.Addr{}, ErrUnsupported
}

// ssdpSearchV6From 从 src 发一轮 M-SEARCH 到 dst，取第一条 host 可路由的 LOCATION。
// 用无连接 socket 而不是 DialUDP：应答由网关从它的链路本地地址发回，连接态 socket
// 会把它当成「不是对端」丢掉。
func ssdpSearchV6From(ctx context.Context, src netip.Addr, dst netip.AddrPort) (*url.URL, netip.Addr, error) {
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: src.AsSlice(), Zone: src.Zone()})
	if err != nil {
		return nil, netip.Addr{}, err
	}
	defer conn.Close()

	host := netip.AddrPortFrom(dst.Addr().WithZone(""), dst.Port()).String() // HOST 头不带 zone
	buf := make([]byte, 2048)
	for _, st := range v6SearchTargets {
		if ctx.Err() != nil {
			return nil, netip.Addr{}, ctx.Err()
		}
		req := "M-SEARCH * HTTP/1.1\r\nHOST: " + host + "\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: " + st + "\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(req), net.UDPAddrFromAddrPort(dst)); err != nil {
			continue
		}
		deadline := time.Now().Add(ssdpV6Timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, netip.Addr{}, err
		}
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				break // 读超时：换下一个 ST
			}
			if loc, gw, ok := ssdpV6Location(buf[:n]); ok {
				return loc, gw, nil
			}
		}
	}
	return nil, netip.Addr{}, ErrUnsupported
}

// ssdpV6Location 解析一条 SSDP 应答，取 LOCATION 与其中的网关地址。只认可路由的 v6 host：
// 链路本地/ULA 的描述 URL 与「源地址是 GUA」的 SOAP 请求配不成对，拿到了也开不成洞。
func ssdpV6Location(b []byte) (*url.URL, netip.Addr, bool) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil {
		return nil, netip.Addr{}, false
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("LOCATION"))
	if err != nil || loc.Host == "" {
		return nil, netip.Addr{}, false
	}
	gw, err := netip.ParseAddr(loc.Hostname())
	if err != nil || !gw.Is6() || gw.Is4In6() || gw.IsLinkLocalUnicast() || gw.IsPrivate() {
		return nil, netip.Addr{}, false
	}
	return loc, gw, true
}

// ifaceGUA 取网卡上第一个 GUA。
func ifaceGUA(ifi net.Interface) (netip.Addr, bool) {
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		if ip = ip.Unmap(); isGlobalUnicastV6(ip) {
			return ip, true
		}
	}
	return netip.Addr{}, false
}

// upnp6NoSuchEntry WANIPv6FirewallControl 的 704 NoSuchEntry：UniqueID 已不存在。
const upnp6NoSuchEntry = 704

// upnpFaultCode 取 SOAP 错误码；不是 SOAP 错误返回 0。
func upnpFaultCode(err error) int {
	var fault *soap.SOAPFaultError
	if errors.As(err, &fault) {
		return fault.Detail.UPnPError.Errorcode
	}
	return 0
}

// upnp6Client 用 IGDv2 的 WANIPv6FirewallControl:1 开 v6 防火墙 pinhole。
// 续租优先 UpdatePinhole，报 704 unknown id 就重新 AddPinhole；ids 按 (协议, 端口, GUA)
// 存 UniqueID，供续租/删除认领。bound 是按被放行的 GUA 绑好出站源地址的客户端副本。
type upnp6Client struct {
	fw *internetgateway2.WANIPv6FirewallControl1

	mu    sync.Mutex
	bound map[netip.Addr]*internetgateway2.WANIPv6FirewallControl1
	ids   map[string]uint16
}

func newUPnP6Client(fw *internetgateway2.WANIPv6FirewallControl1) *upnp6Client {
	return &upnp6Client{
		fw:    fw,
		bound: make(map[netip.Addr]*internetgateway2.WANIPv6FirewallControl1),
		ids:   make(map[string]uint16),
	}
}

// clientFor 取一份把 SOAP 出站源地址绑到 gua 的客户端副本：secure_mode 的网关要求
// AddPinhole 的 InternalClient 就是请求的源地址，多 GUA（临时/隐私地址）时不绑定
// 会由内核任选源地址，于是给哪个地址开洞就不由我们说了算。副本按 gua 缓存，同一条
// 放行的 Add/Update/Delete 都从同一个源发出。
func (u *upnp6Client) clientFor(gua netip.Addr) *internetgateway2.WANIPv6FirewallControl1 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if c, ok := u.bound[gua]; ok {
		return c
	}
	cp := *u.fw
	sc := *cp.SOAPClient
	sc.HTTPClient.Transport = &http.Transport{DialContext: dialFrom(gua)}
	cp.SOAPClient = &sc
	u.bound[gua] = &cp
	return &cp
}

// dialFrom 把源地址绑到 addr 的拨号器；绑不上就退回不绑定（地址可能刚被系统撤销，
// 或描述 URL 与它不同族）——尽力而为，能不能开洞交给网关裁决。
func dialFrom(addr netip.Addr) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		bound := &net.Dialer{LocalAddr: &net.TCPAddr{IP: addr.AsSlice()}}
		if conn, err := bound.DialContext(ctx, network, address); err == nil {
			return conn, nil
		}
		var plain net.Dialer
		return plain.DialContext(ctx, network, address)
	}
}

// discoverUPnP6 建 v6 pinhole 客户端。loc 是已发现的设备描述 URL（v6 SSDP 的成果，
// 见 ssdpSearchV6），为空则自己搜一次。必须走 v6 URL：SOAP 从 v4 通道发过去时
// secure_mode 的网关看到的客户端 v6 地址是 ::，AddPinhole 一律 606。
func discoverUPnP6(ctx context.Context, loc *url.URL) (pinholeClient, error) {
	if loc == nil {
		var err error
		if loc, _, err = ssdpSearchV6(ctx); err != nil {
			return nil, err
		}
	}
	root, err := goupnp.DeviceByURLCtx(ctx, loc)
	if err != nil {
		return nil, err
	}
	clients, err := internetgateway2.NewWANIPv6FirewallControl1ClientsFromRootDevice(root, loc)
	if err != nil || len(clients) == 0 {
		return nil, ErrUnsupported // 设备以 IGDv1 身份宣告或 v6 功能被禁时就没有这个服务
	}
	return newUPnP6Client(clients[0]), nil
}

func (u *upnp6Client) Method() string { return "upnp6" }

func (u *upnp6Client) Open(ctx context.Context, proto string, port int, gua netip.Addr, lifetime time.Duration) (pinhole, error) {
	protoNum, err := protoNumber(proto)
	if err != nil {
		return pinhole{}, err
	}
	seconds := uint32(lifetime / time.Second)
	key := pinNonceKey(proto, port, gua)

	u.mu.Lock()
	id, have := u.ids[key]
	u.mu.Unlock()
	if have {
		// 续租：更新已有 pinhole 的租期，网关才不会每轮换 UniqueID、堆满 pinhole 空间。
		err := u.clientFor(gua).UpdatePinholeCtx(ctx, id, seconds)
		if err == nil {
			return pinhole{proto: proto, port: port, gua: gua, method: "upnp6", id: id, hasID: true}, nil
		}
		if upnpFaultCode(err) != upnp6NoSuchEntry {
			return pinhole{}, classifyUPnPError(err)
		}
		// 704：这条 pinhole 已被网关回收，落到下面重新 AddPinhole。
		u.mu.Lock()
		delete(u.ids, key)
		u.mu.Unlock()
	}

	// RemoteHost 空 + RemotePort 0 = 任意源；InternalClient 是本机 GUA、外部端口 == 内部端口。
	id, err = u.clientFor(gua).AddPinholeCtx(ctx, "", 0, gua.String(), uint16(port), uint16(protoNum), seconds)
	if err != nil {
		return pinhole{}, classifyUPnPError(err)
	}
	u.mu.Lock()
	u.ids[key] = id
	u.mu.Unlock()
	return pinhole{proto: proto, port: port, gua: gua, method: "upnp6", id: id, hasID: true}, nil
}

func (u *upnp6Client) Close(ctx context.Context, h pinhole) error {
	u.mu.Lock()
	delete(u.ids, pinNonceKey(h.proto, h.port, h.gua))
	u.mu.Unlock()
	err := u.clientFor(h.gua).DeletePinholeCtx(ctx, h.id)
	if err == nil || upnpFaultCode(err) == upnp6NoSuchEntry {
		return nil // 已不存在视为已删除
	}
	return classifyUPnPError(err)
}

// classifyUPnPError 把 SOAP 错误码映射成 portmap 的哨兵错误；不认识的错误码
// 原样包装并保留错误码与描述文字，供日志诊断。
func classifyUPnPError(err error) error {
	var fault *soap.SOAPFaultError
	if !errors.As(err, &fault) {
		return err
	}
	code := fault.Detail.UPnPError.Errorcode
	desc := fault.Detail.UPnPError.ErrorDescription
	switch code {
	case 606: // NotAuthorized
		return fmt.Errorf("%w: %s", ErrNotAuthorized, desc)
	case 718, 729: // ConflictInMappingEntry / ConflictWithOtherMechanisms
		return fmt.Errorf("%w: %s", ErrConflict, desc)
	case 725: // OnlyPermanentLeasesSupported
		return fmt.Errorf("%w: %s", ErrPermanentOnly, desc)
	case 501: // ActionFailed（网关资源不足/后端故障）
		return fmt.Errorf("%w: %s", ErrNoResources, desc)
	default:
		return fmt.Errorf("upnp 错误码 %d: %s", code, desc)
	}
}
