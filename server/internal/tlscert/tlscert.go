// Package tlscert 管理 HTTPS 用的服务端证书：关闭、自签（自建根 CA）、外部文件、后台上传
// 四种来源，由动态配置切换，60 秒一轮检查热换。
//
// 两条硬约束：
//   - 坏证书永不把 HTTPS 拉死——解析失败保留上一张可用证书，只打日志。
//   - 根 CA 私钥不经任何接口出去（状态只给指纹），且根证书带名称约束，即使泄漏也签不出
//     任何域名证书（见 selfca.go）。
package tlscert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// 证书来源，取值与动态配置键 tls_cert_source 一致。
const (
	SourceOff    = "off"
	SourceSelf   = "self"
	SourceFile   = "file"
	SourceUpload = "upload"
)

// CheckInterval 后台检查周期：file/upload 看文件是否变了，self 看是否该重签。
const CheckInterval = 60 * time.Second

// Settings 一轮检查所依赖的配置快照。tlscert 不认识 dyncfg，由宿主每轮传进来。
type Settings struct {
	Source    string
	CertFile  string   // source=file 时的证书路径
	KeyFile   string   // source=file 时的私钥路径
	SelfHosts []string // 额外主机名/IP（tls_self_hosts）
	External  []string // 宣告探测到的外部地址（可能带端口），进自签叶证书的 SAN
}

// CertInfo 当前生效证书的回显信息。
type CertInfo struct {
	Subject           string
	SANs              []string
	NotAfter          time.Time
	FingerprintSHA256 string
}

// CAInfo 自签根证书的回显信息；私钥相关的一切都不在其中。
type CAInfo struct {
	FingerprintSHA256 string
	NotAfter          time.Time
	// ConstraintStale 配置里新增的 DNS 名不在根证书的名称约束里：不悄悄放宽，
	// 由管理员决定是否重新生成根证书（装过根的设备要重装）。
	ConstraintStale bool
}

// Status 状态快照。
type Status struct {
	Source string
	Cert   *CertInfo
	CA     *CAInfo
}

// Store 持有当前证书，对外只暴露 GetCertificate。
type Store struct {
	dir  string // <data>/tls
	load func(context.Context) Settings

	cur atomic.Pointer[tls.Certificate]

	mu      sync.Mutex
	source  string
	cert    *CertInfo
	ca      *CAInfo
	stamp   string // file/upload：上轮见到的文件指纹（路径+mtime+大小）
	lastLog string // 上一次打过的错误，相同不重复刷屏
}

// New 建 Store。load 每轮调用一次，取当前动态配置。
func New(dir string, load func(context.Context) Settings) *Store {
	return &Store{dir: dir, load: load}
}

// GetCertificate 交给 tls.Config；没有可用证书时返回错误（握手失败，明文侧不受影响）。
func (s *Store) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := s.cur.Load(); c != nil {
		return c, nil
	}
	s.mu.Lock()
	src := s.source
	s.mu.Unlock()
	if src == SourceOff {
		return nil, errors.New("TLS 已关闭")
	}
	return nil, errors.New("尚无可用证书")
}

// TLSConfig 服务端 TLS 配置：证书按需从 Store 取（热换对已建连无影响），
// NextProtos 让 HTTP/2 协商得出来。
func (s *Store) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: s.GetCertificate,
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// Status 当前状态快照。
func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Source: s.source}
	if s.cert != nil {
		c := *s.cert
		c.SANs = append([]string(nil), s.cert.SANs...)
		st.Cert = &c
	}
	if s.ca != nil {
		ca := *s.ca
		st.CA = &ca
	}
	return st
}

