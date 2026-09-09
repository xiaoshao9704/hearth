package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	caValidity   = 10 * 365 * 24 * time.Hour // 根 10 年：装到别人设备上的信任锚，不能频繁换
	leafValidity = 397 * 24 * time.Hour      // 叶 397 天：在各系统对 TLS 叶证书 825 天上限之内
	leafRenewAt  = 30 * 24 * time.Hour       // 到期前 30 天重签
)

// checkSelf 自签来源的一轮检查：根 CA 不在就生成（之后永不自动重签），叶证书按
// SAN 集合与剩余有效期决定是否重签。
func (s *Store) checkSelf(set Settings) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		s.logOnce("tls: 创建证书目录失败: " + err.Error())
		return
	}
	ca, caKey, created, err := s.loadOrCreateCA(set)
	if err != nil {
		s.logOnce("tls: 根证书不可用: " + err.Error())
		return
	}
	if created {
		log.Printf("tls: 已生成根证书（指纹 %s，到期 %s）", fingerprint(ca.Raw), ca.NotAfter.Format(time.RFC3339))
	}
	dnsWant, ipWant, stale := desiredSANs(set, ca)
	caInfo := &CAInfo{FingerprintSHA256: fingerprint(ca.Raw), NotAfter: ca.NotAfter, ConstraintStale: stale}

	pair, leaf, err := loadPair(s.Path("self.crt"), s.Path("self.key"))
	if err == nil && !leafOutdated(leaf, ca, dnsWant, ipWant) {
		s.apply(pair, leaf, caInfo)
		return
	}
	newPair, newLeaf, err := s.issueLeaf(ca, caKey, dnsWant, ipWant)
	if err != nil {
		s.logOnce("tls: 签发自签证书失败: " + err.Error())
		// 旧叶还在就继续用它，只是 SAN 可能不全。
		if pair != nil {
			s.apply(pair, leaf, caInfo)
		}
		return
	}
	log.Printf("tls: 已签发自签证书（SAN %s）", strings.Join(sansOf(newLeaf), ", "))
	s.apply(newPair, newLeaf, caInfo)
}

// loadOrCreateCA 读现有根 CA，不存在则生成。created 表示本次新建。
func (s *Store) loadOrCreateCA(set Settings) (ca *x509.Certificate, key *ecdsa.PrivateKey, created bool, err error) {
	certPath, keyPath := s.Path("ca.crt"), s.Path("ca.key")
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, key, err = parseCA(certPEM, keyPEM)
		if err == nil {
			return ca, key, false, nil
		}
		return nil, nil, false, err
	}
	if !os.IsNotExist(certErr) && certErr != nil {
		return nil, nil, false, certErr
	}
	if !os.IsNotExist(keyErr) && keyErr != nil {
		return nil, nil, false, keyErr
	}
	ca, key, err = createCA(dnsHosts(set.SelfHosts))
	if err != nil {
		return nil, nil, false, err
	}
	if err := writePEM(certPath, "CERTIFICATE", ca.Raw); err != nil {
		return nil, nil, false, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, false, err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", der); err != nil {
		return nil, nil, false, err
	}
	return ca, key, true, nil
}

// createCA 生成根 CA。名称约束（critical）把这把根钉死在「只为本机签服务器证书」上：
// DNS 只允许 localhost 与管理员登记的主机名，邮件/URI 各给一个不可能匹配的占位以关掉，
// IP 全放行（自签的用途就是按 IP 访问，且冒充按 IP 访问的站点没有实际价值）。
// KeyUsage 只签证书、MaxPathLen=0 不能再签中间 CA。
func createCA(dnsNames []string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	_, v4, _ := net.ParseCIDR("0.0.0.0/0")
	_, v6, _ := net.ParseCIDR("::/0")
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "Hearth CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,

		PermittedDNSDomainsCritical: true, // 这个字段控制整个 nameConstraints 扩展的 critical 位
		PermittedDNSDomains:         append([]string{"localhost"}, dnsNames...),
		PermittedIPRanges:           []*net.IPNet{v4, v6},
		PermittedEmailAddresses:     []string{"invalid"},
		PermittedURIDomains:         []string{"invalid"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return ca, key, nil
}

// issueLeaf 用根签一张服务器证书并落盘。
func (s *Store) issueLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsNames []string, ips []net.IP) (*tls.Certificate, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "hearth"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(s.Path("self.crt"), "CERTIFICATE", der); err != nil {
		return nil, nil, err
	}
	if err := writePEM(s.Path("self.key"), "EC PRIVATE KEY", keyDER); err != nil {
		return nil, nil, err
	}
	// 链上带根：装过根的设备之外，好奇的客户端也能看到完整链。
	pair := &tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key, Leaf: leaf}
	return pair, leaf, nil
}

