package lite

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// stubAnnouncer 探测函数替换为假实现；TTL/最小间隔调小以便测试。
func stubAnnouncer(probe func(locals []string) map[string]string, calls *atomic.Int32) *Announcer {
	a := NewAnnouncer(
		func(context.Context) string { return "" },
		func(context.Context) string { return "" },
		nil,
	)
	a.probe = func(locals, _ []string, _ time.Duration) map[string]string {
		calls.Add(1)
		return probe(locals)
	}
	a.ttl = time.Hour
	a.minInterval = time.Nanosecond
	return a
}

func TestAnnouncerRulesCachesWithinTTL(t *testing.T) {
	var calls atomic.Int32
	a := stubAnnouncer(func(locals []string) map[string]string {
		return map[string]string{locals[0]: "203.0.113.5"}
	}, &calls)
	ctx := context.Background()
	a.Rules(ctx)
	a.Rules(ctx)
	if calls.Load() != 1 {
		t.Fatalf("TTL 内不应重复探测，实际 %d 次", calls.Load())
	}
}

func TestAnnouncerRulesRefreshAfterTTL(t *testing.T) {
	var calls atomic.Int32
	a := stubAnnouncer(func(locals []string) map[string]string { return nil }, &calls)
	a.ttl = time.Millisecond
	ctx := context.Background()
	a.Rules(ctx)
	time.Sleep(5 * time.Millisecond)
	a.Rules(ctx)
	if calls.Load() != 2 {
		t.Fatalf("TTL 过期应重探，实际探测 %d 次", calls.Load())
	}
}

func TestAnnouncerRefreshMinInterval(t *testing.T) {
	var calls atomic.Int32
	a := stubAnnouncer(func(locals []string) map[string]string { return nil }, &calls)
	a.minInterval = time.Hour
	ctx := context.Background()
	a.Refresh(ctx)
	if a.Refresh(ctx) {
		t.Fatal("最小间隔内的 Refresh 应直接返回未变")
	}
	if calls.Load() != 1 {
		t.Fatalf("最小间隔内不应重复探测，实际 %d 次", calls.Load())
	}
}

func TestAnnouncerChangedDetection(t *testing.T) {
	var calls atomic.Int32
	ext := "" // 假探测：ext 非空时把首张网卡映射到它
	a := stubAnnouncer(func(locals []string) map[string]string {
		if ext == "" {
			return nil
		}
		return map[string]string{locals[0]: ext}
	}, &calls)
	ctx := context.Background()

	if a.Refresh(ctx) {
		t.Fatal("空→空不应报变化")
	}
	ext = "203.0.113.5"
	if !a.Refresh(ctx) {
		t.Fatal("空→有映射应报变化")
	}
	if a.Refresh(ctx) {
		t.Fatal("映射不变不应报变化")
	}
	ext = "198.51.100.7"
	if !a.Refresh(ctx) {
		t.Fatal("映射变化应报变化")
	}
}

func TestAnnouncerSingleFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	a := NewAnnouncer(
		func(context.Context) string { return "" },
		func(context.Context) string { return "" },
		nil,
	)
	a.probe = func(locals, _ []string, _ time.Duration) map[string]string {
		calls.Add(1)
		<-release
		return nil
	}
	ctx := context.Background()
	done := make(chan bool, 3)
	for range 3 {
		go func() { done <- a.Refresh(ctx) }()
	}
	// 等三个调用者都进入，再放行探测
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range 3 {
		<-done
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("并发刷新应只探测一次，实际 %d 次", n)
	}
}

// ---- 宣告出口 ----

// announcerWith 组一个只做宣告、不真探测的 Announcer：stun 是假的探测结果，mapped 是假的映射查询。
func announcerWith(public string, stun map[string]string, mapped MappedFunc) *Announcer {
	a := NewAnnouncer(
		func(context.Context) string { return public },
		func(context.Context) string { return "" },
		mapped,
	)
	a.probe = func(locals, _ []string, _ time.Duration) map[string]string { return nil }
	a.public, a.stun, a.probedAt = public, stun, time.Now()
	return a
}

func TestAnnouncerExplicitPublicIP(t *testing.T) {
	a := announcerWith("203.0.113.5", nil, func(int) (netip.AddrPort, bool) {
		return netip.MustParseAddrPort("198.51.100.7:33445"), true
	})
	ctx := context.Background()
	if got := a.Announce(ctx, sampleSDP); got != sampleSDP {
		t.Fatalf("显式 public IP 走 pion 改写规则，SDP 出口不应改动:\n%s", got)
	}
	rules := a.Rules(ctx)
	if len(rules) != 1 || rules[0].Local != "" || rules[0].External[0] != "203.0.113.5" ||
		rules[0].Mode != webrtc.ICEAddressRewriteModeUnspecified {
		t.Fatalf("应为 catch-all 替换规则: %+v", rules)
	}
}

