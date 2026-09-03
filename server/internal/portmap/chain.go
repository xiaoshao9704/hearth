package portmap

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// 级联申请：内层网关给出的外部地址是私网时，说明上游还有一层 NAT，就把「内层拿到的
// 外部端口」当作外层的内部端口继续往上申请，直到拿到公网地址。形态是链式 client——
// Mapper 只看最外层的外部地址与端口，不感知跳数。
//
// 上游眼里的请求者就是内层路由：报文经内层 NAT 到达上游时源地址已变成内层路由的 WAN
// 地址，所以 PCP 头里的 client IP 与 UPnP 的 NewInternalClient 都填这个地址——既过得了
// PCP 的 ADDRESS_MISMATCH 校验，也过得了 miniupnpd secure_mode「只能给自己开洞」这条；
// 映射目标本来也正是内层路由。不需要 THIRD_PARTY option。
//
// 级联不替代诊断指引：任一跳失败就退回单跳的诊断（今天的行为），一次配好的静态转发
// 比一条可能在任意一层悄悄失效的自动链条更可靠。

// maxHops 链的深度上限，含本机默认网关。
const maxHops = 3

// hopDiscoverFunc 发现 x（内层给出的外部地址）上游的那一层网关，并用一条真实申请当探测。
// 返回上游客户端、它的网关地址，以及这条申请的结果。做成字段是为了单测注入回环假网关。
type hopDiscoverFunc func(ctx context.Context, x netip.Addr, w Want, external int, lifetime time.Duration) (client, netip.Addr, Mapping, error)

// chainHop 链上的一跳。
type chainHop struct {
	cl client
	gw netip.Addr
	// x 发现这一跳时内层给出的外部地址：它同时是上游眼里的请求者地址（PCP 的 client IP /
	// UPnP 的 NewInternalClient）。内层 WAN 地址一变，这一跳的凭据就失效，要重新发现。
	x   netip.Addr
	ext netip.Addr // 最近一次申请拿到的外部地址，供 Status.Hops 回显
}

// chainClient 实现 client：hops[0] 是默认网关，之后每跳是上一跳的上游。
type chainClient struct {
	discover hopDiscoverFunc
	now      func() time.Time
	logf     func(string, ...any)

	mu    sync.Mutex
	hops  []*chainHop
	ports map[mapKey][]int         // 每个 (协议, 内部端口) 在各跳申请到的外部端口
	tried map[netip.Addr]time.Time // 上游发现失败的负缓存，键是内层给出的外部地址
	stall string                   // 链为何停在当前这一跳，给 upstream_nat 的诊断文案用
}

// hopLister 由 chainClient 实现，给 Mapper 回显跳数与停链原因。
type hopLister interface {
	hopList() []Hop
	stallReason() string
}

func newChain(first client, gw netip.Addr, now func() time.Time, logf func(string, ...any)) *chainClient {
	return &chainClient{
		discover: discoverHop,
		now:      now,
		logf:     logf,
		hops:     []*chainHop{{cl: first, gw: gw}},
		ports:    make(map[mapKey][]int),
		tried:    make(map[netip.Addr]time.Time),
	}
}

func (c *chainClient) Method() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hops[0].cl.Method()
}

// Map 先向默认网关申请，拿到的外部地址不是公网就继续向上游申请「上一跳的外部端口 → 同端口优先」，
// 返回最外层的结果（Internal 仍是 w.Port）。上游那几跳失败不算失败：链停在当前这一跳，
// 由 Mapper 按外部地址给 upstream_nat 诊断。
func (c *chainClient) Map(ctx context.Context, w Want, external int, lifetime time.Duration) (Mapping, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	mp, err := c.hops[0].cl.Map(ctx, w, external, lifetime)
	if err != nil {
		return mp, err
	}
	c.hops[0].ext = mp.ExternalIP
	c.stall = ""

	key := mapKey{strings.ToLower(w.Proto), w.Port}
	prev := c.ports[key]
	ports := []int{mp.External}
	out := mp
	for len(ports) < maxHops && !isPublicAddr(out.ExternalIP) {
		up, ok := c.extend(ctx, len(ports), out, w, preferredPort(prev, len(ports), out.External), lifetime)
		if !ok {
			break
		}
		ports = append(ports, up.External)
		if !up.ExpiresAt.IsZero() && (mp.ExpiresAt.IsZero() || up.ExpiresAt.Before(mp.ExpiresAt)) {
			// 整条链只在最短的那一段租期内有效。
			mp.ExpiresAt = up.ExpiresAt
		}
		out = up
	}
	c.ports[key] = ports

	mp.External, mp.ExternalIP = out.External, out.ExternalIP
	return mp, nil
}

// Unmap 从最外层往里逐跳删除：外层映射的内部端口正是内层的外部端口，
// 内层先没了会让外层指向一个已不存在的端口。
func (c *chainClient) Unmap(ctx context.Context, w Want, external int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := mapKey{strings.ToLower(w.Proto), w.Port}
	ports := c.ports[key]
	delete(c.ports, key)

	var errs []error
	for i := len(ports) - 1; i >= 1 && i < len(c.hops); i-- {
		up := Want{Proto: w.Proto, Port: ports[i-1], Desc: w.Desc}
		if err := c.hops[i].cl.Unmap(ctx, up, ports[i]); err != nil {
			errs = append(errs, fmt.Errorf("上游网关 %s: %w", c.hops[i].gw, err))
		}
	}
	if len(ports) > 0 {
		external = ports[0] // 入参是最外层的端口，第一跳要用它自己那个
	}
	errs = append(errs, c.hops[0].cl.Unmap(ctx, w, external))
	return errors.Join(errs...)
}

