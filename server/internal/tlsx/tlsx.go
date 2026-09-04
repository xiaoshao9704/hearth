// Package tlsx 进程内 TLS：三种模式（off / acme / selfsigned）由动态配置 tls_mode 决定。
//
//   - off：现状，HTTPS 不监听，HTTP 正常服务（反代在前的部署形态）。
//   - acme：CertMagic 按 site_domain 或探测到的公网 IP 自动签发，证书与
//     ACME 账户落 <data>/certs；签发前及任何 ACME 失败期间始终用本地自签名证书兜底。
//   - selfsigned：本地 CA（<data>/certs/ca.{crt,key}，10 年）+ 叶子证书（1 年，到期自动
//     续签）；SAN 覆盖 localhost、回环、本机 LAN 地址、探测到的公网地址与 site_domain，
//     地址集合变化即重签。它既可独立选用，也是 ACME 路径永远在场的兜底。
//
// 热切换取舍：tls_mode 切模式时 HTTP listener 不重启——主机的 HTTP handler 是一个按当前
// 模式分流的壳（off 时透传业务 handler，否则只做 ACME 挑战 + 308）；HTTPS listener 由
// Sync 按配置 reconcile（起/停/换地址）。换证书不需要动 listener：GetCertificate 回调
// 每次握手现取。Sync 幂等，配置保存后、宣告探测刷新后各调一次即可。
package tlsx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

// 证书参数写死，不开配置项。
const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 365 * 24 * time.Hour
	// leafRenewBefore 叶子证书距到期不足这个时长就重签（进程常年开着也要能自己续）。
	leafRenewBefore = 30 * 24 * time.Hour
	acmeIPDebounce  = 30 * time.Second
	acmeRetryMin    = time.Minute
	acmeRetryMax    = 10 * time.Minute
)

// Config 一次 Sync 看到的 TLS 相关动态配置快照。
type Config struct {
	Mode           string // off / acme / selfsigned
	Domain         string // site_domain
	IPSubject      string // netcheck 确认公网可直连的 IP，仅 Domain 空时使用
	HTTPAddr       string // HTTP 监听地址，供 HTTP-01 复用现有 listener
	HTTPSAddr      string // HTTPS 监听地址，供 TLS-ALPN-01 复用现有 listener
	ACMEDir        string // ACME 目录，空 = CertMagic 默认（Let's Encrypt 生产）
	ACMEEmail      string // 账户邮箱，可空
	ACMEProfile    string // 空 = CA 默认；IP 标识会强制 shortlived
	DNSProvider    certmagic.DNSProvider
	DNSProviderKey string // provider/zone/凭证指纹，只用于判断配置变化
}

// Status 证书与监听状态，进管理后台与自检回显。
type Status struct {
	Mode        string    `json:"mode"`
	Active      string    `json:"active"` // 当前实际握手使用：off / selfsigned / acme
	Domain      string    `json:"domain"`
	Subject     string    `json:"subject"` // ACME 签发标识（域名或 IP）
	Profile     string    `json:"profile"` // 当前 ACME profile
	HTTPSAddr   string    `json:"https_addr"`
	Listening   bool      `json:"listening"`    // HTTPS listener 在跑
	SANs        []string  `json:"sans"`         // 当前实际叶子证书的 SAN
	NotAfter    time.Time `json:"not_after"`    // 叶子证书到期时间（未知为零值）
	CAAvailable bool      `json:"ca_available"` // 本地 CA 已生成（可下载）
	LastError   string    `json:"last_error"`   // 上次签发/续签/监听错误
	NextRetry   time.Time `json:"next_retry"`   // ACME 失败后的下次尝试时间
}

