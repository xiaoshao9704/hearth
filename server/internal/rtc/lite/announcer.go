package lite

import (
	"context"
	"log"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// 探测缓存的默认参数：TTL 内 Rules 复用探测结果；Refresh 受最小间隔保护，
// 周期刷新也刷不出密集探测。
const (
	DefaultAnnounceTTL         = 5 * time.Minute
	DefaultAnnounceMinInterval = 30 * time.Second
)

// MappedFunc 端口映射结果查询：本机 UDP 监听端口 → 网关映射出的外部地址。nil = 无映射来源。
// 用函数类型而不是依赖具体的映射包，是为了让 lite 与端口映射的实现解耦。
type MappedFunc func(port int) (netip.AddrPort, bool)

// Announcer 决定「向对端宣告哪些地址」，两条出口：
//   - Rules 给 Transport.NewAPI 用——只在显式配置了 public IP 时返回 pion 的替换规则；
//     其余情况一律返回 nil，让 pion 只宣告本机真实地址（host 候选）。
//   - Announce 在 SDP 出口把外部地址追加成 srflx 候选：端口映射结果与 STUN 探测结果并列
//     （映射结果排最前），都没有则不追加。
//
// 外部地址不再交给 pion 的改写规则，因为它改不了端口（见 AppendMappedCandidate）。
// 显式配置的 public IP 是例外：它的端口天然就是本地监听端口，语义正确，而「只通告它、
// 删掉 host 候选」交给 pion 比自己删 SDP 行更稳，所以那条路径保持不变、SDP 出口不再追加任何东西。
//
// 探测同一时刻只跑一次，并发调用者等待并共享结果。配置 getter 逐次调用：
// 管理后台改了 *_public_ip / *_stun_servers 下一轮探测即生效。
type Announcer struct {
	publicIP, stunServers func(context.Context) string
	mapped                MappedFunc
	probe                 func(locals, servers []string, timeout time.Duration) map[string]string
	ttl, minInterval      time.Duration // 零值用默认常量；测试覆盖

	mu     sync.Mutex
	public string            // 显式配置的 public IP，非空即覆盖语义
	stun   map[string]string // STUN 探测结果 local→external
	// mediaPort 最近一次 Announce 见到的本机媒体端口，只用于 Snapshot 回显映射结果
	// （映射查询按端口，而端口要到第一条 SDP 出来才知道）。
	mediaPort   int
	probedAt    time.Time
	probing     bool
	probeDone   chan struct{}
	lastChanged bool
}

func NewAnnouncer(publicIP, stunServers func(context.Context) string, mapped MappedFunc) *Announcer {
	return &Announcer{publicIP: publicIP, stunServers: stunServers, mapped: mapped, probe: probeAllSTUN}
}

// Rules 返回建 PeerConnection 用的地址改写规则；缓存过期（TTL）先同步重探。
// 只有显式配置 public IP 时才有规则（catch-all 替换，所有 host 候选改写成它）。
func (a *Announcer) Rules(ctx context.Context) []webrtc.ICEAddressRewriteRule {
	a.mu.Lock()
	fresh := !a.probedAt.IsZero() && time.Since(a.probedAt) < a.ttlDur()
	public := a.public
	a.mu.Unlock()
	if !fresh {
		a.runProbe(ctx)
		a.mu.Lock()
		public = a.public
		a.mu.Unlock()
	}
	if public == "" {
		return nil
	}
	return []webrtc.ICEAddressRewriteRule{{
		External:        []string{public},
		AsCandidateType: webrtc.ICECandidateTypeHost,
		// Mode 零值 = 替换：所有 host candidate 都改写成该地址
	}}
}

// Announce SDP 出口：把外部地址追加成 srflx 候选后返回。用的是 Rules 那一轮探到的缓存
// （不在应答路径上触发探测），也保证同一个 PeerConnection 的改写规则与追加候选出自同一份状态；
// ctx 只为调用点对齐保留。
func (a *Announcer) Announce(_ context.Context, sdp string) string {
	a.mu.Lock()
	public, stun, mapped := a.public, a.stun, a.mapped
	a.mu.Unlock()
	if public != "" { // 覆盖语义已由 pion 的替换规则实现，不再追加
		return sdp
	}
	var port int
	out := appendSrflx(sdp, func(hosts []candLine) []srflxCand {
		if len(hosts) == 0 {
			return nil
		}
		port = int(hosts[0].addr.Port()) // 单端口 mux 下所有 host 候选同端口
		// 映射结果与 STUN 结果并列宣告、互不否决（appendSrflx 按外部地址去重，对端各取可达者）：
		// 双层 NAT + 上游 DMZ 时映射返回的外部地址是私网（内层路由的 WAN），公网对端连不上它，
		// 真正可达的是 STUN 公网 IP + 映射端口（DMZ 是端口不变透传）；反之 1:1 NAT 下映射结果最准。
		// 所以三类都给：映射地址、STUN IP + 本地端口、STUN IP + 映射端口。
		var out []srflxCand
		var extPort uint16
		if mapped != nil {
			if ext, ok := mapped(port); ok {
				extPort = ext.Port()
				// 端口映射只覆盖 v4：raddr 取该段首个 v4 host 候选，没有就没法组这一行
				if v4, ok := firstV4(hosts); ok {
					out = append(out, srflxCand{ext: ext, related: v4.addr})
				}
			}
		}
		for _, h := range hosts {
			ext, ok := stun[h.addr.Addr().String()]
			if !ok {
				continue
			}
			ip, err := netip.ParseAddr(ext)
			if err != nil {
				continue
			}
			out = append(out, srflxCand{ext: netip.AddrPortFrom(ip, h.addr.Port()), related: h.addr})
			if extPort != 0 {
				out = append(out, srflxCand{ext: netip.AddrPortFrom(ip, extPort), related: h.addr})
			}
		}
		return out
	})
	if port > 0 {
		a.mu.Lock()
		a.mediaPort = port
		a.mu.Unlock()
	}
	return out
}

// Refresh 外部触发（进程内周期刷新、端口映射变化回调）的探测：距上次不足最小间隔直接返回未变。
func (a *Announcer) Refresh(ctx context.Context) (changed bool) {
	a.mu.Lock()
	recent := !a.probedAt.IsZero() && time.Since(a.probedAt) < a.minIntervalDur()
	a.mu.Unlock()
	if recent {
		return false
	}
	return a.runProbe(ctx)
}

// Snapshot 当前会宣告的外部地址与探测时间，给日志与管理后台回显。
func (a *Announcer) Snapshot() (externals []string, probedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := externalIPs(a.public, a.stun)
	if a.mapped != nil && a.mediaPort > 0 {
		if ext, ok := a.mapped(a.mediaPort); ok {
			out = append([]string{ext.String()}, out...) // 映射结果最准，排最前
		}
	}
	return out, a.probedAt
}

func (a *Announcer) runProbe(ctx context.Context) bool {
	a.mu.Lock()
	if a.probing { // 有探测在跑：等它结束共享结果，不重复探测
		done := a.probeDone
		a.mu.Unlock()
		<-done
		a.mu.Lock()
		changed := a.lastChanged
		a.mu.Unlock()
		return changed
	}
	a.probing = true
	a.probeDone = make(chan struct{})
	a.mu.Unlock()

	public := a.publicIP(ctx)
	var stun map[string]string
	if public == "" { // 显式配置时不必探测
		servers := splitTrim(a.stunServers(ctx))
		if len(servers) == 0 {
			servers = DefaultSTUNServers
		}
		stun = stunExternals(LocalIPs(), servers, a.probe)
	}

	a.mu.Lock()
	old := externalIPs(a.public, a.stun)
	changed := a.public != public || !maps.Equal(a.stun, stun)
	a.public, a.stun = public, stun
	a.probedAt = time.Now()
	a.lastChanged = changed
	a.probing = false
	close(a.probeDone)
	a.mu.Unlock()

	if changed {
		log.Printf("宣告公网映射变化: %v → %v", old, externalIPs(public, stun))
	}
	return changed
}

// externalIPs 探测侧的外部地址（显式配置的 public IP，或 STUN 探到的公网 IP），排序去重。
func externalIPs(public string, stun map[string]string) []string {
	if public != "" {
		return []string{public}
	}
	if len(stun) == 0 {
		return nil
	}
	out := slices.Sorted(maps.Values(stun))
	return slices.Compact(out)
}

func firstV4(hosts []candLine) (candLine, bool) {
	for _, h := range hosts {
		if h.addr.Addr().Is4() {
			return h, true
		}
	}
	return candLine{}, false
}

func (a *Announcer) ttlDur() time.Duration {
	if a.ttl > 0 {
		return a.ttl
	}
	return DefaultAnnounceTTL
}

func (a *Announcer) minIntervalDur() time.Duration {
	if a.minInterval > 0 {
		return a.minInterval
	}
	return DefaultAnnounceMinInterval
}
