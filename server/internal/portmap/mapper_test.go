package portmap

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct {
	w        Want
	external int
	lifetime time.Duration
}

// fakeClient 可注入结果序列的假网关客户端。
type fakeClient struct {
	name    string
	mapFn   func(w Want, external int, lifetime time.Duration) (Mapping, error)
	unmapFn func(w Want, external int) error

	mu     sync.Mutex
	maps   []call
	unmaps []call
}

func (f *fakeClient) Method() string {
	if f.name == "" {
		return "pcp"
	}
	return f.name
}

func (f *fakeClient) Map(_ context.Context, w Want, external int, lifetime time.Duration) (Mapping, error) {
	f.mu.Lock()
	f.maps = append(f.maps, call{w, external, lifetime})
	f.mu.Unlock()
	return f.mapFn(w, external, lifetime)
}

func (f *fakeClient) Unmap(_ context.Context, w Want, external int) error {
	f.mu.Lock()
	f.unmaps = append(f.unmaps, call{w: w, external: external})
	f.mu.Unlock()
	if f.unmapFn != nil {
		return f.unmapFn(w, external)
	}
	return nil
}

func (f *fakeClient) mapCalls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.maps...)
}

func (f *fakeClient) unmapCalls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.unmaps...)
}

// okMap 一条成功的映射；external 为 0 表示网关同意了同端口。
func okMap(w Want, external int, ip string) Mapping {
	if external == 0 {
		external = w.Port
	}
	return Mapping{
		Proto:      w.Proto,
		Internal:   w.Port,
		External:   external,
		ExternalIP: netip.MustParseAddr(ip),
		ExpiresAt:  time.Now().Add(leaseDuration),
	}
}

func testMapper(c client) *Mapper {
	m := New()
	m.gateway = func() (netip.Addr, error) { return netip.MustParseAddr("192.168.0.1"), nil }
	m.newPCP = func(netip.AddrPort) client { return c }
	m.upnp = func(context.Context) (client, error) { return nil, ErrUnsupported }
	// 默认不去找上游：级联要发真报文，单测按需再注入（见 chain_test.go）。
	m.hopFound = func(context.Context, netip.Addr, Want, int, time.Duration) (client, netip.Addr, Mapping, error) {
		return nil, netip.Addr{}, Mapping{}, ErrUnsupported
	}
	// v6 pinhole 默认关掉：不去读真实网卡地址，v4 用例才有确定的时序。需要验 v6 的用例
	// 单独注入 gua6/gateway6/newPCP6/upnp6（见 pinhole 相关测试）。
	m.gua6 = func() []netip.Addr { return nil }
	m.logf = func(string, ...any) {}
	return m
}

// runRounds 跑 n 轮后让 Run 退出，返回每轮请求的等待时长。
func runRounds(m *Mapper, wants func(context.Context) []Want, n int) []time.Duration {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	m.sleep = func(_ context.Context, d time.Duration) bool {
		delays = append(delays, d)
		return len(delays) < n
	}
	m.Run(ctx, wants)
	return delays
}

func staticWants(ws ...Want) func(context.Context) []Want {
	return func(context.Context) []Want { return ws }
}

func TestMapperRefreshInterruptsWait(t *testing.T) {
	m := New()
	done := make(chan bool, 1)
	go func() { done <- m.sleepOrRefresh(context.Background(), time.Hour) }()
	m.Refresh()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Refresh 不应停掉 Mapper")
		}
	case <-time.After(time.Second):
		t.Fatal("Refresh 应立即打断续租等待")
	}
}

func TestMapperOnChangeIncludesIPv6Pinholes(t *testing.T) {
	m := New()
	m.logf = func(string, ...any) {}
	calls := 0
	m.OnChange = func(Status) { calls++ }
	pinhole := Pinhole{Proto: "tcp", Port: 8443, GUA: netip.MustParseAddr("2001:db8::1"), Method: "pcp6"}
	m.setPinholes([]Pinhole{pinhole}, "已放行")
	m.setPinholes([]Pinhole{pinhole}, "已放行")
	m.setPinholes(nil, "")
	if calls != 2 {
		t.Fatalf("IPv6 pinhole 建立和撤销应各触发一次 OnChange，实际 %d", calls)
	}
}

