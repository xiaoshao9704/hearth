package tlsx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

func TestHTTPHandlerKeepsRecoveryPathsAndRedirectsPermanently(t *testing.T) {
	m := New(t.TempDir(), nil, nil)
	m.cfg = Config{Mode: "selfsigned", HTTPSAddr: ":8443"}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := m.HTTPHandler(fallback)

	for _, path := range []string{"/healthz", "/api/admin/settings"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader("body"))
		req.RemoteAddr = "127.0.0.1:12345"
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s 应直通业务 handler，实际状态码 %d", path, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader("body")))
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("非回环 API 请求不得明文直通，实际 %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://example.com:8080/room?q=1", strings.NewReader("body"))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("普通请求应返回 308，实际 %d", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "https://example.com/room?q=1" {
		t.Fatalf("重定向应使用外部 443 且不暴露内部端口，实际 %q", got)
	}
	m.cfg = Config{Mode: "acme", IPSubject: "2001:db8::1", HTTPSAddr: ":8443"}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://localhost/room", nil))
	if got := rr.Header().Get("Location"); got != "https://[2001:db8::1]/room" {
		t.Fatalf("IPv6 IP 证书重定向必须加方括号，实际 %q", got)
	}

	m.httpChallenge = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/.well-known/acme-challenge/token" {
			return false
		}
		w.WriteHeader(http.StatusOK)
		return true
	}
	rr = httptest.NewRecorder()
	h = m.HTTPHandler(fallback)
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example.com/.well-known/acme-challenge/token", nil))
	if rr.Code == http.StatusPermanentRedirect {
		t.Fatal("ACME HTTP-01 挑战必须先于 HTTPS 重定向处理")
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://example.com/providers/bellows/w/channel", strings.NewReader("offer")))
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("WHIP POST 应以 308 保留方法与请求体，实际 %d", rr.Code)
	}
}

func TestSelfsignedLifecycle(t *testing.T) {
	m := New(t.TempDir(), func() http.Handler { return http.NotFoundHandler() }, func() []string { return nil })

	m.Sync(Config{Mode: "selfsigned", HTTPSAddr: "127.0.0.1:0"})
	st := m.Status()
	if st.LastError != "" {
		t.Fatalf("Sync 报错: %s", st.LastError)
	}
	if !st.Listening || !st.CAAvailable {
		t.Fatalf("应监听且 CA 已生成: %+v", st)
	}
	if !slices.Contains(st.SANs, "localhost") || !slices.Contains(st.SANs, "127.0.0.1") {
		t.Fatalf("SAN 缺 localhost/回环: %v", st.SANs)
	}
	if time.Until(st.NotAfter) < 300*24*time.Hour {
		t.Fatalf("叶子证书有效期应接近一年: %v", st.NotAfter)
	}
	if _, err := m.CACertPEM(); err != nil {
		t.Fatalf("CA 应可下载: %v", err)
	}

	cert, err := m.getCertificate(nil)
	if err != nil || cert.Leaf == nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	// 同配置再 Sync 不重签
	first := st.NotAfter
	m.Sync(Config{Mode: "selfsigned", HTTPSAddr: "127.0.0.1:0"})
	if !m.Status().NotAfter.Equal(first) {
		t.Fatal("同配置 Sync 不应重签")
	}

	// 地址集合变化（externals 新增公网地址）应重签
	m.externals = func() []string { return []string{"203.0.113.10:47700"} }
	m.Sync(Config{Mode: "selfsigned", HTTPSAddr: "127.0.0.1:0"})
	st = m.Status()
	if !slices.Contains(st.SANs, "203.0.113.10") {
		t.Fatalf("SAN 应含新公网地址: %v", st.SANs)
	}

	// 切回 off：listener 停、CA 文件仍在
	m.Sync(Config{Mode: "off"})
	st = m.Status()
	if st.Listening {
		t.Fatal("off 后不应再监听 HTTPS")
	}
	if !st.CAAvailable {
		t.Fatal("CA 文件应保留")
	}
	m.Close()
}

