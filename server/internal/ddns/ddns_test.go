package ddns

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// 阿里云签名：固定 key+参数（含固定 nonce/时间戳）必须得到稳定的签名串。
// 期望值由同一算法的独立实现（Python hmac）算出，防「实现改了但自洽」的假绿。
func TestAliyunSignStable(t *testing.T) {
	params := map[string]string{
		"Format":           "JSON",
		"Version":          "2015-01-09",
		"AccessKeyId":      "test-key-id",
		"SignatureMethod":  "HMAC-SHA1",
		"Timestamp":        "2026-01-02T03:04:05Z",
		"SignatureVersion": "1.0",
		"SignatureNonce":   "abc123",
		"Action":           "DescribeDomainRecords",
		"DomainName":       "example.com",
		"RRKeyWord":        "voice",
		"TypeKeyWord":      "A",
		"PageSize":         "100",
	}
	got := aliSign("test-secret", params)
	want := "a+7aK6N+Q7vyGNcbUKoqtI/bITQ="
	if got != want {
		t.Fatalf("签名不稳定: got %s, want %s", got, want)
	}
}

func TestSplitExternals(t *testing.T) {
	v4, v6 := splitExternals([]string{"203.0.113.10:47700", "192.168.1.2", "[2001:db8::1]:7881", "10.0.0.1"})
	if v4.String() != "203.0.113.10" {
		t.Fatalf("v4 应剥掉端口并跳过私网: %v", v4)
	}
	if v6.String() != "2001:db8::1" {
		t.Fatalf("v6 应剥掉端口: %v", v6)
	}
	v4, v6 = splitExternals(nil)
	if v4.IsValid() || v6.IsValid() {
		t.Fatal("空快照应得零值")
	}
}

// fakeProvider 记录调用的测试提供方（单测不打真实 API）。
type fakeProvider struct {
	calls atomic.Int32
	fail  bool
}

func (f *fakeProvider) Name() string { return "duckdns" }
func (f *fakeProvider) Update(context.Context, string, netip.Addr, netip.Addr) error {
	f.calls.Add(1)
	if f.fail {
		return errors.New("模拟 API 拒绝")
	}
	return nil
}

func TestRunnerDedupBackoffAndState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "ddns-state.json")
	cfg := Config{Provider: "duckdns", Host: "voice.duckdns.org", DuckDNSToken: "x"}
	ext := []string{"203.0.113.10:47700"}

	// off 时不动作
	fp := &fakeProvider{}
	r := NewRunner(statePath)
	r.prov = fp
	r.Sync(context.Background(), Config{Provider: "off"}, ext)
	if st := r.Status(); st.Provider != "off" || st.LastError != "" {
		t.Fatalf("off 应安静: %+v", st)
	}
	if fp.calls.Load() != 0 {
		t.Fatal("off 不应调提供方")
	}

	// 凭证不全（未注入提供方时）视为未启用，状态说明原因
	r2 := NewRunner(statePath)
	r2.Sync(context.Background(), Config{Provider: "duckdns", Host: "voice.duckdns.org"}, ext)
	if r2.Status().LastError == "" {
		t.Fatal("缺 token 应在状态里说明")
	}

	// 首次推送：地址非空即调一次
	r.Sync(context.Background(), cfg, ext)
	if fp.calls.Load() != 1 {
		t.Fatalf("首次应推送一次，实际 %d", fp.calls.Load())
	}
	st := r.Status()
	if st.LastError != "" || st.V4 != "203.0.113.10" || st.UpdatedAt.IsZero() {
		t.Fatalf("推送成功后状态不对: %+v", st)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatal("成功推送应落 ddns-state.json")
	}

	// 同地址再 Sync：去重，不打 API
	r.Sync(context.Background(), cfg, ext)
	if fp.calls.Load() != 1 {
		t.Fatal("地址未变不应重推")
	}

	// 模拟重启：新 Runner 读回状态文件，同地址也不重推
	r3 := NewRunner(statePath)
	r3.prov = fp
	r3.Sync(context.Background(), cfg, ext)
	if fp.calls.Load() != 1 {
		t.Fatal("重启后地址未变不应重推")
	}

	// 地址变化：推送
	r3.Sync(context.Background(), cfg, []string{"203.0.113.99"})
	if fp.calls.Load() != 2 {
		t.Fatal("地址变化应触发推送")
	}

	// 失败：进状态并挂退避；退避窗口内换地址也不重试
	fp.fail = true
	r3.Sync(context.Background(), cfg, []string{"203.0.113.100"})
	if fp.calls.Load() != 3 {
		t.Fatal("地址变化应推送（即使会失败）")
	}
	if s := r3.Status(); s.LastError == "" || s.NextRetry.IsZero() {
		t.Fatalf("失败应进状态并挂退避: %+v", s)
	}
	r3.Sync(context.Background(), cfg, []string{"203.0.113.101"})
	if fp.calls.Load() != 3 {
		t.Fatal("退避窗口内不应重试")
	}
}

func TestSplitHost(t *testing.T) {
	domain, sub := splitHost("voice.example.com", 2)
	if domain != "example.com" || sub != "voice" {
		t.Fatalf("got %s / %s", domain, sub)
	}
	domain, sub = splitHost("example.com", 2)
	if domain != "example.com" || sub != "" {
		t.Fatalf("主域本身: got %s / %q", domain, sub)
	}
	domain, sub = splitHost("a.b.example.co.uk", 3)
	if domain != "example.co.uk" || sub != "a.b" {
		t.Fatalf("三段主域: got %s / %s", domain, sub)
	}
}