var (
	httpWant  = Want{Proto: "tcp", Port: 8080, Desc: "hearth http"}
	mediaWant = Want{Proto: "udp", Port: 47700, Desc: "hearth media", StrictPort: true}
)

func TestMapperPrefersSamePort(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "198.51.100.9"), nil
	}}
	m := testMapper(c)

	delays := runRounds(m, staticWants(httpWant), 1)

	calls := c.mapCalls()
	if len(calls) != 1 || calls[0].external != httpWant.Port {
		t.Fatalf("首选外部端口应等于内部端口，实际调用 %+v", calls)
	}
	if calls[0].lifetime != leaseDuration {
		t.Fatalf("租期 = %v", calls[0].lifetime)
	}
	st := m.Snapshot()
	if st.Diagnosis != DiagOK || st.Method != "pcp" || len(st.Mappings) != 1 {
		t.Fatalf("状态不对: %+v", st)
	}
	if st.Gateway.String() != "192.168.0.1" {
		t.Fatalf("网关 = %v", st.Gateway)
	}
	if mp, ok := m.Lookup("tcp", 8080); !ok || mp.External != 8080 {
		t.Fatalf("Lookup = %+v %v", mp, ok)
	}
	if delays[0] != renewInterval {
		t.Fatalf("成功后应按续租周期等待，得到 %v", delays[0])
	}
}

func TestMapperConflictLetsGatewayChoose(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		if external == w.Port {
			return Mapping{}, ErrConflict
		}
		return okMap(w, 51000, "198.51.100.9"), nil
	}}
	m := testMapper(c)

	runRounds(m, staticWants(httpWant), 1)

	calls := c.mapCalls()
	if len(calls) != 2 || calls[1].external != 0 {
		t.Fatalf("冲突后应改由网关分配，实际调用 %+v", calls)
	}
	st := m.Snapshot()
	if st.Diagnosis != DiagOK || len(st.Mappings) != 1 || st.Mappings[0].External != 51000 {
		t.Fatalf("状态不对: %+v", st)
	}
}

func TestMapperStrictPortConflict(t *testing.T) {
	// 网关改派了端口：StrictPort 的 want 要撤销改派的那条并报 port_conflict。
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, 51000, "198.51.100.9"), nil
	}}
	m := testMapper(c)

	delays := runRounds(m, staticWants(mediaWant), 1)

	if calls := c.mapCalls(); len(calls) != 1 {
		t.Fatalf("StrictPort 不该退而求其次再申请一次，实际 %+v", calls)
	}
	un := c.unmapCalls()
	if len(un) != 1 || un[0].external != 51000 {
		t.Fatalf("应撤销被改派的映射，实际 %+v", un)
	}
	st := m.Snapshot()
	if st.Diagnosis != DiagPortConflict || len(st.Mappings) != 0 {
		t.Fatalf("状态不对: %+v", st)
	}
	if delays[0] != minBackoff {
		t.Fatalf("失败后应退避重试，得到 %v", delays[0])
	}
}

func TestMapperStrictPortDoesNotAcceptAnyPort(t *testing.T) {
	c := &fakeClient{mapFn: func(Want, int, time.Duration) (Mapping, error) {
		return Mapping{}, ErrConflict
	}}
	m := testMapper(c)

	runRounds(m, staticWants(mediaWant), 1)

	if calls := c.mapCalls(); len(calls) != 1 {
		t.Fatalf("StrictPort 冲突后不该改用网关分配，实际 %+v", calls)
	}
	if st := m.Snapshot(); st.Diagnosis != DiagPortConflict {
		t.Fatalf("诊断 = %s", st.Diagnosis)
	}
}

func TestMapperPrivateExternalIsUpstreamNAT(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "10.0.0.8"), nil
	}}
	m := testMapper(c)

	delays := runRounds(m, staticWants(httpWant), 1)

	st := m.Snapshot()
	if st.Diagnosis != DiagUpstreamNAT {
		t.Fatalf("诊断 = %s", st.Diagnosis)
	}
	// 私网外部地址不算失败：映射照常保留、Lookup 照常返回、按续租周期走。
	if len(st.Mappings) != 1 {
		t.Fatalf("映射应保留: %+v", st.Mappings)
	}
	if _, ok := m.Lookup("tcp", 8080); !ok {
		t.Fatal("Lookup 应仍返回该映射")
	}
	if delays[0] != renewInterval {
		t.Fatalf("等待时长 = %v", delays[0])
	}
	if st.Detail == "" {
		t.Fatal("upstream_nat 必须给出可执行的指引")
	}
}