// leafOutdated 叶证书是否需要重签：过期在即、SAN 集合变了、或不是当前根签的。
func leafOutdated(leaf, ca *x509.Certificate, dnsWant []string, ipWant []net.IP) bool {
	if time.Now().Add(leafRenewAt).After(leaf.NotAfter) {
		return true
	}
	if leaf.CheckSignatureFrom(ca) != nil {
		return true
	}
	if !slices.Equal(sortedStrings(leaf.DNSNames), dnsWant) {
		return true
	}
	return !slices.Equal(ipStrings(leaf.IPAddresses), ipStrings(ipWant))
}

// desiredSANs 叶证书该带的 SAN：localhost + 127.0.0.1 + 本机全部非回环单播地址 +
// 宣告探测到的外部地址 + tls_self_hosts。stale 表示配置里有根证书名称约束不允许的
// DNS 名——这类名字不会写进叶证书：名称约束是整证书判定，混进一个不被允许的名字会让
// 整张证书对所有人失效，所以只报告、不放进去。
func desiredSANs(set Settings, ca *x509.Certificate) (dns []string, ips []net.IP, stale bool) {
	names := map[string]bool{"localhost": true}
	for _, h := range dnsHosts(set.SelfHosts) {
		if permittedDNS(ca, h) {
			names[h] = true
		} else {
			stale = true
		}
	}
	addrs := map[string]bool{"127.0.0.1": true}
	for _, ip := range localIPs() {
		addrs[ip] = true
	}
	for _, e := range set.External {
		if ip := hostIP(e); ip != "" {
			addrs[ip] = true
		}
	}
	for _, h := range set.SelfHosts {
		if ip := net.ParseIP(strings.TrimSpace(h)); ip != nil {
			addrs[ip.String()] = true
		}
	}
	for n := range names {
		dns = append(dns, n)
	}
	for a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			ips = append(ips, ip)
		}
	}
	slices.Sort(dns)
	slices.SortFunc(ips, func(a, b net.IP) int { return strings.Compare(a.String(), b.String()) })
	return dns, ips, stale
}

// permittedDNS 根证书的名称约束是否允许这个 DNS 名（约束项本身与其子域均允许）。
func permittedDNS(ca *x509.Certificate, name string) bool {
	if len(ca.PermittedDNSDomains) == 0 {
		return true
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, d := range ca.PermittedDNSDomains {
		d = strings.ToLower(strings.TrimPrefix(d, "."))
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// dnsHosts 从 tls_self_hosts 里挑出主机名（IP 走 SAN 的 IP 分支，不进名称约束的 DNS 项）。
func dnsHosts(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || net.ParseIP(h) != nil {
			continue
		}
		out = append(out, strings.ToLower(h))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// localIPs 本机全部非回环单播地址（v4/v6，含私网）。取不到就算了，SAN 少几条不致命。
func localIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || !n.IP.IsGlobalUnicast() {
				continue
			}
			out = append(out, n.IP.String())
		}
	}
	return out
}

// hostIP 从「IP」或「IP:端口」里取 IP，取不到返回空串。
func hostIP(s string) string {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
}

// ---- 小工具 ----

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func writePEM(path, blockType string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600)
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("根证书或私钥不是合法 PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// loadPair 读一对证书文件；任一步失败都返回错误，由调用方决定重签还是保留旧证书。
func loadPair(certPath, keyPath string) (*tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	pair.Leaf = leaf
	return &pair, leaf, nil
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

func sansOf(leaf *x509.Certificate) []string {
	out := append([]string(nil), leaf.DNSNames...)
	return append(out, ipStrings(leaf.IPAddresses)...)
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}