func TestAnnouncerMappedAndSTUNSideBySide(t *testing.T) {
	// 映射命中时三类并列：映射地址排最前、STUN IP + 本地端口、STUN IP + 映射端口
	a := announcerWith("", map[string]string{"192.168.50.4": "198.51.100.7"},
		func(port int) (netip.AddrPort, bool) {
			if port != 51973 {
				t.Fatalf("应按 host 候选的端口查映射，实际 %d", port)
			}
			return netip.MustParseAddrPort("203.0.113.5:33445"), true
		})
	got := a.Announce(context.Background(), sampleSDP)
	cands := srflxLines(t, got)
	var addrs []string
	for _, c := range cands {
		addrs = append(addrs, net.JoinHostPort(c.Address(), strconv.Itoa(c.Port())))
	}
	want := []string{"203.0.113.5:33445", "198.51.100.7:51973", "198.51.100.7:33445"}
	if !slices.Equal(addrs, want) {
		t.Fatalf("应并列宣告 %v，实际 %v:\n%s", want, addrs, got)
	}
	if r := cands[0].RelatedAddress(); r == nil || r.Address != "192.168.50.4" {
		t.Fatalf("raddr 应取首个 v4 host 候选，实际 %+v", r)
	}
}

func TestAnnouncerMappedSamePortDedupes(t *testing.T) {
	// 映射端口 == 本地端口且外部 IP 与 STUN 一致：三类合并成一条
	a := announcerWith("", map[string]string{"192.168.50.4": "198.51.100.7"},
		func(int) (netip.AddrPort, bool) { return netip.MustParseAddrPort("198.51.100.7:51973"), true })
	cands := srflxLines(t, a.Announce(context.Background(), sampleSDP))
	if len(cands) != 1 {
		t.Fatalf("同一外部地址应去重为 1 条，实际 %d", len(cands))
	}
}

func TestAnnouncerSTUNFallback(t *testing.T) {
	// 无映射来源：按 STUN 表逐 host 候选追加「公网 IP + 本地端口」
	a := announcerWith("", map[string]string{"192.168.50.4": "198.51.100.7"}, nil)
	got := a.Announce(context.Background(), sampleSDP)
	cands := srflxLines(t, got)
	if len(cands) != 1 {
		t.Fatalf("应追加 1 条，实际 %d 条:\n%s", len(cands), got)
	}
	if cands[0].Address() != "198.51.100.7" || cands[0].Port() != 51973 {
		t.Fatalf("STUN 只给 IP，端口沿用本地监听端口，实际 %s:%d", cands[0].Address(), cands[0].Port())
	}
	// v6 host 候选不在 STUN 表里，原样保留、不追加
	if !strings.Contains(got, "fd08::1 51973 typ host") {
		t.Fatalf("v6 host 候选应原样保留:\n%s", got)
	}
}

func TestAnnouncerMappedMissFallsBackToSTUN(t *testing.T) {
	a := announcerWith("", map[string]string{"192.168.50.4": "198.51.100.7"},
		func(int) (netip.AddrPort, bool) { return netip.AddrPort{}, false })
	cands := srflxLines(t, a.Announce(context.Background(), sampleSDP))
	if len(cands) != 1 || cands[0].Address() != "198.51.100.7" {
		t.Fatalf("映射未命中应回落 STUN，实际 %+v", cands)
	}
}

func TestAnnouncerNoSourcesLeavesSDP(t *testing.T) {
	a := announcerWith("", nil, nil)
	if got := a.Announce(context.Background(), sampleSDP); got != sampleSDP {
		t.Fatalf("无外部地址来源时不应改动 SDP:\n%s", got)
	}
}

func TestAnnouncerSnapshot(t *testing.T) {
	a := announcerWith("", map[string]string{"192.168.50.4": "198.51.100.7", "10.0.0.2": "198.51.100.7"},
		func(int) (netip.AddrPort, bool) { return netip.MustParseAddrPort("203.0.113.5:33445"), true })
	if ext, _ := a.Snapshot(); !slices.Equal(ext, []string{"198.51.100.7"}) {
		t.Fatalf("未见过 SDP 时只回显 STUN 结果（同出口去重），实际 %v", ext)
	}
	a.Announce(context.Background(), sampleSDP) // 记下媒体端口
	ext, _ := a.Snapshot()
	if !slices.Equal(ext, []string{"203.0.113.5:33445", "198.51.100.7"}) {
		t.Fatalf("映射结果应排在 STUN 之前，实际 %v", ext)
	}
}