func TestMapperCGNATIsUpstreamNAT(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "100.64.1.2"), nil
	}}
	m := testMapper(c)
	runRounds(m, staticWants(httpWant), 1)
	if st := m.Snapshot(); st.Diagnosis != DiagUpstreamNAT {
		t.Fatalf("CGNAT 地址应判为 upstream_nat，得到 %s", st.Diagnosis)
	}
}

func TestMapperWantsChangeDropsStale(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "198.51.100.9"), nil
	}}
	m := testMapper(c)

	changed := Want{Proto: "tcp", Port: 9090, Desc: "hearth http"}
	round := 0
	wants := func(context.Context) []Want {
		round++
		if round == 1 {
			return []Want{httpWant}
		}
		return []Want{changed}
	}
	runRounds(m, wants, 2)

	un := c.unmapCalls()
	if len(un) != 1 || un[0].w.Port != 8080 {
		t.Fatalf("旧端口应被撤销，实际 %+v", un)
	}
	if _, ok := m.Lookup("tcp", 8080); ok {
		t.Fatal("旧映射应已从 Lookup 消失")
	}
	if mp, ok := m.Lookup("tcp", 9090); !ok || mp.External != 9090 {
		t.Fatalf("新映射未建立: %+v %v", mp, ok)
	}
}

func TestMapperEmptyWantsRevokes(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "198.51.100.9"), nil
	}}
	m := testMapper(c)

	round := 0
	wants := func(context.Context) []Want {
		round++
		if round == 1 {
			return []Want{httpWant}
		}
		return nil
	}
	delays := runRounds(m, wants, 2)

	if un := c.unmapCalls(); len(un) != 1 {
		t.Fatalf("wants 清空后应撤销已有映射，实际 %+v", un)
	}
	st := m.Snapshot()
	if st.Diagnosis != DiagOff || len(st.Mappings) != 0 {
		t.Fatalf("状态不对: %+v", st)
	}
	if delays[1] != minBackoff {
		t.Fatalf("停用后应短周期回读 wants，得到 %v", delays[1])
	}
}

func TestMapperNotAuthorizedBacksOffHard(t *testing.T) {
	c := &fakeClient{mapFn: func(Want, int, time.Duration) (Mapping, error) {
		return Mapping{}, ErrNotAuthorized
	}}
	m := testMapper(c)

	delays := runRounds(m, staticWants(httpWant), 2)

	st := m.Snapshot()
	if st.Diagnosis != DiagDisabledByGateway {
		t.Fatalf("诊断 = %s", st.Diagnosis)
	}
	for i, d := range delays {
		if d != maxBackoff {
			t.Fatalf("第 %d 轮等待 %v，网关明确拒绝时不该紧密重试", i+1, d)
		}
	}
}

func TestMapperErrorBacksOffExponentially(t *testing.T) {
	c := &fakeClient{mapFn: func(Want, int, time.Duration) (Mapping, error) {
		return Mapping{}, errors.New("网关内部错误")
	}}
	m := testMapper(c)

	delays := runRounds(m, staticWants(httpWant), 3)

	if st := m.Snapshot(); st.Diagnosis != DiagError {
		t.Fatalf("诊断 = %s", st.Diagnosis)
	}
	if delays[0] != minBackoff || delays[1] != 2*minBackoff || delays[2] != 4*minBackoff {
		t.Fatalf("退避序列 = %v", delays)
	}
}

func TestMapperNoGateway(t *testing.T) {
	c := &fakeClient{mapFn: func(Want, int, time.Duration) (Mapping, error) {
		t.Fatal("发现不到网关时不该发请求")
		return Mapping{}, nil
	}}
	m := testMapper(c)
	m.gateway = func() (netip.Addr, error) { return netip.Addr{}, errNoGateway }

	delays := runRounds(m, staticWants(httpWant), 1)

	st := m.Snapshot()
	if st.Diagnosis != DiagNoGateway || st.Detail == "" {
		t.Fatalf("状态不对: %+v", st)
	}
	if delays[0] != minBackoff {
		t.Fatalf("等待时长 = %v", delays[0])
	}
}

