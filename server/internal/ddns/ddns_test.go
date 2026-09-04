package ddns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	alidnslib "github.com/libdns/alidns"
	"github.com/libdns/libdns"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func withHTTPClient(t *testing.T, fn roundTripFunc) {
	t.Helper()
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: fn}
	t.Cleanup(func() { http.DefaultClient = old })
}

func response(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
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
	err   error
}

func (f *fakeProvider) Name() string { return "duckdns" }
func (f *fakeProvider) Update(context.Context, string, netip.Addr, netip.Addr) error {
	f.calls.Add(1)
	if f.err != nil {
		return f.err
	}
	if f.fail {
		return errors.New("模拟 API 拒绝")
	}
	return nil
}

func TestRunnerRedactsURLError(t *testing.T) {
	const secret = "change-me-secret"
	fp := &fakeProvider{err: fmt.Errorf("provider 调用失败: %w", &url.Error{
		Op:  "Get",
		URL: "https://example.com/update?AccessKeyId=" + secret + "&Signature=" + secret,
		Err: errors.New("模拟网络错误"),
	})}
	r := NewRunner(filepath.Join(t.TempDir(), "state.json"))
	r.prov = fp

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	r.Sync(context.Background(), Config{Provider: "aliyun", Host: "voice.example.com"}, []string{"203.0.113.10"})

	if got := r.Status().LastError; got != "模拟网络错误" {
		t.Fatalf("LastError 应只保留底层错误，实际 %q", got)
	}
	if got := logs.String(); strings.Contains(got, secret) || strings.Contains(got, "AccessKeyId") || strings.Contains(got, "Signature") {
		t.Fatalf("日志不得包含请求 URL 或凭证: %s", got)
	}
}

func TestRunnerZoneChangeTriggersUpdate(t *testing.T) {
	fp := &fakeProvider{}
	r := NewRunner(filepath.Join(t.TempDir(), "state.json"))
	r.prov = fp
	cfg := Config{Provider: "cloudflare", Host: "voice.example.com", Zone: "example.com"}
	r.Sync(context.Background(), cfg, []string{"203.0.113.10"})
	cfg.Zone = "voice.example.com"
	r.Sync(context.Background(), cfg, []string{"203.0.113.10"})
	if got := fp.calls.Load(); got != 2 {
		t.Fatalf("zone 改变必须绕过地址判重，实际调用 %d 次", got)
	}
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

func TestDuckDNSTransportErrorDoesNotLeakToken(t *testing.T) {
	withHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("模拟网络错误")
	})
	token := "change-me-secret"
	err := (&DuckDNS{Token: token}).Update(context.Background(), "voice.duckdns.org", netip.MustParseAddr("203.0.113.10"), netip.Addr{})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("网络错误不得泄漏 token: %v", err)
	}
}

func TestDuckDNSIPv6OnlyDoesNotRewriteA(t *testing.T) {
	called := false
	withHTTPClient(t, func(*http.Request) (*http.Response, error) {
		called = true
		return response("OK"), nil
	})
	err := (&DuckDNS{Token: "change-me"}).Update(context.Background(), "voice.duckdns.org", netip.Addr{}, netip.MustParseAddr("2001:db8::1"))
	if err == nil || called {
		t.Fatalf("仅 IPv6 时应在发请求前拒绝，避免 DuckDNS 自动改写 A: err=%v called=%v", err, called)
	}
}

func TestRunnerSerializesConcurrentSync(t *testing.T) {
	fp := &fakeProvider{}
	r := NewRunner(filepath.Join(t.TempDir(), "state.json"))
	r.prov = fp
	cfg := Config{Provider: "duckdns", Host: "voice.duckdns.org", DuckDNSToken: "change-me"}
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			<-start
			r.Sync(context.Background(), cfg, []string{"203.0.113.10"})
			done <- struct{}{}
		}()
	}
	close(start)
	<-done
	<-done
	if got := fp.calls.Load(); got != 1 {
		t.Fatalf("并发 Sync 对同一目标只能调用提供方一次，实际 %d", got)
	}
}

