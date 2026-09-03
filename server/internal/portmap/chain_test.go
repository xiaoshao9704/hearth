package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// order 记录跨两个假网关的报文顺序，用来验证 Unmap 是从最外层往里删的。
type order struct {
	mu   sync.Mutex
	seen []string
}

func (o *order) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, s)
}

func (o *order) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seen...)
}

// pcpEcho 按请求回一条成功的 PCP 响应：外部端口回显请求里的建议值，外部地址取 ext。
// 顺带把「map / del」记进 ord（lifetime 0 就是删除）。
func pcpEcho(ord *order, label, ext string) func([]byte) []byte {
	return func(req []byte) []byte {
		lifetime := binary.BigEndian.Uint32(req[4:8])
		kind := "map"
		if lifetime == 0 {
			kind = "del"
		}
		ord.add(label + "-" + kind)
		return pcpMapResp(req, 0, lifetime, binary.BigEndian.Uint16(req[42:44]), ext)
	}
}

func testChain(first client, discover hopDiscoverFunc) *chainClient {
	c := newChain(first, netip.MustParseAddr("192.168.0.1"), time.Now, func(string, ...any) {})
	c.discover = discover
	return c
}

// TestChainSecondHopRequest 走两个回环 PCP 假网关，验证级联的报文级约定：
// 第二跳的 client IP 是第一跳给出的外部地址、内部端口是第一跳的外部端口、
// 首选外部端口仍是同一个，返回的是最外层的结果，撤销从外往里。
func TestChainSecondHopRequest(t *testing.T) {
	var ord order
	inner := startFakeGW(t, pcpEcho(&ord, "inner", "10.0.0.2"))     // 第一跳：外部地址仍是私网
	outer := startFakeGW(t, pcpEcho(&ord, "outer", "198.51.100.9")) // 第二跳：拿到公网地址

	upstreamGW := netip.MustParseAddr("10.0.0.1")
	var discovered int
	c := testChain(newPCPClient(inner.addr()), func(ctx context.Context, x netip.Addr, w Want, external int, lifetime time.Duration) (client, netip.Addr, Mapping, error) {
		discovered++
		cl := newPCPClientAs(outer.addr(), x)
		mp, err := cl.Map(ctx, w, external, lifetime)
		return cl, upstreamGW, mp, err
	})

	ctx := context.Background()
	mp, err := c.Map(ctx, udpWant, udpWant.Port, time.Hour)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if mp.Internal != udpWant.Port || mp.External != udpWant.Port || mp.ExternalIP.String() != "198.51.100.9" {
		t.Fatalf("应返回最外层的结果（内部端口不变）: %+v", mp)
	}

	reqs := outer.requests()
	if len(reqs) != 1 {
		t.Fatalf("第二跳请求数 = %d", len(reqs))
	}
	req := reqs[0]
	if got := netip.AddrFrom16([16]byte(req[8:24])).Unmap(); got.String() != "10.0.0.2" {
		t.Fatalf("第二跳的 client IP = %v，应是第一跳给出的外部地址", got)
	}
	if got := binary.BigEndian.Uint16(req[40:42]); int(got) != udpWant.Port {
		t.Fatalf("第二跳的内部端口 = %d，应等于第一跳的外部端口 %d", got, udpWant.Port)
	}
	if got := binary.BigEndian.Uint16(req[42:44]); int(got) != udpWant.Port {
		t.Fatalf("第二跳的建议外部端口 = %d，应与第一跳同端口", got)
	}

	hops := c.hopList()
	if len(hops) != 2 || hops[0].Gateway.String() != "192.168.0.1" || hops[1].Gateway != upstreamGW {
		t.Fatalf("跳列表 = %+v", hops)
	}
	if hops[0].ExternalIP.String() != "10.0.0.2" || hops[1].ExternalIP.String() != "198.51.100.9" {
		t.Fatalf("各跳外部地址 = %+v", hops)
	}

	// 续租：已有的跳直接重发，不再发现。
	if _, err := c.Map(ctx, udpWant, udpWant.Port, time.Hour); err != nil {
		t.Fatalf("续租: %v", err)
	}
	if discovered != 1 {
		t.Fatalf("发现了 %d 次上游，已建立的跳不该重新发现", discovered)
	}

	if err := c.Unmap(ctx, udpWant, mp.External); err != nil {
		t.Fatalf("Unmap: %v", err)
	}
	got := ord.list()
	want := []string{"inner-map", "outer-map", "inner-map", "outer-map", "outer-del", "inner-del"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("报文顺序 = %v，撤销必须从最外层往里", got)
	}
}

