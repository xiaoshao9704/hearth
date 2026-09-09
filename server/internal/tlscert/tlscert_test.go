package tlscert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// nameConstraintsOID RFC 5280 nameConstraints，测试用来断言这个扩展存在且 critical。
var nameConstraintsOID = []int{2, 5, 29, 30}

func selfStore(t *testing.T, set *Settings) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tls")
	return New(dir, func(context.Context) Settings { return *set })
}

func currentLeaf(t *testing.T, s *Store) *x509.Certificate {
	t.Helper()
	c, err := s.GetCertificate(nil)
	if err != nil {
		t.Fatalf("没有可用证书: %v", err)
	}
	return c.Leaf
}

func caOf(t *testing.T, s *Store) *x509.Certificate {
	t.Helper()
	pemBytes, err := s.CACertPEM()
	if err != nil {
		t.Fatalf("读根证书失败: %v", err)
	}
	blk, _ := pem.Decode(pemBytes)
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析根证书失败: %v", err)
	}
	return ca
}

func caKeyOf(t *testing.T, s *Store) *ecdsa.PrivateKey {
	t.Helper()
	b, err := os.ReadFile(s.Path("ca.key"))
	if err != nil {
		t.Fatalf("读根私钥失败: %v", err)
	}
	blk, _ := pem.Decode(b)
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		t.Fatalf("解析根私钥失败: %v", err)
	}
	return key
}

// TestSelfNameConstraints 自签根必须带 critical 的名称约束，且用它签出来的域名证书验不过——
// 这是「装到别人设备上的根不能被滥用」的核心保证。
func TestSelfNameConstraints(t *testing.T) {
	set := Settings{Source: SourceSelf}
	s := selfStore(t, &set)
	s.Check(context.Background())

	ca := caOf(t, s)
	var found, critical bool
	for _, ext := range ca.Extensions {
		if slices.Equal([]int(ext.Id), nameConstraintsOID) {
			found, critical = true, ext.Critical
		}
	}
	if !found || !critical {
		t.Fatalf("根证书应带 critical 的名称约束: found=%v critical=%v", found, critical)
	}
	if !slices.Contains(ca.PermittedDNSDomains, "localhost") {
		t.Fatalf("名称约束应允许 localhost: %v", ca.PermittedDNSDomains)
	}
	if ca.MaxPathLen != 0 || !ca.MaxPathLenZero {
		t.Fatal("根证书不应允许再签中间 CA")
	}
	if ca.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Fatalf("根证书的 KeyUsage 只应有签证书/签 CRL: %v", ca.KeyUsage)
	}

	// 私钥泄漏的场景：拿根私钥签一张 example.com，任何遵守名称约束的验证方都会拒绝。
	leaf := signWith(t, ca, caKeyOf(t, s), "example.com")
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "example.com"}); err == nil {
		t.Fatal("用带名称约束的根签出的 example.com 证书不该验得过")
	} else if !strings.Contains(err.Error(), "not authorized") && !strings.Contains(err.Error(), "permitted") {
		t.Fatalf("应因名称约束被拒，实际: %v", err)
	}

	// 自己签的叶必须验得过（SAN 里的 localhost 与本机 IP 都在约束允许范围内）。
	if _, err := currentLeaf(t, s).Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatalf("自签叶证书应能通过校验: %v", err)
	}
}

// TestSelfResignOnSANChange SAN 集合变化即重签叶证书，根不动。
func TestSelfResignOnSANChange(t *testing.T) {
	set := Settings{Source: SourceSelf}
	s := selfStore(t, &set)
	ctx := context.Background()
	s.Check(ctx)
	first := currentLeaf(t, s)
	caFP := s.Status().CA.FingerprintSHA256

	// 集合没变：不重签。
	s.Check(ctx)
	if currentLeaf(t, s).SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("SAN 没变不应重签")
	}

	set.External = []string{"203.0.113.5:47720"} // 探测到新的外部地址（带端口，取 IP）
	s.Check(ctx)
	leaf := currentLeaf(t, s)
	if leaf.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("外部地址变化应重签叶证书")
	}
	if !slices.Contains(sansOf(leaf), "203.0.113.5") {
		t.Fatalf("新叶证书应含新的外部地址: %v", sansOf(leaf))
	}
	if s.Status().CA.FingerprintSHA256 != caFP {
		t.Fatal("重签叶证书不应换根")
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > 398*24*time.Hour {
		t.Fatalf("叶证书有效期应在 397 天档: %v", leaf.NotAfter.Sub(leaf.NotBefore))
	}
}

// TestSelfConstraintStale 新增的 DNS 名不在根的名称约束里时只报告、不悄悄放宽，
// 也不写进叶证书（名称约束是整证书判定，混进去会让整张证书失效）。
func TestSelfConstraintStale(t *testing.T) {
	set := Settings{Source: SourceSelf}
	s := selfStore(t, &set)
	ctx := context.Background()
	s.Check(ctx)
	if s.Status().CA.ConstraintStale {
		t.Fatal("初始状态不该是 stale")
	}

	set.SelfHosts = []string{"hearth.example.com"}
	s.Check(ctx)
	if !s.Status().CA.ConstraintStale {
		t.Fatal("新增根约束外的 DNS 名后应报告 constraint_stale")
	}
	if slices.Contains(sansOf(currentLeaf(t, s)), "hearth.example.com") {
		t.Fatal("约束外的 DNS 名不该写进叶证书")
	}

	// 重新生成根：新根的约束涵盖它，stale 消失，叶证书也带上了。
	if err := s.RotateCA(ctx); err != nil {
		t.Fatalf("重新生成根证书失败: %v", err)
	}
	if s.Status().CA.ConstraintStale {
		t.Fatal("重新生成根后不该再 stale")
	}
	if !slices.Contains(sansOf(currentLeaf(t, s)), "hearth.example.com") {
		t.Fatalf("新根签的叶证书应含登记的主机名: %v", sansOf(currentLeaf(t, s)))
	}
}

