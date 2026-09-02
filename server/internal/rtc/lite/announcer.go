package lite

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// 探测缓存的默认参数：TTL 内 Rules 复用规则；Refresh 受最小间隔保护，
// 健康检查周期触发也刷不出密集探测。
const (
	DefaultAnnounceTTL         = 5 * time.Minute
	DefaultAnnounceMinInterval = 30 * time.Second
)

// Announcer 宣告规则的探测缓存：Rules 给建 PeerConnection 用（缓存过期先同步重探），
// Refresh 给健康检查等外部触发用。同一时刻只跑一次探测，并发调用者等待并共享结果。
// 配置 getter 逐次调用：管理后台改了 *_public_ip / *_stun_servers 即时生效。
type Announcer struct {
	publicIP, stunServers func(context.Context) string
	probe                 func(locals, servers []string, timeout time.Duration) map[string]string
	ttl, minInterval      time.Duration // 零值用默认常量；测试覆盖

	mu          sync.Mutex
	rules       []webrtc.ICEAddressRewriteRule
	probedAt    time.Time
	probing     bool
	probeDone   chan struct{}
	lastChanged bool
}

func NewAnnouncer(publicIP, stunServers func(context.Context) string) *Announcer {
	return &Announcer{publicIP: publicIP, stunServers: stunServers, probe: probeAllSTUN}
}

// Rules 返回当前宣告规则；缓存过期（TTL）先同步重探。
func (a *Announcer) Rules(ctx context.Context) []webrtc.ICEAddressRewriteRule {
	a.mu.Lock()
	fresh := !a.probedAt.IsZero() && time.Since(a.probedAt) < a.ttlDur()
	a.mu.Unlock()
	if !fresh {
		a.runProbe(ctx)
	}
	rules, _ := a.Snapshot()
	return rules
}

// Refresh 外部触发（如健康检查）的探测：距上次不足最小间隔直接返回未变。
func (a *Announcer) Refresh(ctx context.Context) (changed bool) {
	a.mu.Lock()
	recent := !a.probedAt.IsZero() && time.Since(a.probedAt) < a.minIntervalDur()
	a.mu.Unlock()
	if recent {
		return false
	}
	return a.runProbe(ctx)
}

// Snapshot 只读当前规则与探测时间，给健康检查回显。
func (a *Announcer) Snapshot() (rules []webrtc.ICEAddressRewriteRule, probedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rules), a.probedAt
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

	servers := splitTrim(a.stunServers(ctx))
	if len(servers) == 0 {
		servers = DefaultSTUNServers
	}
	rules := announceRules(a.publicIP(ctx), LocalIPs(), servers, a.probe)

	a.mu.Lock()
	old := a.rules
	changed := !rulesEqual(old, rules)
	a.rules = rules
	a.probedAt = time.Now()
	a.lastChanged = changed
	a.probing = false
	close(a.probeDone)
	a.mu.Unlock()

	if changed {
		log.Printf("宣告公网映射变化: %v → %v", RuleExternals(old), RuleExternals(rules))
	}
	return changed
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

func rulesEqual(x, y []webrtc.ICEAddressRewriteRule) bool {
	return slices.EqualFunc(x, y, func(a, b webrtc.ICEAddressRewriteRule) bool {
		return a.Local == b.Local && a.Mode == b.Mode && slices.Equal(a.External, b.External)
	})
}
