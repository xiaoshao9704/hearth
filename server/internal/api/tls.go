// 进程内 TLS、/api/site 与网络自检（/api/admin/netcheck）的接入层接线。
package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"hearth/server/internal/ddns"
	"hearth/server/internal/portmap"
	"hearth/server/internal/tlsx"
)

// SetTLS 注入 TLS 管理器（main 在路由建好后设置；测试中可为 nil，netcheck/概览按 off 回报）。
func (a *API) SetTLS(m *tlsx.Manager) { a.tls = m }

// SetPortMapper 注入端口映射器（自检回显用；nil 时按未启用回报）。
func (a *API) SetPortMapper(m *portmap.Mapper) { a.mapper = m }

// tlsConfig 当前 TLS 相关动态配置快照。
func (a *API) tlsConfig(ctx context.Context) tlsx.Config {
	return tlsx.Config{
		Mode:      a.dynVal(ctx, "tls_mode"),
		Domain:    a.dynVal(ctx, "site_domain"),
		HTTPSAddr: a.dynVal(ctx, "https_addr"),
		ACMEDir:   a.dynVal(ctx, "acme_directory"),
		ACMEEmail: a.dynVal(ctx, "acme_email"),
	}
}

// SyncTLS 按当前动态配置 reconcile TLS（模式/地址/证书集合）。调用点：配置保存后、
// 宣告探测刷新后（公网地址变化要重签自签名叶子证书）、进程启动时。
func (a *API) SyncTLS() {
	if a.tls == nil {
		return
	}
	a.tls.Sync(a.tlsConfig(context.Background()))
}

// AnnounceExternals 宣告探测的公网地址快照：整台机器只有一份探测状态（ember 的
// Announcer，lkembed 的地址改写也读它），可能带端口（映射结果）。自检回显与
// tlsx 自签名证书的 SAN 来源都走这里。
func (a *API) AnnounceExternals() ([]string, time.Time) {
	if a.ember == nil {
		return nil, time.Time{}
	}
	return a.ember.AnnounceSnapshot()
}

// tlsStatus 证书状态回显；未接 TLS 管理器时按 off 报。
func (a *API) tlsStatus() tlsx.Status {
	if a.tls == nil {
		return tlsx.Status{Mode: "off"}
	}
	return a.tls.Status()
}

// ---- /api/site（公开）----

// site 站点公开信息：登录页与首启向导用。needs_setup = users 表为空（首启向导可用，
// 此时注册接口放行首个账号并自动成为管理员）。
func (a *API) site(w http.ResponseWriter, r *http.Request) {
	users, _, err := a.st.Counts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        a.cfg.SiteName,
		"policy":      a.regPolicy(r),
		"needs_setup": users == 0,
	})
}

// ---- CA 下载 ----