func TestACMEWithoutSubjectKeepsSelfsignedFallback(t *testing.T) {
	m := New(t.TempDir(), func() http.Handler { return http.NotFoundHandler() }, nil)
	m.Sync(Config{Mode: "acme", HTTPSAddr: "127.0.0.1:0"})
	st := m.Status()
	if !st.Listening || st.Active != "selfsigned" || st.LastError == "" {
		t.Fatalf("缺签发标识时应报错但仍以自签名监听: %+v", st)
	}
	m.Close()
}

func TestGetCertificateBeforeLeaf(t *testing.T) {
	m := New(t.TempDir(), func() http.Handler { return http.NotFoundHandler() }, nil)
	if _, err := m.getCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("证书未就绪时应报错")
	}
}

func TestRecordCertificateUpdatesACMEStatus(t *testing.T) {
	m := New(t.TempDir(), nil, nil)
	m.cfg = Config{Mode: "acme", Domain: "example.com"}
	m.status = Status{Mode: "acme", Subject: "example.com", Active: "selfsigned"}
	notAfter := time.Now().Add(30 * 24 * time.Hour).Round(time.Second)
	m.recordCertificate(0, &tls.Certificate{Leaf: &x509.Certificate{
		DNSNames: []string{"example.com"}, IPAddresses: []net.IP{net.ParseIP("203.0.113.10")}, NotAfter: notAfter,
	}}, "example.com", "")
	st := m.Status()
	if st.Active != "acme" || !st.NotAfter.Equal(notAfter) || !slices.Equal(st.SANs, []string{"203.0.113.10", "example.com"}) {
		t.Fatalf("ACME 证书状态未回填: %+v", st)
	}
}

func TestIPSubjectForcesShortlivedProfile(t *testing.T) {
	cfg := Config{Domain: "", IPSubject: "203.0.113.10", ACMEProfile: "classic"}
	if subject := acmeSubject(cfg); subject != "203.0.113.10" || acmeProfile(cfg, subject) != "shortlived" {
		t.Fatalf("IP 标识必须强制 shortlived: subject=%q profile=%q", subject, acmeProfile(cfg, subject))
	}
	cfg.Domain = "example.com"
	if subject := acmeSubject(cfg); subject != "example.com" || acmeProfile(cfg, subject) != "classic" {
		t.Fatalf("域名应优先且保留显式 profile: subject=%q profile=%q", subject, acmeProfile(cfg, subject))
	}
}

func TestACMEFailureKeepsSelfsignedAndBacksOff(t *testing.T) {
	m := New(t.TempDir(), func() http.Handler { return http.NotFoundHandler() }, nil)
	m.manageSync = func(context.Context, *certmagic.Config, []string) error {
		return errors.New("模拟签发失败")
	}
	m.Sync(Config{Mode: "selfsigned", HTTPSAddr: "127.0.0.1:0"})
	listener := m.srv
	m.Sync(Config{Mode: "acme", Domain: "example.com", HTTPAddr: "127.0.0.1:0", HTTPSAddr: "127.0.0.1:0"})
	t.Cleanup(m.Close)
	if m.srv != listener {
		t.Fatal("从自签名切到 ACME 不得重启 HTTPS listener")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		st := m.Status()
		if st.LastError != "" && !st.NextRetry.IsZero() {
			if !st.Listening || st.Active != "selfsigned" {
				t.Fatalf("ACME 失败时 HTTPS 必须由自签名兜底: %+v", st)
			}
			clientConn, serverConn := net.Pipe()
			cert, err := m.getCertificate(&tls.ClientHelloInfo{ServerName: "example.com", Conn: serverConn})
			clientConn.Close()
			serverConn.Close()
			if err != nil || cert == nil || cert.Leaf == nil || cert.Leaf.Issuer.CommonName != "Hearth Local CA" {
				t.Fatalf("ACME 失败后的新握手应取到自签名证书: cert=%v err=%v", cert != nil, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ACME 失败应记录错误与下次重试时间: %+v", m.Status())
}

func TestConcurrentSyncIsSerialized(t *testing.T) {
	m := New(t.TempDir(), func() http.Handler { return http.NotFoundHandler() }, nil)
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			m.Sync(Config{Mode: "selfsigned", HTTPSAddr: "127.0.0.1:0"})
			done <- struct{}{}
		}()
	}
	<-done
	<-done
	if st := m.Status(); st.LastError != "" || !st.Listening {
		t.Fatalf("并发 Sync 不应产生监听假故障: %+v", st)
	}
	m.Close()
}
