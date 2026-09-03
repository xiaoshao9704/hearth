package portmap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// upnpDiscoveryTimeout UPnP 发现要连做 v2/v1/PPP 三次 SSDP 搜索，比 PCP 的单包首发宽裕得多。
// 只在首轮或换协议时付一次，Run 本身是后台协程，不拖启动。
const upnpDiscoveryTimeout = 4 * discoveryTimeout

// Mapper 后台维护一组端口映射：发现网关 → 申请 → 续租 → 退出时撤销。
// 任何失败都只反映在 Snapshot 的诊断里，不影响进程其余部分。
type Mapper struct {
	// OnChange 映射集合变化（建立、丢失、外部地址或外部端口变化）时同步回调，
	// 用于刷新 SDP 宣告一类的下游状态；调用方不得在回调里阻塞。可为 nil，须在 Run 前设置。
	OnChange func(Status)

	// 发现途径与时间源做成字段，单测注入假 client 与假时钟。
	gateway  func() (netip.Addr, error)
	newPCP   func(netip.AddrPort) client
	upnp     func(context.Context) (client, error)
	hopFound hopDiscoverFunc // 级联时发现上游那一跳，见 chain.go
	sleep    func(context.Context, time.Duration) bool
	now      func() time.Time
	logf     func(string, ...any)

	mu      sync.Mutex
	cl      client // 锁定的协议客户端，nil = 尚未发现
	active  map[mapKey]Mapping
	st      Status
	backoff time.Duration
	closed  bool
}

type mapKey struct {
	proto string
	port  int
}

func New() *Mapper {
	return &Mapper{
		gateway:  defaultGateway,
		newPCP:   newPCPClient,
		upnp:     discoverUPnP,
		hopFound: discoverHop,
		sleep:    sleepCtx,
		now:      time.Now,
		logf:     log.Printf,
		active:   make(map[mapKey]Mapping),
		st:       Status{Diagnosis: DiagOff},
	}
}

// Run 阻塞到 ctx 结束。wants 是 getter，每轮读一次：端口来自动态配置，
// 后台改了下一轮就撤旧加新。
func (m *Mapper) Run(ctx context.Context, wants func(context.Context) []Want) {
	for {
		delay := m.round(ctx, wants)
		if !m.sleep(ctx, delay) {
			return
		}
	}
}

// Lookup 查一条已建立的映射，给 SDP 宣告外部地址用。
func (m *Mapper) Lookup(proto string, internal int) (Mapping, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Mapping{}, false
	}
	mp, ok := m.active[mapKey{strings.ToLower(proto), internal}]
	return mp, ok
}

// UDPExternal 查 UDP 端口映射出的外部地址，签名即 lite.MappedFunc，直接交给内核做 SDP 宣告。
// 外部地址无效时返回 false：UPnP 路径可能给不出有效的外部地址，宣告出去就是个连不通的候选。
func (m *Mapper) UDPExternal(port int) (netip.AddrPort, bool) {
	mp, ok := m.Lookup("udp", port)
	if !ok || !mp.ExternalIP.IsValid() {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(mp.ExternalIP, uint16(mp.External)), true
}

func (m *Mapper) Snapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.clone()
}

// Close 撤销全部映射。Run 已退出（ctx 结束）时它的 ctx 也没用了，所以要传一个新的。
func (m *Mapper) Close(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()

	m.unmapAll(ctx)

	m.mu.Lock()
	m.st = Status{Diagnosis: DiagOff, Detail: "端口映射已停止", UpdatedAt: m.now()}
	m.mu.Unlock()
}

