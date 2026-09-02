package lite

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// stubAnnouncer 探测函数替换为假实现；TTL/最小间隔调小以便测试。
func stubAnnouncer(probe func(locals []string) map[string]string, calls *atomic.Int32) *Announcer {
	a := NewAnnouncer(
		func(context.Context) string { return "" },
		func(context.Context) string { return "" },
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
	r1 := a.Rules(ctx)
	r2 := a.Rules(ctx)
	if calls.Load() != 1 {
		t.Fatalf("TTL 内不应重复探测，实际 %d 次", calls.Load())
	}
	if !rulesEqual(r1, r2) {
		t.Fatal("TTL 内两次 Rules 应返回同一缓存")
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

func TestLoopbackRemote(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:43210":  true,
		"[::1]:43210":      true,
		"10.0.0.8:43210":   false,
		"203.0.113.9:8080": false,
		"garbage":          false,
	} {
		if got := LoopbackRemote(addr); got != want {
			t.Errorf("LoopbackRemote(%q) = %v, 期望 %v", addr, got, want)
		}
	}
}