func TestMapperFallsBackToUPnP(t *testing.T) {
	pcp := &fakeClient{mapFn: func(Want, int, time.Duration) (Mapping, error) {
		return Mapping{}, ErrUnsupported
	}}
	upnp := &fakeClient{name: "upnp", mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "198.51.100.9"), nil
	}}
	m := testMapper(pcp)
	m.upnp = func(context.Context) (client, error) { return upnp, nil }

	runRounds(m, staticWants(httpWant), 2)

	st := m.Snapshot()
	if st.Method != "upnp" || st.Diagnosis != DiagOK {
		t.Fatalf("状态不对: %+v", st)
	}
	// 锁定 UPnP 之后不该再回头试 PCP。
	if len(pcp.mapCalls()) != 1 {
		t.Fatalf("PCP 被调用 %d 次", len(pcp.mapCalls()))
	}
	if len(upnp.mapCalls()) != 2 {
		t.Fatalf("UPnP 被调用 %d 次", len(upnp.mapCalls()))
	}
}

func TestMapperPermanentOnly(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, lifetime time.Duration) (Mapping, error) {
		if lifetime != 0 {
			return Mapping{}, ErrPermanentOnly
		}
		mp := okMap(w, external, "198.51.100.9")
		mp.ExpiresAt = time.Time{}
		return mp, nil
	}}
	m := testMapper(c)

	runRounds(m, staticWants(httpWant), 1)

	calls := c.mapCalls()
	if len(calls) != 2 || calls[1].lifetime != 0 {
		t.Fatalf("应以 lifetime=0 重发，实际 %+v", calls)
	}
	st := m.Snapshot()
	if st.Diagnosis != DiagOK || !st.Mappings[0].ExpiresAt.IsZero() {
		t.Fatalf("永久映射的 ExpiresAt 应为零值: %+v", st)
	}
}

func TestMapperCloseRevokes(t *testing.T) {
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "198.51.100.9"), nil
	}}
	m := testMapper(c)
	runRounds(m, staticWants(httpWant, mediaWant), 1)

	m.Close(context.Background())

	un := c.unmapCalls()
	if len(un) != 2 {
		t.Fatalf("Close 应撤销全部映射，实际 %+v", un)
	}
	if _, ok := m.Lookup("tcp", 8080); ok {
		t.Fatal("Close 后 Lookup 应返回 false")
	}
	if st := m.Snapshot(); st.Diagnosis != DiagOff {
		t.Fatalf("诊断 = %s", st.Diagnosis)
	}
}

func TestMapperOnChangeOnlyOnRealChange(t *testing.T) {
	ext := "198.51.100.9"
	c := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, ext), nil
	}}
	m := testMapper(c)
	var got []Status
	m.OnChange = func(st Status) { got = append(got, st) }

	round := 0
	wants := func(context.Context) []Want {
		round++
		if round == 3 {
			ext = "198.51.100.10" // 外部地址变了
		}
		return []Want{httpWant}
	}
	runRounds(m, wants, 3)

	// 第 1 轮建立、第 2 轮原样续租（不回调）、第 3 轮外部地址变化。
	if len(got) != 2 {
		t.Fatalf("回调次数 = %d，续租不变时不该回调", len(got))
	}
	if got[1].Mappings[0].ExternalIP.String() != "198.51.100.10" {
		t.Fatalf("回调带的状态不对: %+v", got[1].Mappings)
	}
}