// TestChainStopsAtPublic 第一跳就拿到公网地址时不该去找上游。
func TestChainStopsAtPublic(t *testing.T) {
	inner := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "198.51.100.9"), nil
	}}
	c := testChain(inner, func(context.Context, netip.Addr, Want, int, time.Duration) (client, netip.Addr, Mapping, error) {
		t.Fatal("外部地址已是公网，不该再找上游")
		return nil, netip.Addr{}, Mapping{}, nil
	})

	if _, err := c.Map(context.Background(), httpWant, httpWant.Port, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}
	if hops := c.hopList(); len(hops) != 1 {
		t.Fatalf("跳数 = %d", len(hops))
	}
	if c.stallReason() != "" {
		t.Fatalf("到公网即止不算停链: %q", c.stallReason())
	}
}

// TestChainMaxHops 每一跳都只给私网地址时，链最多 3 跳（含默认网关）。
func TestChainMaxHops(t *testing.T) {
	inner := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "10.0.0.2"), nil
	}}
	n := 0
	c := testChain(inner, func(_ context.Context, x netip.Addr, w Want, external int, _ time.Duration) (client, netip.Addr, Mapping, error) {
		n++
		gw := netip.MustParseAddr(fmt.Sprintf("10.%d.0.1", n))
		up := &fakeClient{}
		return up, gw, okMap(w, external, fmt.Sprintf("10.%d.0.2", n)), nil
	})

	if _, err := c.Map(context.Background(), httpWant, httpWant.Port, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}
	if hops := c.hopList(); len(hops) != maxHops {
		t.Fatalf("跳数 = %d，上限是 %d", len(hops), maxHops)
	}
	if n != maxHops-1 {
		t.Fatalf("发现了 %d 次上游", n)
	}
}

// TestChainNegativeCache 上游发现失败后一段时间内不重试，超过 maxBackoff 才再试一次。
func TestChainNegativeCache(t *testing.T) {
	inner := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "10.0.0.2"), nil
	}}
	n := 0
	c := testChain(inner, func(context.Context, netip.Addr, Want, int, time.Duration) (client, netip.Addr, Mapping, error) {
		n++
		return nil, netip.Addr{}, Mapping{}, ErrUnsupported
	})
	base := time.Now()
	c.now = func() time.Time { return base }

	ctx := context.Background()
	for range 3 {
		if _, err := c.Map(ctx, httpWant, httpWant.Port, time.Hour); err != nil {
			t.Fatalf("Map: %v", err)
		}
	}
	if n != 1 {
		t.Fatalf("发现了 %d 次，负缓存内不该重复发现", n)
	}
	if !strings.Contains(c.stallReason(), "未发现") {
		t.Fatalf("停链原因 = %q", c.stallReason())
	}

	c.now = func() time.Time { return base.Add(maxBackoff + time.Second) }
	if _, err := c.Map(ctx, httpWant, httpWant.Port, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}
	if n != 2 {
		t.Fatalf("过了负缓存期应再试一次，实际发现 %d 次", n)
	}
}

// TestChainRejectsPublicUpstream 发现结果落在公网地址上时拒绝接上：绝不向公网地址开洞。
func TestChainRejectsPublicUpstream(t *testing.T) {
	inner := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "10.0.0.2"), nil
	}}
	c := testChain(inner, func(_ context.Context, x netip.Addr, w Want, external int, _ time.Duration) (client, netip.Addr, Mapping, error) {
		return &fakeClient{}, netip.MustParseAddr("198.51.100.1"), okMap(w, external, "198.51.100.9"), nil
	})

	mp, err := c.Map(context.Background(), httpWant, httpWant.Port, time.Hour)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(c.hopList()) != 1 || mp.ExternalIP.String() != "10.0.0.2" {
		t.Fatalf("公网候选应被拒绝，链停在第一跳: hops=%+v mp=%+v", c.hopList(), mp)
	}
	if !strings.Contains(c.stallReason(), "公网") {
		t.Fatalf("停链原因 = %q", c.stallReason())
	}
}

// TestChainStopsOnLoop 上游地址绕回链上已有的一跳时立即停。
func TestChainStopsOnLoop(t *testing.T) {
	// 外部地址就是本机默认网关：再往上就是绕回来了。
	inner := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "192.168.0.1"), nil
	}}
	c := testChain(inner, func(context.Context, netip.Addr, Want, int, time.Duration) (client, netip.Addr, Mapping, error) {
		t.Fatal("外部地址已是链上的网关，不该再发现")
		return nil, netip.Addr{}, Mapping{}, nil
	})
	if _, err := c.Map(context.Background(), httpWant, httpWant.Port, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(c.hopList()) != 1 || !strings.Contains(c.stallReason(), "环路") {
		t.Fatalf("hops=%d stall=%q", len(c.hopList()), c.stallReason())
	}

	// 发现回来的网关地址与已有跳重合，同样算环路。
	inner2 := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "10.0.0.2"), nil
	}}
	c2 := testChain(inner2, func(_ context.Context, x netip.Addr, w Want, external int, _ time.Duration) (client, netip.Addr, Mapping, error) {
		return &fakeClient{}, netip.MustParseAddr("192.168.0.1"), okMap(w, external, "10.1.0.2"), nil
	})
	if _, err := c2.Map(context.Background(), httpWant, httpWant.Port, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(c2.hopList()) != 1 || !strings.Contains(c2.stallReason(), "环路") {
		t.Fatalf("hops=%d stall=%q", len(c2.hopList()), c2.stallReason())
	}
}