// Manager 持有模式状态、证书材料与 HTTPS listener。全部导出方法并发安全。
type Manager struct {
	dir     string              // <data>/certs
	handler func() http.Handler // 业务 handler（HTTPS 与 off 模式的 HTTP 共用）
	// externals 宣告探测快照（公网地址，可能带端口），selfsigned 的 SAN 来源之一
	externals func() []string

	syncMu sync.Mutex // 串行化多个触发源的完整 reconcile，避免重复监听同一地址
	mu     sync.Mutex
	cfg    Config
	cache  *certmagic.Cache
	magic  *certmagic.Config
	// httpChallenge 指向当前 issuer 的 HTTP-01 处理器，也作为不走公网的分流单测注入点。
	httpChallenge func(http.ResponseWriter, *http.Request) bool
	// manageSync 是单测注入点；nil 时调 CertMagic ManageSync。
	manageSync     func(context.Context, *certmagic.Config, []string) error
	acmeCancel     context.CancelFunc
	acmeGeneration uint64
	acmeWG         sync.WaitGroup
	leaf           *tls.Certificate // selfsigned 当前叶子证书
	sans           []string         // 当前叶子证书覆盖的 SAN 集合（重签判据）
	srv            *http.Server     // HTTPS listener（off 时为 nil）
	status         Status
}

// New dir 是证书目录（<data>/certs）；handler 与 externals 取值函数随后续调用现取，
// 方便调用方在路由建好后一次性注入、之后不再动。
func New(dir string, handler func() http.Handler, externals func() []string) *Manager {
	return &Manager{dir: dir, handler: handler, externals: externals}
}

// Sync 按当前配置 reconcile：模式切换起/停 HTTPS listener，selfsigned 下生成/重签
// 证书，acme 下异步交给 CertMagic 签发。幂等；任何 ACME 失败都只落状态，
// HTTPS 始终由本地自签名叶子证书兜底。
func (m *Manager) Sync(cfg Config) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	if cfg.Mode == "" {
		cfg.Mode = "off"
	}
	m.mu.Lock()
	previousCfg, previousStatus := m.cfg, m.status
	sameCfg := sameConfig(previousCfg, cfg)
	m.cfg = cfg
	m.mu.Unlock()

	st := Status{Mode: cfg.Mode, Domain: cfg.Domain, HTTPSAddr: cfg.HTTPSAddr, Active: "off"}

	switch cfg.Mode {
	case "off":
		m.stopACME()
		m.stopHTTPS()
		m.setStatus(st)
	case "acme":
		if err := m.ensureLeaf(); err != nil {
			st.LastError = "自签名兜底证书生成失败: " + err.Error()
		}
		m.mu.Lock()
		leaf := m.leaf
		m.mu.Unlock()
		if leaf != nil {
			st.Active = "selfsigned"
			st.SANs = leafSANs(leaf)
			st.NotAfter = leaf.Leaf.NotAfter
		}
		st.Subject = acmeSubject(cfg)
		st.Profile = acmeProfile(cfg, st.Subject)
		if sameCfg && previousStatus.Subject == st.Subject {
			st.LastError = previousStatus.LastError
			st.NextRetry = previousStatus.NextRetry
			if previousStatus.Active == "acme" {
				st.Active = previousStatus.Active
				st.SANs = slices.Clone(previousStatus.SANs)
				st.NotAfter = previousStatus.NotAfter
			}
		}
		st.Listening = m.ensureHTTPS()
		if st.LastError == "" && !st.Listening {
			st.LastError = m.lastErr()
		}
		if st.Subject == "" {
			m.stopACME()
			if st.LastError == "" {
				st.LastError = "没有可签发的标识：请填 site_domain，或等待 netcheck 确认公网 IP 可直连"
			}
		}
		m.setStatus(st)
		if st.Subject != "" && (!sameCfg || m.acmeConfig() == nil) {
			m.configureACME(cfg, st.Subject)
		}
	case "selfsigned":
		m.stopACME()
		if err := m.ensureLeaf(); err != nil {
			st.LastError = err.Error()
		}
		m.mu.Lock()
		leaf := m.leaf
		m.mu.Unlock()
		if leaf != nil {
			st.Active = "selfsigned"
			st.SANs = leafSANs(leaf)
			st.NotAfter = leaf.Leaf.NotAfter
		}
		st.Listening = m.ensureHTTPS()
		if st.LastError == "" && !st.Listening {
			st.LastError = m.lastErr()
		}
		m.setStatus(st)
	default:
		m.stopACME()
		m.stopHTTPS()
		st.LastError = "未知 tls_mode: " + cfg.Mode
		m.setStatus(st)
	}
}