// TestRotateCAInvalidatesOldLeaf 轮换后旧叶由旧根签发，对新根验不过（装过旧根的设备要重装）。
func TestRotateCAInvalidatesOldLeaf(t *testing.T) {
	set := Settings{Source: SourceSelf}
	s := selfStore(t, &set)
	ctx := context.Background()
	s.Check(ctx)
	oldLeaf := currentLeaf(t, s)
	oldFP := s.Status().CA.FingerprintSHA256

	if err := s.RotateCA(ctx); err != nil {
		t.Fatalf("重新生成根证书失败: %v", err)
	}
	newFP := s.Status().CA.FingerprintSHA256
	if newFP == oldFP {
		t.Fatal("重新生成根证书后指纹应变化")
	}
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t, s))
	if _, err := oldLeaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err == nil {
		t.Fatal("旧叶证书不该能被新根验过")
	}
	if _, err := currentLeaf(t, s).Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatalf("新叶证书应能被新根验过: %v", err)
	}
}

// TestFileLoaderHotSwapAndBadFile file 来源：文件变了热换；坏文件保留上一张证书。
func TestFileLoaderHotSwap(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	writeSelfSigned(t, certPath, keyPath, "one.example.com")

	set := Settings{Source: SourceFile, CertFile: certPath, KeyFile: keyPath}
	s := New(filepath.Join(dir, "tls"), func(context.Context) Settings { return set })
	ctx := context.Background()
	s.Check(ctx)
	first := currentLeaf(t, s)
	if !slices.Contains(first.DNSNames, "one.example.com") {
		t.Fatalf("应加载指定路径的证书: %v", first.DNSNames)
	}
	if s.Status().CA != nil {
		t.Fatal("外部证书来源不该回显根证书信息")
	}

	// 覆盖成另一张（模拟外部工具续期），mtime 变化即热换。
	writeSelfSigned(t, certPath, keyPath, "two.example.com")
	touchLater(t, certPath, keyPath)
	s.Check(ctx)
	if !slices.Contains(currentLeaf(t, s).DNSNames, "two.example.com") {
		t.Fatal("文件变化后应热换成新证书")
	}

	// 写坏文件：保留上一张证书，绝不把 HTTPS 拉死。
	os.WriteFile(certPath, []byte("not a pem"), 0o600)
	touchLater(t, certPath, keyPath)
	s.Check(ctx)
	if !slices.Contains(currentLeaf(t, s).DNSNames, "two.example.com") {
		t.Fatal("坏证书文件应保留上一张可用证书")
	}
}

// TestSourceOff off 来源不生成任何文件，也不持证书。
func TestSourceOff(t *testing.T) {
	set := Settings{Source: SourceOff}
	s := selfStore(t, &set)
	s.Check(context.Background())
	if _, err := s.GetCertificate(nil); err == nil {
		t.Fatal("off 时不该有可用证书")
	}
	if _, err := os.Stat(s.Path("ca.crt")); !os.IsNotExist(err) {
		t.Fatal("off 时不该生成任何证书文件")
	}
	if _, err := s.CACertPEM(); err == nil {
		t.Fatal("off 时 /ca.crt 应取不到根证书")
	}
}

// TestSourceSwitch 来源切换即时生效：self→off 丢掉证书，off→self 再生成。
func TestSourceSwitch(t *testing.T) {
	set := Settings{Source: SourceSelf}
	s := selfStore(t, &set)
	ctx := context.Background()
	s.Check(ctx)
	currentLeaf(t, s)

	set.Source = SourceOff
	s.Check(ctx)
	if _, err := s.GetCertificate(nil); err == nil {
		t.Fatal("切到 off 后不该还有可用证书")
	}
	set.Source = SourceSelf
	s.Check(ctx)
	currentLeaf(t, s)
}

// ---- 测试工具 ----

// signWith 用给定的 CA 签一张域名叶证书（模拟根私钥泄漏后的滥用）。
func signWith(t *testing.T, ca *x509.Certificate, key *ecdsa.PrivateKey, dnsName string) *x509.Certificate {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sn, _ := serial()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// writeSelfSigned 造一对自签的证书文件，给 file 来源当外部证书用。
func writeSelfSigned(t *testing.T, certPath, keyPath, dnsName string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sn, _ := serial()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{dnsName},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		t.Fatal(err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER); err != nil {
		t.Fatal(err)
	}
}

// touchLater 把 mtime 推到未来：同一秒内连写两次时文件系统的时间戳可能不变，
// 而热换判定就看 mtime。
func touchLater(t *testing.T, paths ...string) {
	t.Helper()
	future := time.Now().Add(time.Minute)
	for _, p := range paths {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}
}
