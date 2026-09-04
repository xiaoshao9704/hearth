package tlsx

import (
	"crypto/tls"
	"net/http"
	"slices"
	"testing"
	"time"
)

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