func TestDNSPodCreatesMissingRootRecord(t *testing.T) {
	var actions []string
	withHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		action := strings.TrimPrefix(r.URL.Path, "/")
		actions = append(actions, action)
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("domain") != "example.com" || r.Form.Get("sub_domain") != "@" {
			t.Fatalf("主域拆分错误: %v", r.Form)
		}
		if action == "Record.List" {
			return response(`{"status":{"code":"10","message":"没有记录"}}`), nil
		}
		return response(`{"status":{"code":"1","message":"Action completed successful"}}`), nil
	})
	err := (&DNSPod{ID: "id", Token: "change-me"}).Update(context.Background(), "example.com", netip.MustParseAddr("203.0.113.10"), netip.Addr{})
	if err != nil {
		t.Fatalf("缺失的主域记录应进入创建分支: %v", err)
	}
	if got := strings.Join(actions, ","); got != "Record.List,Record.Create" {
		t.Fatalf("调用顺序错误: %s", got)
	}
}

func TestDNSPodUsesExplicitZone(t *testing.T) {
	withHTTPClient(t, func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("domain") != "example.com" || r.Form.Get("sub_domain") != "voice" {
			t.Fatalf("应直接使用显式 zone: %v", r.Form)
		}
		return response(`{"status":{"code":"1","message":"ok"},"records":[{"id":"1","value":"203.0.113.10"}]}`), nil
	})
	err := (&DNSPod{ID: "id", Token: "change-me", Zone: "example.com"}).Update(context.Background(), "voice.example.com", netip.MustParseAddr("203.0.113.10"), netip.Addr{})
	if err != nil {
		t.Fatalf("显式 zone 更新失败: %v", err)
	}
}

func TestDNSPodDomainNotFoundUsesErrorCode(t *testing.T) {
	if !isDomainNotFound(&dpError{Code: "6", Message: "域名不存在"}) {
		t.Fatal("错误码 6 应识别为域名不存在")
	}
	if isDomainNotFound(&dpError{Code: "7", Message: "域名已锁定"}) {
		t.Fatal("不得按文案把其他域名错误当成不存在")
	}
}

type fakeRecordSetter struct {
	zones       []string
	records     [][]libdns.Record
	succeedZone string
}

func (f *fakeRecordSetter) SetRecords(_ context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	f.zones = append(f.zones, zone)
	f.records = append(f.records, append([]libdns.Record(nil), records...))
	if f.succeedZone != "" && zone != f.succeedZone {
		return nil, errors.New("zone 不存在")
	}
	return records, nil
}

func TestLibDNSExplicitZoneCreatesRelativeAddressRecords(t *testing.T) {
	setter := &fakeRecordSetter{}
	p := &libDNSProvider{name: "cloudflare", zone: "example.com", setter: setter}
	v4 := netip.MustParseAddr("203.0.113.10")
	v6 := netip.MustParseAddr("2001:db8::1")
	if err := p.Update(context.Background(), "voice.example.com", v4, v6); err != nil {
		t.Fatal(err)
	}
	if len(setter.zones) != 1 || setter.zones[0] != "example.com" {
		t.Fatalf("应只调用显式 zone 一次: %v", setter.zones)
	}
	if len(setter.records[0]) != 2 {
		t.Fatalf("应同时写 A/AAAA: %v", setter.records[0])
	}
	for i, want := range []netip.Addr{v4, v6} {
		rec, ok := setter.records[0][i].(libdns.Address)
		if !ok || rec.Name != "voice" || rec.IP != want || rec.TTL != ddnsTTL {
			t.Fatalf("记录 %d 不对: %#v", i, setter.records[0][i])
		}
	}
}