// Run 后台检查循环：先立刻跑一轮，之后每 CheckInterval 一轮。
func (s *Store) Run(ctx context.Context) {
	s.Check(ctx)
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Check(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Check 跑一轮当前来源的检查。任何失败都不动已生效的证书。
func (s *Store) Check(ctx context.Context) {
	set := s.load(ctx)
	if set.Source == "" {
		set.Source = SourceSelf
	}
	s.mu.Lock()
	switched := s.source != set.Source
	s.source = set.Source
	if switched {
		s.stamp = ""
		s.lastLog = ""
		s.cert = nil
		s.ca = nil
	}
	s.mu.Unlock()
	if switched {
		s.cur.Store(nil)
	}
	switch set.Source {
	case SourceOff:
		// 不生成任何文件、不持证书：与没有 TLS 的部署等价。
	case SourceFile:
		s.checkFile(set.CertFile, set.KeyFile)
	case SourceUpload:
		s.checkFile(s.Path("upload.crt"), s.Path("upload.key"))
	default:
		s.checkSelf(set)
	}
}

// Path 数据目录下的证书文件路径。
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// SaveUpload 校验并落盘上传的证书对；校验不过什么都不写。
func (s *Store) SaveUpload(certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.Path("upload.crt"), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(s.Path("upload.key"), keyPEM, 0o600)
}

// CACertPEM 根证书的 PEM（公开可下载）；不是自签来源或还没生成时返回错误。
func (s *Store) CACertPEM() ([]byte, error) {
	s.mu.Lock()
	src := s.source
	s.mu.Unlock()
	if src != SourceSelf {
		return nil, errors.New("当前证书来源不是自签")
	}
	return os.ReadFile(s.Path("ca.crt"))
}

// RotateCA 重新生成根 CA 与叶证书：根私钥可能泄漏、或名称约束需要放宽时用。
// 装过旧根的设备必须重装新根。
func (s *Store) RotateCA(ctx context.Context) error {
	set := s.load(ctx)
	if set.Source != SourceSelf {
		return errors.New("当前证书来源不是自签")
	}
	for _, n := range []string{"ca.crt", "ca.key", "self.crt", "self.key"} {
		if err := os.Remove(s.Path(n)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	s.mu.Lock()
	s.cert, s.ca = nil, nil
	s.mu.Unlock()
	s.cur.Store(nil)
	s.checkSelf(set)
	if s.cur.Load() == nil {
		return errors.New("重新生成根证书失败，详见服务端日志")
	}
	return nil
}

// ---- file / upload ----

func (s *Store) checkFile(certPath, keyPath string) {
	if certPath == "" || keyPath == "" {
		s.logOnce("tls: 证书来源为文件但未填写证书/私钥路径")
		return
	}
	stamp := certPath + "|" + keyPath + "|" + fileStamp(certPath) + "|" + fileStamp(keyPath)
	s.mu.Lock()
	same := stamp == s.stamp
	s.stamp = stamp
	s.mu.Unlock()
	if same && s.cur.Load() != nil {
		return
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		// 保留旧证书：外部工具续期时文件可能有一瞬间不成对。
		s.logOnce("tls: 读取证书失败（保留上一张证书）: " + err.Error())
		return
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		s.logOnce("tls: 解析证书失败（保留上一张证书）: " + err.Error())
		return
	}
	pair.Leaf = leaf
	s.apply(&pair, leaf, nil)
	log.Printf("tls: 已加载证书 %s（到期 %s）", certPath, leaf.NotAfter.Format(time.RFC3339))
}

// fileStamp 文件变化指纹：mtime + 大小。取不到（不存在）返回空串，下一轮照样重试。
func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fi.ModTime().UTC().Format(time.RFC3339Nano) + ":" + strconv.FormatInt(fi.Size(), 10)
}

// ---- 公共 ----

func (s *Store) apply(pair *tls.Certificate, leaf *x509.Certificate, ca *CAInfo) {
	info := &CertInfo{
		Subject:           leaf.Subject.String(),
		SANs:              sansOf(leaf),
		NotAfter:          leaf.NotAfter,
		FingerprintSHA256: fingerprint(leaf.Raw),
	}
	s.cur.Store(pair)
	s.mu.Lock()
	s.cert = info
	s.ca = ca
	s.lastLog = ""
	s.mu.Unlock()
}

func (s *Store) logOnce(msg string) {
	s.mu.Lock()
	dup := s.lastLog == msg
	s.lastLog = msg
	s.mu.Unlock()
	if !dup {
		log.Print(msg)
	}
}