// round 跑一轮申请/续租，返回距下一轮的等待时长。
func (m *Mapper) round(ctx context.Context, wants func(context.Context) []Want) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	ws := normalizeWants(wants(ctx))
	if ctx.Err() != nil {
		return 0
	}

	if len(ws) == 0 {
		m.unmapAll(ctx)
		m.commit(nil, DiagOff, "未启用端口映射（没有待映射的端口）")
		// 端口来自动态配置，配上之后要尽快生效，不等一个续租周期。
		return minBackoff
	}

	m.dropStale(ctx, ws)

	var (
		mappings []Mapping
		errs     []error
	)
	for _, w := range ws {
		m.mu.Lock()
		c := m.cl
		m.mu.Unlock()

		var (
			mp  Mapping
			err error
		)
		if c != nil {
			mp, err = m.apply(ctx, c, w)
		} else {
			// 发现与第一条 want 的申请是同一件事，不重复申请。
			c, mp, err = m.pick(ctx, w)
			if c == nil {
				m.commit(nil, DiagNoGateway, detailNoGateway)
				return m.bump()
			}
			m.mu.Lock()
			m.cl = c
			m.mu.Unlock()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%d: %w", w.Proto, w.Port, err))
			continue
		}
		mappings = append(mappings, mp)
	}

	if anyIs(errs, ErrUnsupported) {
		// 锁定的网关不再应答（换了网络、网关被替换）：丢掉客户端，下一轮重新发现，
		// 否则会一直对着死地址续租。
		m.mu.Lock()
		m.cl = nil
		m.mu.Unlock()
	}

	switch {
	case anyIs(errs, ErrNotAuthorized):
		m.commit(mappings, DiagDisabledByGateway, detailDisabledByGateway)
		// 网关已明确拒绝，紧密重试毫无意义：直接退到最大间隔等人改配置。
		return m.stall()
	case anyIs(errs, ErrConflict):
		m.commit(mappings, DiagPortConflict, detailPortConflict(errs))
		return m.bump()
	case len(errs) > 0:
		m.commit(mappings, DiagError, errors.Join(errs...).Error())
		return m.bump()
	}

	hops, stall := m.chainInfo()
	if ip, ok := firstPrivateExternal(mappings); ok {
		// 私网外部地址不算失败：上游做了 DMZ/转发时映射照样有效，只是要提示补那一层配置。
		m.commit(mappings, DiagUpstreamNAT, detailUpstreamNAT(ip, mappings, stall))
	} else {
		m.commit(mappings, DiagOK, detailOK(mappings, hops))
	}
	m.reset()
	return renewInterval
}

// pick 发现网关并锁定协议客户端：PCP（含 NAT-PMP 回落）只要网关有响应就锁定，
// 只有 ErrUnsupported（5351 上没反应）才退到 UPnP。PCP 侧不额外套超时——
// roundTrip 自己就限死了重试次数与每次的等待。
// 返回的 Mapping/error 是这条 want 的申请结果，调用方不必重复申请。
// 锁定的客户端一律包一层 chainClient：外部地址是私网时它自己会向上游续接（chain.go），
// Mapper 只看最外层的结果。
func (m *Mapper) pick(ctx context.Context, w Want) (client, Mapping, error) {
	gw, err := m.gateway()
	if err != nil {
		return nil, Mapping{}, err
	}
	m.mu.Lock()
	m.st.Gateway = gw
	m.mu.Unlock()

	c := m.newChain(m.newPCP(netip.AddrPortFrom(gw, pcpPort)), gw)
	mp, err := m.apply(ctx, c, w)
	if !errors.Is(err, ErrUnsupported) {
		return c, mp, err
	}
	if ctx.Err() != nil {
		return nil, Mapping{}, ctx.Err()
	}

	uctx, cancel := context.WithTimeout(ctx, upnpDiscoveryTimeout)
	defer cancel()
	uc, err := m.upnp(uctx)
	if err != nil {
		return nil, Mapping{}, err
	}
	cc := m.newChain(uc, gw)
	mp, err = m.apply(uctx, cc, w)
	if errors.Is(err, ErrUnsupported) {
		return nil, Mapping{}, err
	}
	return cc, mp, err
}

func (m *Mapper) newChain(first client, gw netip.Addr) *chainClient {
	c := newChain(first, gw, m.now, m.logf)
	c.discover = m.hopFound
	return c
}

// chainInfo 取链的回显与停链原因；非链式客户端（不会出现，留作兜底）返回空。
func (m *Mapper) chainInfo() ([]Hop, string) {
	m.mu.Lock()
	c := m.cl
	m.mu.Unlock()
	if hl, ok := c.(hopLister); ok {
		return hl.hopList(), hl.stallReason()
	}
	return nil, ""
}

func (m *Mapper) unmapAll(ctx context.Context) {
	m.mu.Lock()
	c := m.cl
	active := m.active
	m.active = make(map[mapKey]Mapping)
	m.mu.Unlock()
	if c == nil || len(active) == 0 {
		return
	}
	for k, mp := range active {
		m.unmapOne(ctx, c, k, mp)
	}
}

// dropStale 撤销已不在 wants 里的映射（后台改了端口）。
func (m *Mapper) dropStale(ctx context.Context, ws []Want) {
	keep := make(map[mapKey]bool, len(ws))
	for _, w := range ws {
		keep[mapKey{w.Proto, w.Port}] = true
	}
	m.mu.Lock()
	c := m.cl
	var stale []mapKey
	for k := range m.active {
		if !keep[k] {
			stale = append(stale, k)
		}
	}
	drop := make(map[mapKey]Mapping, len(stale))
	for _, k := range stale {
		drop[k] = m.active[k]
		delete(m.active, k)
	}
	m.mu.Unlock()
	if c == nil {
		return
	}
	for k, mp := range drop {
		m.unmapOne(ctx, c, k, mp)
	}
}