func (c *chainClient) hopList() []Hop {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Hop, 0, len(c.hops))
	for _, h := range c.hops {
		out = append(out, Hop{Gateway: h.gw, Method: h.cl.Method(), ExternalIP: h.ext})
	}
	return out
}

func (c *chainClient) stallReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stall
}

// extend 申请第 i 跳（i >= 1）：已有这一跳就直接续租，没有就先发现。
// 返回 false 表示链停在 i-1 跳，原因记在 c.stall 里。调用方持有 c.mu。
func (c *chainClient) extend(ctx context.Context, i int, inner Mapping, w Want, pref int, lifetime time.Duration) (Mapping, bool) {
	x := inner.ExternalIP
	if !x.IsValid() {
		c.stall = "内层网关没有给出外部地址"
		return Mapping{}, false
	}
	up := Want{Proto: w.Proto, Port: inner.External, Desc: w.Desc}

	if i < len(c.hops) {
		if h := c.hops[i]; h.x == x {
			mp, err := h.cl.Map(ctx, up, pref, lifetime)
			if err != nil {
				c.stall = fmt.Sprintf("向上游网关 %s 申请被拒绝：%v", h.gw, err)
				return Mapping{}, false
			}
			h.ext = mp.ExternalIP
			return mp, true
		}
		c.hops = c.hops[:i] // 内层 WAN 地址变了，这一跳的凭据失效，重新发现
	}

	if t, ok := c.tried[x]; ok && c.now().Sub(t) < maxBackoff {
		c.stall = "未发现支持 UPnP/PCP 的上游网关（发现失败后一段时间内不重试）"
		return Mapping{}, false
	}
	for _, h := range c.hops {
		if h.gw == x {
			// 外部地址等于链上某一跳的网关，再往上就是绕回来了。
			c.stall = "上游网关地址出现环路，链停在这一跳"
			return Mapping{}, false
		}
	}

	cl, gw, mp, err := c.discover(ctx, x, up, pref, lifetime)
	if err != nil {
		c.tried[x] = c.now()
		if errors.Is(err, ErrUnsupported) {
			c.stall = "未发现支持 UPnP/PCP 的上游网关"
		} else {
			c.stall = fmt.Sprintf("上游网关拒绝了申请：%v", err)
		}
		c.logf("portmap: 未能向 %s 上游的网关申请端口映射（%s），%v 内不再重试", x, c.stall, maxBackoff)
		return Mapping{}, false
	}
	if isPublicAddr(gw) {
		// 公网地址上的设备不是「我们这一侧」的网关，绝不对它开洞。
		c.tried[x] = c.now()
		c.stall = "上游候选地址是公网地址，不向它申请映射"
		return Mapping{}, false
	}
	for _, h := range c.hops {
		if h.gw == gw {
			c.tried[x] = c.now()
			c.stall = "上游网关地址出现环路，链停在这一跳"
			return Mapping{}, false
		}
	}

	c.hops = append(c.hops, &chainHop{cl: cl, gw: gw, x: x, ext: mp.ExternalIP})
	c.logf("portmap: 上游网关 %s 支持 %s，映射 %s %d → %s:%d",
		gw, cl.Method(), up.Proto, up.Port, mp.ExternalIP, mp.External)
	return mp, true
}

// preferredPort 续租时每跳都用上一轮申请到的那个外部端口重发，网关才不会每轮换端口；
// 没有记录（首次申请）就沿用同端口优先。
func preferredPort(ports []int, i, fallback int) int {
	if i < len(ports) && ports[i] > 0 {
		return ports[i]
	}
	return fallback
}

// discoverHop 是 hopDiscoverFunc 的默认实现：候选地址按网段启发得出（见 upstreamCandidate），
// 协议顺序与 Mapper.pick 相同——PCP 只要网关有应答就锁定，ErrUnsupported 才退到 UPnP
// 单播发现（多播 M-SEARCH 出不了本级路由）。x 是上游眼里的请求者地址。
func discoverHop(ctx context.Context, x netip.Addr, w Want, external int, lifetime time.Duration) (client, netip.Addr, Mapping, error) {
	cand, ok := upstreamCandidate(x)
	if !ok {
		return nil, netip.Addr{}, Mapping{}, ErrUnsupported
	}

	pc := newPCPClientAs(netip.AddrPortFrom(cand, pcpPort), x)
	mp, err := pc.Map(ctx, w, external, lifetime)
	if !errors.Is(err, ErrUnsupported) {
		return pc, cand, mp, err
	}
	if ctx.Err() != nil {
		return nil, netip.Addr{}, Mapping{}, ctx.Err()
	}

	uctx, cancel := context.WithTimeout(ctx, upnpDiscoveryTimeout)
	defer cancel()
	uc, err := discoverUPnPAt(uctx, cand)
	if err != nil {
		return nil, netip.Addr{}, Mapping{}, err
	}
	uc.internalClient = x
	mp, err = uc.Map(uctx, w, external, lifetime)
	return uc, cand, mp, err
}

// upstreamCandidate 猜内层 WAN 地址 x 上游的网关：同 /24 的 .1，x 自己就是 .1 时改试 .254。
// 只对私网/CGNAT 的 IPv4 成立——公网地址那一侧已经不是我们能开洞的设备了。
func upstreamCandidate(x netip.Addr) (netip.Addr, bool) {
	if !x.Is4() || isPublicAddr(x) {
		return netip.Addr{}, false
	}
	b := x.As4()
	switch b[3] {
	case 1:
		b[3] = 254
	default:
		b[3] = 1
	}
	return netip.AddrFrom4(b), true
}