func TestLibDNSGuessesZoneWithoutMatchingErrorStrings(t *testing.T) {
	setter := &fakeRecordSetter{succeedZone: "example.co.uk"}
	p := &libDNSProvider{name: "aliyun", setter: setter}
	if err := p.Update(context.Background(), "voice.example.co.uk", netip.MustParseAddr("203.0.113.10"), netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(setter.zones, ","); got != "co.uk,example.co.uk" {
		t.Fatalf("应从最短后缀逐级尝试: %s", got)
	}
}

func TestLibDNSRejectsHostOutsideExplicitZone(t *testing.T) {
	setter := &fakeRecordSetter{}
	p := &libDNSProvider{name: "cloudflare", zone: "example.net", setter: setter}
	err := p.Update(context.Background(), "voice.example.com", netip.MustParseAddr("203.0.113.10"), netip.Addr{})
	if err == nil || len(setter.zones) != 0 {
		t.Fatalf("zone 不匹配应在调用提供方前拒绝: err=%v calls=%v", err, setter.zones)
	}
}

func TestPrepareAliRecordsPreservesExistingID(t *testing.T) {
	existing := []libdns.Record{
		alidnsRecord("record-id", "voice", "A", "203.0.113.9"),
		alidnsRecord("same-id", "voice", "AAAA", "2001:db8::1"),
	}
	desired := []libdns.Record{
		libdns.Address{Name: "voice", TTL: ddnsTTL, IP: netip.MustParseAddr("203.0.113.10")},
		libdns.Address{Name: "voice", TTL: ddnsTTL, IP: netip.MustParseAddr("2001:db8::1")},
	}
	pending, unchanged := prepareAliRecords(existing, desired)
	if len(pending) != 1 || len(unchanged) != 1 {
		t.Fatalf("应只更新变化的记录: pending=%v unchanged=%v", pending, unchanged)
	}
	got, ok := pending[0].(alidnslib.DomainRecord)
	if !ok || got.ID != "record-id" || got.Value != "203.0.113.10" || got.TTL != uint32(ddnsTTL.Seconds()) {
		t.Fatalf("更新记录必须保留上游 ID: %#v", pending[0])
	}
}

func alidnsRecord(id, name, recordType, value string) alidnslib.DomainRecord {
	return alidnslib.DomainRecord{ID: id, Name: name, Type: recordType, Value: value, TTL: uint32(ddnsTTL.Seconds())}
}

type fakeDNSProvider struct{}

func (*fakeDNSProvider) AppendRecords(_ context.Context, _ string, records []libdns.Record) ([]libdns.Record, error) {
	return records, nil
}

func (*fakeDNSProvider) DeleteRecords(_ context.Context, _ string, records []libdns.Record) ([]libdns.Record, error) {
	return records, nil
}

type failingDNSProvider struct{ fakeDNSProvider }

func (*failingDNSProvider) AppendRecords(context.Context, string, []libdns.Record) ([]libdns.Record, error) {
	return nil, &url.Error{Op: "Get", URL: "https://example.com/?AccessKeyId=change-me", Err: errors.New("模拟网络错误")}
}

func TestConfiguredZoneProvider(t *testing.T) {
	p := &configuredZoneProvider{upstream: &fakeDNSProvider{}, zone: "example.com"}
	if _, err := p.AppendRecords(context.Background(), "example.com.", nil); err != nil {
		t.Fatalf("同一 zone 应允许尾点差异: %v", err)
	}
	if _, err := p.AppendRecords(context.Background(), "other.example", nil); err == nil {
		t.Fatal("显式 ddns_zone 与权威 zone 不一致时必须拒绝")
	}
}

func TestConfiguredZoneProviderRedactsURLError(t *testing.T) {
	p := &configuredZoneProvider{upstream: &failingDNSProvider{}, zone: "example.com"}
	_, err := p.AppendRecords(context.Background(), "example.com", nil)
	if err == nil || err.Error() != "模拟网络错误" {
		t.Fatalf("DNS-01 provider 错误不得携带凭证 URL: %v", err)
	}
}

func TestDNSProviderFingerprintChangesWithCredentials(t *testing.T) {
	_, first := NewDNSProvider(Config{Provider: "cloudflare", Zone: "example.com", CFToken: "change-me-a"})
	_, second := NewDNSProvider(Config{Provider: "cloudflare", Zone: "example.com", CFToken: "change-me-b"})
	if first == "" || first == second || strings.Contains(first, "change-me") {
		t.Fatalf("fingerprint 应能识别凭证变化且不暴露原文: first=%q second=%q", first, second)
	}
}