// adminTLSCA 下载自签名模式的本地 CA 证书（装进设备即不再提示证书警告）。
func (a *API) adminTLSCA(w http.ResponseWriter, r *http.Request) {
	if a.tls == nil {
		writeErr(w, http.StatusNotFound, "TLS 未启用")
		return
	}
	pem, err := a.tls.CACertPEM()
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="hearth-ca.crt"`)
	w.Write(pem)
}

// ---- 网络自检 ----

// netcheckResult GET /api/admin/netcheck 的返回。自检只走管理接口：
// /healthz 语义不变（只报活、恒 200、无副作用），诊断与回显全在这里。
type netcheckResult struct {
	Portmap   portmap.Status `json:"portmap"`   // 映射快照（方法/外部地址/级联跳数/诊断，v6 放行也在其中）
	Externals []string       `json:"externals"` // 宣告探测快照（STUN/映射/显式配置）
	ProbedAt  time.Time      `json:"probed_at"`
	Domain    domainCheck    `json:"domain"`
	DDNS      ddns.Status    `json:"ddns"` // DDNS 推送状态（off = 未启用）
	TLS       tlsx.Status    `json:"tls"`
	External  externalCheck  `json:"external"` // 从本机向公开地址回探 /healthz 的结论
}

type domainCheck struct {
	Configured string   `json:"configured"` // site_domain（空 = 未配置）
	Resolved   []string `json:"resolved"`   // 系统 resolver 查到的 A/AAAA
	Match      string   `json:"match"`      // ok / mismatch / unconfigured / error
	Detail     string   `json:"detail,omitempty"`
}

type externalCheck struct {
	URL     string `json:"url,omitempty"`
	Verdict string `json:"verdict"` // reachable / unknown / failed
	Detail  string `json:"detail"`
}

func (a *API) adminNetcheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	res := netcheckResult{TLS: a.tlsStatus(), DDNS: a.ddnsStatus()}
	if a.mapper != nil {
		res.Portmap = a.mapper.Snapshot()
	} else {
		res.Portmap = portmap.Status{Diagnosis: portmap.DiagOff, Detail: "端口映射未接入"}
	}
	res.Externals, res.ProbedAt = a.AnnounceExternals()

	cfg := a.tlsConfig(ctx)
	res.Domain = checkDomain(ctx, cfg.Domain, res.Externals)
	res.External = a.probeExternal(ctx, cfg, res, a.publicProbeBase(ctx))
	writeJSON(w, http.StatusOK, res)
}

// checkDomain 比对 site_domain 的解析结果与探测到的公网地址。
func checkDomain(ctx context.Context, domain string, externals []string) domainCheck {
	dc := domainCheck{Configured: domain, Match: "unconfigured"}
	if domain == "" {
		return dc
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", domain)
	if err != nil {
		dc.Match = "error"
		dc.Detail = "域名解析失败: " + err.Error()
		return dc
	}
	for _, ip := range addrs {
		dc.Resolved = append(dc.Resolved, ip.String())
	}
	slices.Sort(dc.Resolved)
	pub := externalIPs(externals)
	if len(pub) == 0 {
		dc.Match = "error"
		dc.Detail = "还没有探测到本机公网地址，无法比对（STUN 不可达且未做端口映射）"
		return dc
	}
	for _, r := range dc.Resolved {
		if slices.Contains(pub, r) {
			dc.Match = "ok"
			return dc
		}
	}
	dc.Match = "mismatch"
	dc.Detail = "域名解析到的地址与探测到的公网地址不一致（DDNS 未更新或解析指向了别的机器）"
	return dc
}

// externalIPs 宣告快照（可能带端口）压成去重地址列表。
func externalIPs(externals []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range externals {
		s := e
		if ap, err := netip.ParseAddrPort(e); err == nil {
			s = ap.Addr().String()
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// probeExternal 从本机向公开地址回探 /healthz。
// 回环探测打不通不等于失败——NAT hairpin 不支持的网关很常见，所以结论分三档：
// reachable（真的通了）/ unknown（本机打不到但映射/探测看起来正常，提示用手机流量验证）/
// failed（映射没建立，或按域名探时证书与域名不匹配）。
func (a *API) probeExternal(ctx context.Context, cfg tlsx.Config, res netcheckResult, publicBase string) externalCheck {
	if cfg.Mode == "off" {
		if publicBase == "" {
			return externalCheck{Verdict: "unknown", Detail: "未配置 PUBLIC_URL 或 site_domain，无法安全确定公开回探地址"}
		}
		base, err := url.Parse(publicBase)
		if err != nil || base.Scheme == "" || base.Host == "" {
			return externalCheck{Verdict: "failed", Detail: "公开地址配置无效，无法回探"}
		}
		probeURL := strings.TrimSuffix(base.String(), "/") + "/healthz"
		reachable, _ := tryHealthz(ctx, probeURL, base.Scheme == "https")
		if reachable {
			return externalCheck{URL: probeURL, Verdict: "reachable", Detail: "外部地址回探通过"}
		}
		return externalCheck{URL: probeURL, Verdict: "unknown",
			Detail: "从本机回探不通；反向代理或网关可能不支持回环，请从实际外部网络验证"}
	}
	host := cfg.Domain
	if host == "" {
		pub := externalIPs(res.Externals)
		if len(pub) == 0 {
			return externalCheck{Verdict: "failed",
				Detail: "没有可回探的公开地址：site_domain 未配置且未探测到公网地址（端口映射未建立）"}
		}
		host = pub[0]
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		host = "[" + host + "]"
	}

	// 进程内 TLS 映射把外部 443 指到 https_addr。
	scheme, port := "https", "443"
	verify := cfg.Domain != "" && cfg.Mode == "acme" // 只有 acme 的证书链能过公开验证
	url := scheme + "://" + host + ":" + port + "/healthz"

	reachable, certErr := tryHealthz(ctx, url, verify)
	if !reachable && verify && certErr != nil {
		// 按域名探时证书不匹配/不受信是确定性的配置问题，判 failed；
		// 自签名模式不受信是预期（用户没装 CA），跳过一次不验证的再试
		if cfg.Mode == "acme" {
			return externalCheck{URL: url, Verdict: "failed",
				Detail: "443 可达但证书校验失败（域名与证书不匹配或尚未签发完成）: " + certErr.Error()}
		}
		reachable, _ = tryHealthz(ctx, url, false)
		if reachable {
			return externalCheck{URL: url, Verdict: "reachable",
				Detail: "回探通过（自签名证书按预期跳过公开校验；设备装上 CA 证书后浏览器不再告警）"}
		}
	}
	if reachable {
		return externalCheck{URL: url, Verdict: "reachable", Detail: "外部地址回探通过"}
	}
	// 打不通：映射/探测有结果时大概率是网关不支持 hairpin 回环，不能判死
	if res.Portmap.Diagnosis == portmap.DiagOK || res.Portmap.Diagnosis == portmap.DiagUpstreamNAT || len(res.Externals) > 0 {
		return externalCheck{URL: url, Verdict: "unknown",
			Detail: "从本机回探不通——多数家用网关不支持 hairpin 回环，本机打不到自己的外部地址不等于外网不通；请用手机流量打开站点地址验证"}
	}
	return externalCheck{URL: url, Verdict: "failed",
		Detail: "回探不通且端口映射未建立（外部 80/443 没有指向本机）"}
}

// tryHealthz 单次回探，3 秒超时。verify=false 跳过证书公开校验（自签名模式预期如此）。
func tryHealthz(ctx context.Context, url string, verify bool) (ok bool, certErr error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: !verify}} //nolint:gosec // 自检回探有意跳过
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, nil
	}
	resp, err := c.Do(req)
	if err != nil {
		var hostErr x509.HostnameError
		var authErr x509.UnknownAuthorityError
		var certInvalid x509.CertificateInvalidError
		if errors.As(err, &hostErr) || errors.As(err, &authErr) || errors.As(err, &certInvalid) {
			return false, err
		}
		return false, nil
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}