func sameConfig(a, b Config) bool {
	return a.Mode == b.Mode && a.Domain == b.Domain && a.IPSubject == b.IPSubject &&
		a.HTTPAddr == b.HTTPAddr && a.HTTPSAddr == b.HTTPSAddr && a.ACMEDir == b.ACMEDir &&
		a.ACMEEmail == b.ACMEEmail && a.ACMEProfile == b.ACMEProfile &&
		a.DNSProviderKey == b.DNSProviderKey
}

func acmeSubject(cfg Config) string {
	if domain := strings.TrimSpace(cfg.Domain); domain != "" {
		return domain
	}
	if ip := net.ParseIP(strings.TrimSpace(cfg.IPSubject)); ip != nil {
		return ip.String()
	}
	return ""
}

func acmeProfile(cfg Config, subject string) string {
	if net.ParseIP(subject) != nil {
		return "shortlived"
	}
	return strings.TrimSpace(cfg.ACMEProfile)
}

// Status 当前状态快照。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status
	st.SANs = slices.Clone(st.SANs)
	return st
}

// setStatus 以本次 Sync 的结果为准整体替换（LastError 只反映本次动作）；
// CAAvailable 统一在这里按文件现状算——CA 可能正是这一轮生成的。
func (m *Manager) setStatus(st Status) {
	st.CAAvailable = m.hasCAFile()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = st
}

func (m *Manager) lastErr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status.LastError
}

// ---- CertMagic ----

func (m *Manager) acmeConfig() *certmagic.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.magic
}

func (m *Manager) ensureCache() *certmagic.Cache {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache == nil {
		m.cache = certmagic.NewCache(certmagic.CacheOptions{
			GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
				m.mu.Lock()
				defer m.mu.Unlock()
				if m.magic == nil {
					return nil, errors.New("ACME 配置已停用")
				}
				return m.magic, nil
			},
			Logger: zap.NewNop(),
		})
	}
	return m.cache
}

// configureACME 更换 CertMagic 配置并启动可取消的签发循环。旧标识从内存缓存
// 卸载，避免 IP 变动后继续续签旧证书；磁盘材料保留，便于切回时复用。
func (m *Manager) configureACME(cfg Config, subject string) {
	cache := m.ensureCache()
	storage := &certmagic.FileStorage{Path: m.dir}
	magic := certmagic.New(cache, certmagic.Config{
		Storage:           storage,
		DefaultServerName: subject,
		Logger:            zap.NewNop(),
	})
	if net.ParseIP(subject) != nil {
		// shortlived 证书有效期很短，提前一半生命周期进入续签窗口。
		magic.RenewalWindowRatio = 0.5
	}
	issuerTemplate := certmagic.ACMEIssuer{
		CA:             cfg.ACMEDir,
		Email:          cfg.ACMEEmail,
		Agreed:         true,
		Profile:        acmeProfile(cfg, subject),
		AltHTTPPort:    addrPort(cfg.HTTPAddr),
		AltTLSALPNPort: addrPort(cfg.HTTPSAddr),
		Logger:         magic.Logger,
	}
	if cfg.Domain != "" && cfg.DNSProvider != nil {
		issuerTemplate.DNS01Solver = &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider: cfg.DNSProvider,
			Logger:      magic.Logger,
		}}
	}
	issuer := certmagic.NewACMEIssuer(magic, issuerTemplate)
	magic.Issuers = []certmagic.Issuer{issuer}

	m.mu.Lock()
	oldSubject := ""
	if m.magic != nil {
		oldSubject = m.magic.DefaultServerName
	}
	if m.acmeCancel != nil {
		m.acmeCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.acmeCancel = cancel
	m.acmeGeneration++
	generation := m.acmeGeneration
	m.magic = magic
	m.httpChallenge = issuer.HandleHTTPChallenge
	m.mu.Unlock()

	if oldSubject != "" && oldSubject != subject {
		cache.RemoveManaged([]certmagic.SubjectIssuer{{Subject: oldSubject}})
	}
	m.acmeWG.Add(1)
	go func() {
		defer m.acmeWG.Done()
		m.obtainLoop(ctx, generation, magic, subject, issuerTemplate.Profile)
	}()
}

