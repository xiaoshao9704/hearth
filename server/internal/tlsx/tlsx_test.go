package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"
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

	m.acme = &autocert.Manager{}
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

func TestACMERequiresDomain(t *testing.T) {
	m := New(t.TempDir(), func() http.Handler { return http.NotFoundHandler() }, nil)
	m.Sync(Config{Mode: "acme", HTTPSAddr: "127.0.0.1:0"})
	st := m.Status()
	if st.Listening || st.LastError == "" {
		t.Fatalf("缺域名应报错且不监听: %+v", st)
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
	notAfter := time.Now().Add(30 * 24 * time.Hour).Round(time.Second)
	m.recordCertificate(&tls.Certificate{Leaf: &x509.Certificate{DNSNames: []string{"example.com"}, NotAfter: notAfter}})
	st := m.Status()
	if !st.NotAfter.Equal(notAfter) || !slices.Equal(st.SANs, []string{"example.com"}) {
		t.Fatalf("ACME 证书状态未回填: %+v", st)
	}
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