func (m *Mapper) unmapOne(ctx context.Context, c client, k mapKey, mp Mapping) {
	if err := c.Unmap(ctx, Want{Proto: k.proto, Port: k.port}, mp.External); err != nil {
		m.logf("portmap: 撤销映射 %s/%d 失败: %v", k.proto, k.port, err)
		return
	}
	m.logf("portmap: 已撤销映射 %s/%d", k.proto, k.port)
}

// apply 单条 want 的申请策略：先要 external == internal（上游 DMZ 是端口不变透传，
// 端口一变整条链就断），冲突了再让网关任选；StrictPort 的不接受改派。
func (m *Mapper) apply(ctx context.Context, c client, w Want) (Mapping, error) {
	mp, err := m.mapOnce(ctx, c, w, w.Port)
	if errors.Is(err, ErrConflict) && !w.StrictPort {
		mp, err = m.mapOnce(ctx, c, w, 0) // 0 = 由网关分配
	}
	if err != nil {
		return Mapping{}, err
	}
	if w.StrictPort && mp.External != w.Port {
		// 这条要求内外一致，改派过的映射留着只会是个误导人的假成功。
		if uerr := c.Unmap(ctx, w, mp.External); uerr != nil {
			m.logf("portmap: 撤销被改派的映射 %s/%d 失败: %v", w.Proto, w.Port, uerr)
		}
		return Mapping{}, fmt.Errorf("%w（网关改派到外部端口 %d，该端口要求内外一致）", ErrConflict, mp.External)
	}
	mp.Proto, mp.Internal, mp.Method = w.Proto, w.Port, c.Method()
	return mp, nil
}

func (m *Mapper) mapOnce(ctx context.Context, c client, w Want, external int) (Mapping, error) {
	mp, err := c.Map(ctx, w, external, leaseDuration)
	if errors.Is(err, ErrPermanentOnly) {
		// 老设备只支持永久映射：lifetime 0 重发，ExpiresAt 留零值，续租退化成每轮重发。
		mp, err = c.Map(ctx, w, external, 0)
		mp.ExpiresAt = time.Time{}
	}
	return mp, err
}

// commit 落状态并在映射集合变化时回调、打日志。续租轮一切照旧时不打任何日志。
func (m *Mapper) commit(mappings []Mapping, diag Diagnosis, detail string) {
	slices.SortFunc(mappings, func(a, b Mapping) int {
		if a.Proto != b.Proto {
			return strings.Compare(a.Proto, b.Proto)
		}
		return a.Internal - b.Internal
	})

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	changed := !sameMappings(m.st.Mappings, mappings)
	diagChanged := m.st.Diagnosis != diag || m.st.Detail != detail
	method := ""
	m.st.Hops = nil
	if m.cl != nil {
		method = m.cl.Method()
		if hl, ok := m.cl.(hopLister); ok {
			m.st.Hops = hl.hopList()
		}
	}
	methodChanged := m.st.Method != method

	m.st.Mappings = mappings
	m.st.Diagnosis = diag
	m.st.Detail = detail
	m.st.Method = method
	m.st.UpdatedAt = m.now()
	m.active = make(map[mapKey]Mapping, len(mappings))
	for _, mp := range mappings {
		m.active[mapKey{mp.Proto, mp.Internal}] = mp
	}
	st := m.st.clone()
	cb := m.OnChange
	m.mu.Unlock()

	if methodChanged && method != "" {
		m.logf("portmap: 网关 %s 支持 %s", st.Gateway, method)
	}
	if changed {
		if len(mappings) == 0 {
			m.logf("portmap: 当前没有已建立的映射")
		} else {
			m.logf("portmap: 映射 %s", describe(mappings))
		}
	}
	if diagChanged {
		m.logf("portmap: %s — %s", diag, detail)
	}
	if changed && cb != nil {
		cb(st)
	}
}

func (m *Mapper) bump() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.backoff == 0:
		m.backoff = minBackoff
	default:
		m.backoff *= 2
	}
	if m.backoff > maxBackoff {
		m.backoff = maxBackoff
	}
	return m.backoff
}

// stall 网关明确拒绝时直接退到最大间隔。
func (m *Mapper) stall() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backoff = maxBackoff
	return m.backoff
}