// TestMapperReportsChainHops 级联走通两跳时，Status 要回显每一跳，诊断按最外层的
// 外部地址判定（拿到公网地址就是 ok，不再提示配上游）。
func TestMapperReportsChainHops(t *testing.T) {
	inner := &fakeClient{mapFn: func(w Want, external int, _ time.Duration) (Mapping, error) {
		return okMap(w, external, "10.0.0.2"), nil
	}}
	m := testMapper(inner)
	m.hopFound = func(_ context.Context, x netip.Addr, w Want, external int, _ time.Duration) (client, netip.Addr, Mapping, error) {
		return &fakeClient{}, netip.MustParseAddr("10.0.0.1"), okMap(w, external, "198.51.100.9"), nil
	}

	runRounds(m, staticWants(httpWant), 1)

	st := m.Snapshot()
	if st.Diagnosis != DiagOK {
		t.Fatalf("诊断 = %s（%s）", st.Diagnosis, st.Detail)
	}
	if len(st.Hops) != 2 || st.Hops[0].Gateway.String() != "192.168.0.1" || st.Hops[1].Gateway.String() != "10.0.0.1" {
		t.Fatalf("跳列表 = %+v", st.Hops)
	}
	if len(st.Mappings) != 1 || st.Mappings[0].ExternalIP.String() != "198.51.100.9" {
		t.Fatalf("映射应取最外层的外部地址: %+v", st.Mappings)
	}
	if !strings.Contains(st.Detail, "经 2 层网关") {
		t.Fatalf("诊断文案应带跳数: %s", st.Detail)
	}
}

func TestNormalizeWants(t *testing.T) {
	ws := normalizeWants([]Want{
		{Proto: "TCP", Port: 8080},
		{Proto: "tcp", Port: 8080}, // 重复
		{Proto: "udp", Port: 0},    // 非法端口
		{Proto: "sctp", Port: 1},   // 非法协议
		{Proto: "udp", Port: 47700},
	})
	if len(ws) != 2 || ws[0].Proto != "tcp" || ws[1].Port != 47700 {
		t.Fatalf("规范化结果 = %+v", ws)
	}
}

// TestLivePortmap 对着本机真实默认网关跑一遍「发现 → 申请 → 撤销」，默认跳过：
// 结果取决于所在网络，CI 与无网关环境都不该因此失败。
// 手动验收：PORTMAP_LIVE=1 go test -run TestLive -v ./internal/portmap/
// 需要人工核对网关租约表时加 PORTMAP_LIVE_HOLD=<秒数>，撤销前会停留这么久。
func TestLivePortmap(t *testing.T) {
	if os.Getenv("PORTMAP_LIVE") != "1" {
		t.Skip("需要 PORTMAP_LIVE=1 和一台支持 PCP/NAT-PMP/UPnP 的网关")
	}
	gw, err := defaultGateway()
	if err != nil {
		t.Fatalf("发现默认网关失败: %v", err)
	}
	t.Logf("默认网关已发现（地址不打印）: is4=%v private=%v", gw.Is4(), gw.IsPrivate())

	m := New()
	m.logf = func(format string, args ...any) { t.Logf(format, args...) }
	m.sleep = func(context.Context, time.Duration) bool { return false } // 只跑一轮

	want := Want{Proto: "udp", Port: 47999, Desc: "hearth portmap live test"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m.Run(ctx, staticWants(want))

	st := m.Snapshot()
	t.Logf("方法=%s 诊断=%s 映射数=%d", st.Method, st.Diagnosis, len(st.Mappings))
	if len(st.Mappings) == 0 {
		t.Fatalf("没有建立任何映射：%s", st.Diagnosis)
	}
	mp := st.Mappings[0]
	t.Logf("映射 %s 内部端口 %d → 外部端口 %d（外部地址是否公网: %v）",
		mp.Proto, mp.Internal, mp.External, isPublicAddr(mp.ExternalIP))
	if mp.External != want.Port {
		t.Errorf("外部端口 %d 与内部端口不同，应优先申请同端口", mp.External)
	}
	if _, ok := m.Lookup("udp", want.Port); !ok {
		t.Error("Lookup 查不到刚建立的映射")
	}

	if s := os.Getenv("PORTMAP_LIVE_HOLD"); s != "" {
		d, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("PORTMAP_LIVE_HOLD: %v", err)
		}
		t.Logf("保持映射 %d 秒，便于核对网关租约表", d)
		time.Sleep(time.Duration(d) * time.Second)
	}

	m.Close(context.Background())
	if _, ok := m.Lookup("udp", want.Port); ok {
		t.Error("Close 后 Lookup 仍返回映射")
	}
}
