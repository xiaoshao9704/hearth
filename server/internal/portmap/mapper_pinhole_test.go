package portmap

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type pinCall struct {
	proto string
	port  int
	gua   netip.Addr
}

// fakePinholeClient 可注入结果的假 v6 pinhole 客户端。
type fakePinholeClient struct {
	name    string
	openErr error

	mu     sync.Mutex
	opens  []pinCall
	closes []pinCall
}

func (f *fakePinholeClient) Method() string {
	if f.name == "" {
		return "pcp6"
	}
	return f.name
}

func (f *fakePinholeClient) Open(_ context.Context, proto string, port int, gua netip.Addr, _ time.Duration) (pinhole, error) {
	f.mu.Lock()
	f.opens = append(f.opens, pinCall{proto, port, gua})
	f.mu.Unlock()
	if f.openErr != nil {
		return pinhole{}, f.openErr
	}
	return pinhole{proto: proto, port: port, gua: gua, method: f.Method()}, nil
}

func (f *fakePinholeClient) Close(_ context.Context, h pinhole) error {
	f.mu.Lock()
	f.closes = append(f.closes, pinCall{h.proto, h.port, h.gua})
	f.mu.Unlock()
	return nil
}

func (f *fakePinholeClient) openCalls() []pinCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pinCall(nil), f.opens...)
}

func (f *fakePinholeClient) closeCalls() []pinCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pinCall(nil), f.closes...)
}

var (
	gua1 = netip.MustParseAddr("2001:db8::1")
	gua2 = netip.MustParseAddr("2001:db8::2")
)

// mapperWithPinhole 在 testMapper 的基础上注入 v6：一组固定 GUA + 假 pinhole 客户端。
func mapperWithPinhole(v4 client, ph *fakePinholeClient, gua6 func() []netip.Addr) *Mapper {
	m := testMapper(v4)
	m.gua6 = gua6
	m.gateway6 = func() (netip.Addr, bool) { return netip.MustParseAddr("2001:db8::ffff"), true }
	m.newPCP6 = func(netip.AddrPort) pinholeClient { return ph }
	m.upnp6 = func(context.Context) (pinholeClient, error) { return nil, ErrUnsupported }
	return m
}

func okV4() *fakeClient {
	return &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "198.51.100.9"), nil
	}}
}

func TestMapperPinholeOpensPerGUAxWant(t *testing.T) {
	ph := &fakePinholeClient{}
	m := mapperWithPinhole(okV4(), ph, func() []netip.Addr { return []netip.Addr{gua1, gua2} })

	runRounds(m, staticWants(httpWant, mediaWant), 1)

	// 2 个 want × 2 个 GUA = 4 条放行。
	if got := len(ph.openCalls()); got != 4 {
		t.Fatalf("Open 调用数 = %d，应为 want×GUA = 4", got)
	}
	st := m.Snapshot()
	if len(st.Pinholes) != 4 {
		t.Fatalf("Status.Pinholes = %d 条，应为 4：%+v", len(st.Pinholes), st.Pinholes)
	}
	for _, p := range st.Pinholes {
		if p.Method != "pcp6" {
			t.Fatalf("放行方法 = %q", p.Method)
		}
	}
	// v6 pinhole 不进 Lookup（那是 v4 srflx 宣告用的）。
	if _, ok := m.Lookup("udp", 47700); !ok {
		t.Fatal("v4 映射仍应能 Lookup")
	}
}

func TestMapperPinholeCoexistsWithV4(t *testing.T) {
	ph := &fakePinholeClient{}
	m := mapperWithPinhole(okV4(), ph, func() []netip.Addr { return []netip.Addr{gua1} })

	runRounds(m, staticWants(httpWant), 1)

	st := m.Snapshot()
	if st.Diagnosis != DiagOK {
		t.Fatalf("v4 诊断 = %s，v6 不应影响 v4", st.Diagnosis)
	}
	if len(st.Mappings) != 1 {
		t.Fatalf("v4 映射 = %d 条", len(st.Mappings))
	}
	if len(st.Pinholes) != 1 || st.Pinholes[0].GUA != gua1 {
		t.Fatalf("v6 放行 = %+v", st.Pinholes)
	}
	if st.V6Detail == "" {
		t.Fatal("v6 侧应有诊断文案")
	}
}