// TestChainUpstreamRejectedFallsBackToOneHop 上游拒绝申请时链停在第一跳，
// Mapper 照旧给出 upstream_nat 诊断，并在指引前说明尝试过上游、原因是什么。
func TestChainUpstreamRejectedFallsBackToOneHop(t *testing.T) {
	inner := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "10.0.0.2"), nil
	}}
	m := testMapper(inner)
	m.hopFound = func(context.Context, netip.Addr, Want, int, time.Duration) (client, netip.Addr, Mapping, error) {
		return nil, netip.Addr{}, Mapping{}, ErrNotAuthorized
	}

	runRounds(m, staticWants(httpWant), 1)

	st := m.Snapshot()
	if st.Diagnosis != DiagUpstreamNAT {
		t.Fatalf("诊断 = %s", st.Diagnosis)
	}
	if len(st.Hops) != 1 {
		t.Fatalf("链应停在第一跳: %+v", st.Hops)
	}
	if !strings.Contains(st.Detail, "已尝试向上游网关申请但未成功") || !strings.Contains(st.Detail, "拒绝") {
		t.Fatalf("诊断文案应说明尝试过上游及原因: %s", st.Detail)
	}
	// 指引里的地址是最外层已到达那一跳的外部地址。
	if !strings.Contains(st.Detail, "10.0.0.2") {
		t.Fatalf("诊断文案 = %s", st.Detail)
	}
}

func TestUpstreamCandidate(t *testing.T) {
	cases := []struct {
		x    string
		want string // 空 = 应拒绝
	}{
		{"10.0.0.2", "10.0.0.1"},
		{"10.0.0.1", "10.0.0.254"},
		{"100.64.1.9", "100.64.1.1"}, // CGNAT 也要往上试
		{"198.51.100.9", ""},         // 公网地址不试
		{"2001:db8::1", ""},          // v4 才有这套启发
	}
	for _, tc := range cases {
		got, ok := upstreamCandidate(netip.MustParseAddr(tc.x))
		if tc.want == "" {
			if ok {
				t.Fatalf("upstreamCandidate(%s) = %v，应拒绝", tc.x, got)
			}
			continue
		}
		if !ok || got.String() != tc.want {
			t.Fatalf("upstreamCandidate(%s) = %v %v，期望 %s", tc.x, got, ok, tc.want)
		}
	}
}

// fakeSSDP 回环上的假 SSDP 应答器：收到任何 M-SEARCH 都回一个指向 loc 的 LOCATION。
func startFakeSSDP(t *testing.T, loc string) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听假 SSDP 失败: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	resp := "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nEXT:\r\n" +
		"LOCATION: " + loc + "\r\nSERVER: fake/1.0 UPnP/1.1 hearth-test/1\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n" +
		"USN: uuid:fake-igd-1::urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n" +
		"Content-Length: 0\r\n\r\n"
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if strings.HasPrefix(string(buf[:n]), "M-SEARCH") {
				conn.WriteToUDP([]byte(resp), from)
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// TestDiscoverUPnPAt 单播发现：假 SSDP 应答器给出设备描述 URL，接上 upnp_test.go 里的
// 假 IGD，验证发现能建出客户端，且 internalClient 覆盖后进了 NewInternalClient。
func TestDiscoverUPnPAt(t *testing.T) {
	fake := &fakeIGDv1{extIP: "203.0.113.5"} // TEST-NET-3，仅测试占位
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	port := startFakeSSDP(t, srv.URL+"/desc.xml")
	old := ssdpPort
	ssdpPort = port
	t.Cleanup(func() { ssdpPort = old })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uc, err := discoverUPnPAt(ctx, netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatalf("discoverUPnPAt: %v", err)
	}

	uc.internalClient = netip.MustParseAddr("10.0.0.2")
	want := Want{Proto: "udp", Port: 47700, Desc: "hearth test"}
	if _, err := uc.Map(ctx, want, 47700, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.added) != 1 || fake.added[0].internalClient != "10.0.0.2" {
		t.Fatalf("NewInternalClient 应是覆盖值，实际 %+v", fake.added)
	}
}

func TestDiscoverUPnPAtNoAnswer(t *testing.T) {
	// 没有应答器：单播搜索超时后应给 ErrUnsupported，交给上层退回单跳。
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close() // 端口空着，报文直接被丢弃

	old := ssdpPort
	ssdpPort = port
	t.Cleanup(func() { ssdpPort = old })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := discoverUPnPAt(ctx, netip.MustParseAddr("127.0.0.1")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("discoverUPnPAt = %v，期望 ErrUnsupported", err)
	}
}