func (m *Mapper) reset() {
	m.mu.Lock()
	m.backoff = 0
	m.mu.Unlock()
}

func (s Status) clone() Status {
	s.Mappings = slices.Clone(s.Mappings)
	s.Hops = slices.Clone(s.Hops)
	return s
}

// normalizeWants 统一协议大小写、丢掉非法端口、按 (协议, 端口) 去重。
func normalizeWants(ws []Want) []Want {
	out := make([]Want, 0, len(ws))
	seen := make(map[mapKey]bool, len(ws))
	for _, w := range ws {
		w.Proto = strings.ToLower(w.Proto)
		if w.Proto != "tcp" && w.Proto != "udp" {
			continue
		}
		if w.Port <= 0 || w.Port > 65535 {
			continue
		}
		k := mapKey{w.Proto, w.Port}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, w)
	}
	return out
}

func sameMappings(a, b []Mapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Proto != b[i].Proto || a[i].Internal != b[i].Internal ||
			a[i].External != b[i].External || a[i].ExternalIP != b[i].ExternalIP {
			return false
		}
	}
	return true
}

func anyIs(errs []error, target error) bool {
	for _, err := range errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// firstPrivateExternal 找出第一个不是公网地址的外部地址（含 CGNAT、回环与无效值）。
func firstPrivateExternal(mappings []Mapping) (netip.Addr, bool) {
	for _, mp := range mappings {
		if !isPublicAddr(mp.ExternalIP) {
			return mp.ExternalIP, true
		}
	}
	return netip.Addr{}, false
}

func isPublicAddr(a netip.Addr) bool {
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsMulticast() {
		return false
	}
	// CGNAT 100.64.0.0/10 不在 IsPrivate 的范围里，但对我们同样是「上游还有一层」。
	if a.Is4() {
		b := a.As4()
		if b[0] == 100 && b[1]&0xc0 == 64 {
			return false
		}
	}
	return true
}

func describe(mappings []Mapping) string {
	parts := make([]string, 0, len(mappings))
	for _, mp := range mappings {
		ext := "?"
		if mp.ExternalIP.IsValid() {
			ext = mp.ExternalIP.String()
		}
		parts = append(parts, fmt.Sprintf("%s %d → %s:%d", mp.Proto, mp.Internal, ext, mp.External))
	}
	return strings.Join(parts, "，")
}

// externalPorts 列出「上游那一层要转发的」协议与端口。
func externalPorts(mappings []Mapping) string {
	parts := make([]string, 0, len(mappings))
	for _, mp := range mappings {
		parts = append(parts, fmt.Sprintf("%s/%d", mp.Proto, mp.External))
	}
	return strings.Join(parts, "、")
}

const detailNoGateway = "未发现支持 UPnP/PCP 的网关：容器 bridge 网络下发现不到网关（SSDP 出不了网桥、" +
	"默认网关是网桥地址），需要 network_mode: host 或裸机运行；也可能是网关没开启 UPnP/PCP。"

const detailDisabledByGateway = "网关因 NAT 行为探测判定上游不支持端口转发，主动禁用了整个端口转发功能。" +
	"若上游已做 DMZ 或端口转发，请在网关上关闭该探测，或改成允许「被过滤」结果的模式。"

func detailPortConflict(errs []error) string {
	return fmt.Sprintf("外部端口被占用或被网关改派，而该端口要求内外一致（上游 DMZ 是端口不变透传）："+
		"请换一个端口。详情：%v", errors.Join(errs...))
}

// detailUpstreamNAT 的「转发到」用最外层已到达那一跳的外部地址：级联走通了几跳，
// 要人工配置的就是再上面那一层。
func detailUpstreamNAT(ip netip.Addr, mappings []Mapping, stall string) string {
	tried := ""
	if stall != "" {
		tried = fmt.Sprintf("已尝试向上游网关申请但未成功（%s）。", stall)
	}
	return fmt.Sprintf("映射已建立，但网关给出的外部地址 %s 是私网地址：上游还有一层 NAT。%s"+
		"请在上游设备把 %s 转发到 %s（本机网关的 WAN 地址），或对它开启 DMZ。",
		ip, tried, externalPorts(mappings), ip)
}

func detailOK(mappings []Mapping, hops []Hop) string {
	detail := "端口映射已建立：" + describe(mappings)
	if len(hops) > 1 {
		gws := make([]string, 0, len(hops))
		for _, h := range hops {
			gws = append(gws, h.Gateway.String())
		}
		detail += fmt.Sprintf("（经 %d 层网关：%s）", len(hops), strings.Join(gws, " → "))
	}
	return detail
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