func TestMapperPinholeGUAChangeRevokes(t *testing.T) {
	ph := &fakePinholeClient{}
	round := 0
	m := mapperWithPinhole(okV4(), ph, func() []netip.Addr {
		round++
		if round == 1 {
			return []netip.Addr{gua1, gua2}
		}
		return []netip.Addr{gua1} // gua2 被撤下（临时地址轮换）
	})

	runRounds(m, staticWants(httpWant), 2)

	// gua2 上那条放行应被撤销。
	var closedGUA2 int
	for _, c := range ph.closeCalls() {
		if c.gua == gua2 {
			closedGUA2++
		}
	}
	if closedGUA2 != 1 {
		t.Fatalf("gua2 撤下后应撤销其放行一次，实际 %d", closedGUA2)
	}
	st := m.Snapshot()
	if len(st.Pinholes) != 1 || st.Pinholes[0].GUA != gua1 {
		t.Fatalf("剩余放行应只有 gua1：%+v", st.Pinholes)
	}
}

func TestMapperPinholeDiscoveryFailureIsSilent(t *testing.T) {
	ph := &fakePinholeClient{}
	m := mapperWithPinhole(okV4(), ph, func() []netip.Addr { return []netip.Addr{gua1} })
	// 两条 v6 途径都不可用：没有网关 GUA + UPnP 不支持。
	m.gateway6 = func() (netip.Addr, bool) { return netip.Addr{}, false }
	m.upnp6 = func(context.Context) (pinholeClient, error) { return nil, ErrUnsupported }

	runRounds(m, staticWants(httpWant), 1)

	st := m.Snapshot()
	// v4 照常成立，v6 失败静默。
	if st.Diagnosis != DiagOK || len(st.Mappings) != 1 {
		t.Fatalf("v6 发现失败不该影响 v4：%+v", st)
	}
	if len(st.Pinholes) != 0 {
		t.Fatalf("v6 无放行，Pinholes 应为空：%+v", st.Pinholes)
	}
	if st.V6Detail != detailNoGateway6 {
		t.Fatalf("v6 诊断 = %q", st.V6Detail)
	}
	if len(ph.openCalls()) != 0 {
		t.Fatal("发现不到 v6 途径时不该调假客户端的 Open")
	}
}

func TestMapperPinholeNoGUAIsSilent(t *testing.T) {
	ph := &fakePinholeClient{}
	m := mapperWithPinhole(okV4(), ph, func() []netip.Addr { return nil })

	runRounds(m, staticWants(httpWant), 1)

	st := m.Snapshot()
	if st.Diagnosis != DiagOK {
		t.Fatalf("没有 GUA 时 v4 照常：%s", st.Diagnosis)
	}
	if len(st.Pinholes) != 0 || st.V6Detail != detailNoV6 {
		t.Fatalf("没有 GUA 应给 detailNoV6：%+v / %q", st.Pinholes, st.V6Detail)
	}
	if len(ph.openCalls()) != 0 {
		t.Fatal("没有 GUA 不该发起任何放行")
	}
}

func TestMapperPinholeCloseRevokesAll(t *testing.T) {
	ph := &fakePinholeClient{}
	m := mapperWithPinhole(okV4(), ph, func() []netip.Addr { return []netip.Addr{gua1, gua2} })

	runRounds(m, staticWants(httpWant), 1)
	m.Close(context.Background())

	if got := len(ph.closeCalls()); got != 2 {
		t.Fatalf("Close 应撤销全部放行（2 条），实际 %d", got)
	}
	if st := m.Snapshot(); len(st.Pinholes) != 0 {
		t.Fatalf("Close 后 Pinholes 应为空：%+v", st.Pinholes)
	}
}

// TestMapperPinholeRenewIdempotent 续租轮对每条放行再 Open 一次（幂等），不重复 Close。
func TestMapperPinholeRenewIdempotent(t *testing.T) {
	ph := &fakePinholeClient{}
	m := mapperWithPinhole(okV4(), ph, func() []netip.Addr { return []netip.Addr{gua1} })

	runRounds(m, staticWants(httpWant), 2)

	if got := len(ph.openCalls()); got != 2 {
		t.Fatalf("两轮应各 Open 一次（续租幂等），实际 %d", got)
	}
	if got := len(ph.closeCalls()); got != 0 {
		t.Fatalf("续租不该 Close，实际 %d", got)
	}
}