func (m *Manager) stopACME() {
	m.mu.Lock()
	if m.acmeCancel != nil {
		m.acmeCancel()
		m.acmeCancel = nil
	}
	m.acmeGeneration++
	subject, cache := m.status.Subject, m.cache
	m.magic, m.httpChallenge = nil, nil
	m.mu.Unlock()
	if cache != nil && subject != "" {
		cache.RemoveManaged([]certmagic.SubjectIssuer{{Subject: subject}})
	}
}

func (m *Manager) obtainLoop(ctx context.Context, generation uint64, magic *certmagic.Config, subject, profile string) {
	if net.ParseIP(subject) != nil && !waitContext(ctx, acmeIPDebounce) {
		return
	}
	backoff := acmeRetryMin
	for {
		manage := m.manageSync
		var err error
		if manage != nil {
			err = manage(ctx, magic, []string{subject})
		} else {
			err = magic.ManageSync(ctx, []string{subject})
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			cert, getErr := magic.GetCertificate(&tls.ClientHelloInfo{ServerName: subject})
			if getErr == nil {
				m.recordCertificate(generation, cert, subject, profile)
				log.Printf("ACME 证书已就绪（标识: %s）", subject)
				return
			}
			err = getErr
		}
		next := time.Now().Add(backoff)
		m.recordACMEError(generation, subject, err, next)
		log.Printf("ACME 证书签发失败（标识: %s，%s 后重试）: %v", subject, backoff, err)
		if !waitContext(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, acmeRetryMax)
	}
}

func (m *Manager) recordACMEError(generation uint64, subject string, err error, next time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.acmeGeneration || m.cfg.Mode != "acme" || m.status.Subject != subject {
		return
	}
	m.status.LastError = "ACME 签发失败: " + err.Error()
	m.status.NextRetry = next
}

func waitContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func addrPort(addr string) int {
	_, raw, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(raw)
	return port
}

// ---- HTTPS listener ----

// ensureHTTPS 按 cfg.HTTPSAddr 起 HTTPS listener；地址不变且在跑则不动。
// 起失败落 LastError 并返回 false。
func (m *Manager) ensureHTTPS() bool {
	m.mu.Lock()
	cfg := m.cfg
	running := m.srv != nil
	curAddr := ""
	if running {
		curAddr = m.srv.Addr
	}
	m.mu.Unlock()

	if running && curAddr == cfg.HTTPSAddr {
		return true
	}
	m.stopHTTPS()

	ln, err := net.Listen("tcp", cfg.HTTPSAddr)
	if err != nil {
		log.Printf("HTTPS 监听 %s 失败: %v", cfg.HTTPSAddr, err)
		m.mu.Lock()
		m.status.LastError = "HTTPS 监听失败: " + err.Error()
		m.mu.Unlock()
		return false
	}
	srv := &http.Server{
		Addr:    cfg.HTTPSAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.handler().ServeHTTP(w, r) }),
		// acme-tls/1 常驻：ServeTLS 在建 listener 时固化 NextProtos，而模式热切换不重起
		// listener，带上它 TLS-ALPN-01 挑战在切到 acme 后立即可用。
		TLSConfig: &tls.Config{GetCertificate: m.getCertificate, MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}},
	}
	m.mu.Lock()
	m.srv = srv
	m.mu.Unlock()
	log.Printf("HTTPS 监听于 %s（%s 模式）", cfg.HTTPSAddr, cfg.Mode)
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTPS 服务异常退出: %v", err)
			m.mu.Lock()
			m.status.LastError = "HTTPS 服务退出: " + err.Error()
			m.srv = nil
			m.mu.Unlock()
		}
	}()
	return true
}

func (m *Manager) stopHTTPS() {
	m.mu.Lock()
	srv := m.srv
	m.srv = nil
	m.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		log.Printf("HTTPS 监听已停止")
	}
}

