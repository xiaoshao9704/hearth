package portmap

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// fakeIGDv6 是跑在 httptest 之上的最小 IGD，只服务 WANIPv6FirewallControl:1 的设备描述与
// AddPinhole/DeletePinhole/UpdatePinhole 三个动作，验证 upnp.go 与 goupnp 生成代码之间的往返。
type fakeIGDv6 struct {
	mu         sync.Mutex
	srcIPs     []string // 每次 SOAP 请求的源地址，验证出站源绑定用
	adds       []pinholeAdd
	dels       []uint16
	updates    []pinholeUpdate
	nextID     uint16
	failUpdate bool // 下一次 UpdatePinhole 回 704 NoSuchEntry（模拟 pinhole 被网关回收）
	failDelete bool // 下一次 DeletePinhole 回 704（验证「已不存在视为成功」）
}

type pinholeAdd struct {
	internalClient string
	internalPort   int
	protocol       int
	remoteHost     string
	remotePort     int
	lease          int
}

type pinholeUpdate struct {
	id    int
	lease int
}

const wanIP6FCNS = "urn:schemas-upnp-org:service:WANIPv6FirewallControl:1"

func (f *fakeIGDv6) handler() http.HandlerFunc {
	const deviceXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:2</deviceType>
    <friendlyName>fake igd v6</friendlyName>
    <manufacturer>hearth-test</manufacturer>
    <modelName>fake</modelName>
    <UDN>uuid:fake-igd-6</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANIPv6FirewallControl:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:WANIPv6FwCtrl1</serviceId>
        <controlURL>/ctl6</controlURL>
        <eventSubURL>/evt6</eventSubURL>
        <SCPDURL>/scpd6.xml</SCPDURL>
      </service>
    </serviceList>
  </device>
</root>`

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/desc.xml":
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(deviceXML))
		case r.Method == http.MethodPost && r.URL.Path == "/ctl6":
			f.serveSOAP(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeIGDv6) serveSOAP(w http.ResponseWriter, r *http.Request) {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		f.mu.Lock()
		f.srcIPs = append(f.srcIPs, host)
		f.mu.Unlock()
	}
	action := r.Header.Get("SOAPACTION")
	body, _ := io.ReadAll(r.Body)
	switch {
	case containsAction(action, "AddPinhole"):
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.adds = append(f.adds, pinholeAdd{
			internalClient: extractStringTag(body, "InternalClient"),
			internalPort:   extractIntTag(body, "InternalPort"),
			protocol:       extractIntTag(body, "Protocol"),
			remoteHost:     extractStringTag(body, "RemoteHost"),
			remotePort:     extractIntTag(body, "RemotePort"),
			lease:          extractIntTag(body, "LeaseTime"),
		})
		f.mu.Unlock()
		writeSOAPResult(w, wanIP6FCNS, "AddPinhole",
			"<UniqueID>"+strconv.Itoa(int(id))+"</UniqueID>")
	case containsAction(action, "UpdatePinhole"):
		f.mu.Lock()
		fail := f.failUpdate
		f.failUpdate = false
		f.updates = append(f.updates, pinholeUpdate{
			id:    extractIntTag(body, "UniqueID"),
			lease: extractIntTag(body, "NewLeaseTime"),
		})
		f.mu.Unlock()
		if fail {
			writeSOAPFault(w, upnp6NoSuchEntry, "NoSuchEntry")
			return
		}
		writeSOAPResult(w, wanIP6FCNS, "UpdatePinhole", "")
	case containsAction(action, "DeletePinhole"):
		f.mu.Lock()
		fail := f.failDelete
		f.failDelete = false
		f.dels = append(f.dels, uint16(extractIntTag(body, "UniqueID")))
		f.mu.Unlock()
		if fail {
			writeSOAPFault(w, upnp6NoSuchEntry, "NoSuchEntry")
			return
		}
		writeSOAPResult(w, wanIP6FCNS, "DeletePinhole", "")
	default:
		http.Error(w, "unknown action: "+action, http.StatusNotImplemented)
	}
}

func containsAction(soapAction, name string) bool {
	return strings.Contains(soapAction, name)
}

func TestUPnP6PinholeRoundTrip(t *testing.T) {
	fake := &fakeIGDv6{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	descURL, err := url.Parse(srv.URL + "/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	clients, err := internetgateway2.NewWANIPv6FirewallControl1ClientsByURLCtx(ctx, descURL)
	if err != nil {
		t.Fatalf("NewWANIPv6FirewallControl1ClientsByURLCtx: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("got %d WANIPv6FirewallControl1 clients, want 1", len(clients))
	}
	c := newUPnP6Client(clients[0])

	// 首次 Open：AddPinhole，验证参数（InternalClient=GUA、端口、协议号、租期）。
	h, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !h.hasID || h.id != 1 {
		t.Fatalf("首条 pinhole 应解析出 UniqueID=1，得到 %+v", h)
	}
	fake.mu.Lock()
	if len(fake.adds) != 1 {
		fake.mu.Unlock()
		t.Fatalf("AddPinhole 调用数 = %d", len(fake.adds))
	}
	add := fake.adds[0]
	fake.mu.Unlock()
	if add.internalClient != testGUA.String() || add.internalPort != 47700 || add.protocol != 17 {
		t.Fatalf("AddPinhole 参数不对: %+v", add)
	}
	if add.remoteHost != "" || add.remotePort != 0 {
		t.Fatalf("RemoteHost/RemotePort 应为任意源（空/0）: %+v", add)
	}
	if add.lease != 3600 {
		t.Fatalf("租期 = %d，应为 3600", add.lease)
	}

	// 续租：走 UpdatePinhole，不再新增 AddPinhole。
	if _, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour); err != nil {
		t.Fatalf("续租: %v", err)
	}
	fake.mu.Lock()
	if len(fake.adds) != 1 || len(fake.updates) != 1 || fake.updates[0].id != 1 {
		fake.mu.Unlock()
		t.Fatalf("续租应走 UpdatePinhole(1)：adds=%+v updates=%+v", fake.adds, fake.updates)
	}
	fake.mu.Unlock()

	// UpdatePinhole 报 704 unknown id → 重新 AddPinhole，拿到新的 UniqueID。
	fake.mu.Lock()
	fake.failUpdate = true
	fake.mu.Unlock()
	h2, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("704 后应重新 AddPinhole: %v", err)
	}
	if h2.id != 2 {
		t.Fatalf("重加后 UniqueID = %d，应为 2", h2.id)
	}
	fake.mu.Lock()
	if len(fake.adds) != 2 {
		fake.mu.Unlock()
		t.Fatalf("704 后应再 AddPinhole 一次，adds=%+v", fake.adds)
	}
	fake.mu.Unlock()

	// Close：DeletePinhole(当前 id)。
	if err := c.Close(ctx, h2); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fake.mu.Lock()
	if len(fake.dels) != 1 || fake.dels[0] != 2 {
		fake.mu.Unlock()
		t.Fatalf("DeletePinhole 调用 = %+v，应删 id=2", fake.dels)
	}
	fake.mu.Unlock()

	// 再删一条并让网关回 704：已不存在视为成功。
	h3, err := c.Open(ctx, "udp", 8080, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("Open 第三条: %v", err)
	}
	fake.mu.Lock()
	fake.failDelete = true
	fake.mu.Unlock()
	if err := c.Close(ctx, h3); err != nil {
		t.Fatalf("Close 遇 704 应视为成功，得到 %v", err)
	}
}

func TestDiscoverUPnP6NoDevice(t *testing.T) {
	// 没有真实 IGD 时（回环网络里没有多播 IGD 应答），发现应在超时后返回 ErrUnsupported，
	// 不 panic、不返回其它错误——让 Mapper 安静退避。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := discoverUPnP6(ctx, nil)
	if err == nil {
		t.Skip("本机网络里存在真实的 WANIPv6FirewallControl IGD，跳过负路径断言")
	}
}

// TestUPnP6BindsSourceToGUA SOAP 请求必须从被放行的 GUA 发出：secure_mode 的网关要求
// AddPinhole 的 InternalClient 就是请求源地址，从 v4 通道发过去时它看到的是 ::，回 606。
// 假 IGD 监听回环 v6，用 ::1 当这条放行的 GUA（回环里只有它能绑）。
func TestUPnP6BindsSourceToGUA(t *testing.T) {
	loop := netip.MustParseAddr("::1")
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("本机没有可用的 IPv6 回环：%v", err)
	}
	fake := &fakeIGDv6{}
	srv := httptest.NewUnstartedServer(fake.handler())
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	descURL, err := url.Parse(srv.URL + "/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	clients, err := internetgateway2.NewWANIPv6FirewallControl1ClientsByURLCtx(ctx, descURL)
	if err != nil {
		t.Fatalf("NewWANIPv6FirewallControl1ClientsByURLCtx: %v", err)
	}
	c := newUPnP6Client(clients[0])
	h, err := c.Open(ctx, "udp", 47700, loop, time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Close(ctx, h); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fake.mu.Lock()
	srcs := append([]string(nil), fake.srcIPs...)
	adds := append([]pinholeAdd(nil), fake.adds...)
	fake.mu.Unlock()
	if len(srcs) != 2 {
		t.Fatalf("SOAP 请求数 = %d（Add + Delete）", len(srcs))
	}
	for i, src := range srcs {
		if got, err := netip.ParseAddr(src); err != nil || got != loop {
			t.Fatalf("第 %d 次 SOAP 的源地址 = %q，应为被放行的 GUA %v", i+1, src, loop)
		}
	}
	if len(adds) != 1 || adds[0].internalClient != loop.String() {
		t.Fatalf("InternalClient 应与源地址一致：%+v", adds)
	}
}

// startFakeSSDP6 回环上的假 SSDP 应答器：收到任何报文都回同一条应答。
func startFakeSSDP6(t *testing.T, resp string) netip.AddrPort {
	t.Helper()
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("本机没有可用的 IPv6 回环：%v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n > 0 {
				conn.WriteToUDP([]byte(resp), from)
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

func ssdpResp(location string) string {
	return "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:2\r\n" +
		"USN: uuid:fake-igd-6::urn:schemas-upnp-org:device:InternetGatewayDevice:2\r\n" +
		"LOCATION: " + location + "\r\n\r\n"
}

// TestSSDPSearchV6 v6 SSDP 是网关 GUA 的权威来源：应答里 LOCATION 的 host 就是它。
// 只认可路由的 host——链路本地的描述 URL 与「源是 GUA」的 SOAP 请求配不成对。
func TestSSDPSearchV6(t *testing.T) {
	old := ssdpV6Timeout
	ssdpV6Timeout = 200 * time.Millisecond // 两个 ST 串行，负路径要等满两轮
	t.Cleanup(func() { ssdpV6Timeout = old })
	loop := netip.MustParseAddr("::1")

	t.Run("取 LOCATION 的 host 当网关地址", func(t *testing.T) {
		dst := startFakeSSDP6(t, ssdpResp("http://[::1]:5000/rootDesc.xml"))
		loc, gw, err := ssdpSearchV6From(context.Background(), loop, dst)
		if err != nil {
			t.Fatalf("ssdpSearchV6From: %v", err)
		}
		if gw != loop {
			t.Fatalf("网关地址 = %v，应取自 LOCATION 的 host", gw)
		}
		if loc.String() != "http://[::1]:5000/rootDesc.xml" {
			t.Fatalf("描述 URL = %q", loc)
		}
	})

	t.Run("跳过链路本地 LOCATION", func(t *testing.T) {
		dst := startFakeSSDP6(t, ssdpResp("http://[fe80::1]:5000/rootDesc.xml"))
		if _, _, err := ssdpSearchV6From(context.Background(), loop, dst); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("链路本地 LOCATION 应被跳过（ErrUnsupported），得到 %v", err)
		}
	})
}