// Close 进程退出时停掉 HTTPS listener 与 CertMagic 续签协程。
func (m *Manager) Close() {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.stopACME()
	m.stopHTTPS()
	m.acmeWG.Wait()
	m.mu.Lock()
	cache := m.cache
	m.cache = nil
	m.mu.Unlock()
	if cache != nil {
		cache.Stop()
	}
}

// getCertificate 每次握手先取 CertMagic 证书；尚未签发或续签出错时立即回落
// 本地自签名叶子证书，不让 HTTPS 握手失败。
func (m *Manager) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	cfg, magic, leaf := m.cfg, m.magic, m.leaf
	generation := m.acmeGeneration
	m.mu.Unlock()
	if cfg.Mode == "acme" && magic != nil {
		cert, err := magic.GetCertificate(hello)
		if err == nil {
			if !slices.Contains(hello.SupportedProtos, "acme-tls/1") {
				subject := acmeSubject(cfg)
				m.recordCertificate(generation, cert, subject, acmeProfile(cfg, subject))
			}
			return cert, nil
		}
		subject := acmeSubject(cfg)
		if hello == nil || hello.ServerName == "" || strings.EqualFold(hello.ServerName, subject) {
			m.recordFallback(generation, subject, leaf, err)
		}
	}
	if leaf != nil {
		return leaf, nil
	}
	return nil, errors.New("证书尚未就绪")
}

func (m *Manager) recordFallback(generation uint64, subject string, leaf *tls.Certificate, cause error) {
	if leaf == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.acmeGeneration || m.cfg.Mode != "acme" || m.status.Subject != subject {
		return
	}
	m.status.Active = "selfsigned"
	m.status.SANs = leafSANs(leaf)
	m.status.NotAfter = leaf.Leaf.NotAfter
	if cause != nil && m.status.LastError == "" {
		m.status.LastError = "ACME 证书尚不可用: " + cause.Error()
	}
}

func (m *Manager) recordCertificate(generation uint64, cert *tls.Certificate, subject, profile string) {
	if cert == nil {
		return
	}
	leaf := cert.Leaf
	if leaf == nil && len(cert.Certificate) > 0 {
		leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	if leaf == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.acmeGeneration || m.cfg.Mode != "acme" || m.status.Subject != subject {
		return
	}
	m.status.Active = "acme"
	m.status.Profile = profile
	m.status.SANs = certificateSANs(leaf)
	m.status.NotAfter = leaf.NotAfter
	m.status.LastError = ""
	m.status.NextRetry = time.Time{}
}

// ---- HTTP 分流 ----

// HTTPHandler tls_mode != off 时 HTTP 端口的 handler：健康检查与回环来源的管理 API
// 保持直通，ACME HTTP-01 挑战优先，其余重定向到 HTTPS（外部 443，不带端口）。
func (m *Manager) HTTPHandler(fallback http.Handler) http.Handler {
	if fallback == nil {
		fallback = http.NotFoundHandler()
	}
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || (strings.HasPrefix(r.URL.Path, "/api/") && loopbackRequest(r)) {
			fallback.ServeHTTP(w, r)
			return
		}
		m.mu.Lock()
		cfg := m.cfg
		m.mu.Unlock()
		host := acmeSubject(cfg)
		if host == "" {
			// 映射对外固定为 443，不能把进程内 HTTPS 监听端口泄漏进公开链接。
			h, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				h = r.Host
			}
			host = h
		}
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		handleChallenge := m.httpChallenge
		m.mu.Unlock()
		if handleChallenge != nil && handleChallenge(w, r) {
			return
		}
		redirect.ServeHTTP(w, r)
	})
}

func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// TLSOn 当前配置是否开了 TLS（HTTP 分流用）。
func (m *Manager) TLSOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Mode == "acme" || m.cfg.Mode == "selfsigned"
}

// ---- selfsigned：本地 CA + 叶子证书 ----

// CACertPEM 本地 CA 的 PEM（管理后台「下载 CA 证书」用）；未生成时报错。
func (m *Manager) CACertPEM() ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(m.dir, "ca.crt"))
	if err != nil {
		return nil, errors.New("本地 CA 尚未生成（先开启自签名证书模式）")
	}
	return b, nil
}

// hasCAFile 本地 CA 文件是否已存在（不限当前模式，曾经开过自签名就还在）。
func (m *Manager) hasCAFile() bool {
	_, err := os.Stat(filepath.Join(m.dir, "ca.crt"))
	return err == nil
}

// ensureLeaf 保证有一张覆盖当前地址集合且在续签窗口之外的叶子证书。
// SAN 集合 = localhost / 回环 / 本机全部 LAN 地址 / 探测到的公网地址 / site_domain；
// 集合变化或临近到期即重签。已建立的 TLS 连接不受影响（GetCertificate 只影响新握手）。
func (m *Manager) ensureLeaf() error {
	m.mu.Lock()
	cfg := m.cfg
	cur, curSANs := m.leaf, m.sans
	m.mu.Unlock()

	dnsNames, ips := m.sanSet(cfg.Domain)
	want := append(slices.Clone(dnsNames), addrsStrings(ips)...)
	slices.Sort(want)

	if cur != nil && time.Until(cur.Leaf.NotAfter) > leafRenewBefore && slices.Equal(want, curSANs) {
		return nil
	}

	caCert, caKey, err := m.ensureCA()
	if err != nil {
		return err
	}
	leaf, err := signLeaf(caCert, caKey, dnsNames, ips)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.leaf, m.sans = leaf, want
	m.mu.Unlock()
	log.Printf("自签名证书已签发（SAN: %s）", strings.Join(want, ", "))
	return nil
}

// sanSet 计算叶子证书的 SAN：域名进 DNSNames，地址进 IPAddresses。
func (m *Manager) sanSet(domain string) (dnsNames []string, ips []net.IP) {
	dnsNames = []string{"localhost"}
	ips = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if domain != "" {
		dnsNames = append(dnsNames, domain)
	}
	if ifs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	if m.externals != nil {
		for _, e := range m.externals() {
			s := e
			if ap, err := netip.ParseAddrPort(e); err == nil { // 映射结果带端口
				s = ap.Addr().String()
			}
			if ip := net.ParseIP(s); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	return dnsNames, dedupIPs(ips)
}

func dedupIPs(ips []net.IP) []net.IP {
	seen := map[string]bool{}
	out := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		s := ip.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, ip)
		}
	}
	return out
}

func addrsStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

// ensureCA 读或生成本地 CA（<dir>/ca.{crt,key}）。CA 私钥仅留在数据目录，
// 只用于签本机自己的叶子证书。
func (m *Manager) ensureCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	crtPath := filepath.Join(m.dir, "ca.crt")
	keyPath := filepath.Join(m.dir, "ca.key")
	if crtPEM, err1 := os.ReadFile(crtPath); err1 == nil {
		if keyPEM, err2 := os.ReadFile(keyPath); err2 == nil {
			cert, key, err := parseCA(crtPEM, keyPEM)
			if err == nil {
				return cert, key, nil
			}
			// 损坏则重签：CA 只是本机信任锚，重签的代价是用户重新装一次 CA
			log.Printf("本地 CA 读取失败，重新生成: %v", err)
		}
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Hearth Local CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, nil, err
	}
	log.Printf("本地 CA 已生成: %s（有效期 10 年，装进设备即不再提示证书警告）", crtPath)
	return cert, key, nil
}

func parseCA(crtPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(crtPEM)
	if blk == nil {
		return nil, nil, errors.New("ca.crt 不是 PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	kblk, _ := pem.Decode(keyPEM)
	if kblk == nil {
		return nil, nil, errors.New("ca.key 不是 PEM")
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// signLeaf 用本地 CA 签一年期叶子证书。
func signLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, dnsNames []string, ips []net.IP) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "hearth"},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

func leafSANs(c *tls.Certificate) []string {
	return certificateSANs(c.Leaf)
}

func certificateSANs(leaf *x509.Certificate) []string {
	if leaf == nil {
		return nil
	}
	var out []string
	out = append(out, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
