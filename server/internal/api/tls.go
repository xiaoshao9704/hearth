// TLS 证书的动态配置、管理接口与公开入口（根证书下载与安装说明页）。
// 证书本身的生成/加载/热换在 internal/tlscert，这里只做接线与回显。
package api

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc"
	"hearth/server/internal/tlscert"
)

// tlsKeys 证书来源与路径。来源默认 self：没有域名的部署起进程即有 https，
// 装一次根证书就能用麦克风/投屏/通行密钥这些要安全上下文的能力。
var tlsKeys = []rtc.ConfigKey{
	{Name: "tls_cert_source", Env: "TLS_CERT_SOURCE", Group: "network", Default: "self",
		Options: []string{"off", "self", "file", "upload"},
		Label:   "TLS 证书来源",
		Hint: "off = 不提供 https（放在反代后面时用）；self = 本机自签，需在各设备安装根证书（安装说明在 /ca）；" +
			"file = 用外部工具签发的 PEM，续期后文件一变自动热换；upload = 后台上传证书与私钥。" +
			"改动保存即生效，不必重启"},
	{Name: "tls_cert_file", Env: "TLS_CERT_FILE", Group: "network",
		Label: "证书文件路径",
		Hint:  "来源为 file 时的证书 PEM 绝对路径（含中间证书时按「叶证书在前」拼接）"},
	{Name: "tls_key_file", Env: "TLS_KEY_FILE", Group: "network",
		Label: "私钥文件路径",
		Hint:  "来源为 file 时的私钥 PEM 绝对路径，进程需有读权限"},
	{Name: "tls_self_hosts", Group: "network",
		Label: "自签证书额外主机名",
		Hint: "逗号分隔的主机名或 IP，追加进自签证书的 SAN（本机地址与探测到的公网地址已自动带上）。" +
			"新增主机名需要重新生成根证书，装过根证书的设备要重装——有域名的话改用 file/upload 更省事"},
}

// tlsSettings 交给 tlscert 的配置快照：动态配置 + 当前宣告到的外部地址。
func (a *API) tlsSettings(ctx context.Context) tlscert.Settings {
	ext, _ := a.announcer.Snapshot()
	return tlscert.Settings{
		Source:    a.dynVal(ctx, "tls_cert_source"),
		CertFile:  strings.TrimSpace(a.dynVal(ctx, "tls_cert_file")),
		KeyFile:   strings.TrimSpace(a.dynVal(ctx, "tls_key_file")),
		SelfHosts: splitComma(a.dynVal(ctx, "tls_self_hosts")),
		External:  ext,
	}
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TLSServerConfig 给监听侧的 TLS 配置（证书按需取，热换不影响在途连接）。
func (a *API) TLSServerConfig() *tls.Config { return a.tlsStore.TLSConfig() }

// CheckTLS 立即跑一轮证书检查（启动时先跑一次，让首个握手就有证书）。
func (a *API) CheckTLS(ctx context.Context) { a.tlsStore.Check(ctx) }

// RunTLSCheck 证书检查循环（60 秒一轮：文件变了热换、自签该重签就重签）。
func (a *API) RunTLSCheck(ctx context.Context) { a.tlsStore.Run(ctx) }

// SetPortmapStatus 接入端口映射快照，供 TLS 状态接口回显对外可达性诊断。
func (a *API) SetPortmapStatus(f func() portmap.Status) { a.portmapStatus = f }

// ---- 管理接口 ----

func (a *API) adminTLS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.tlsStatus(r.Context()))
}

// adminTLSUpload 上传证书与私钥：先校验成对，通过才落盘并把来源切成 upload；
// 失败 400 且什么都不改。
func (a *API) adminTLSUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	certPEM, ok := formFile(w, r, "cert")
	if !ok {
		return
	}
	keyPEM, ok := formFile(w, r, "key")
	if !ok {
		return
	}
	if err := a.tlsStore.SaveUpload(certPEM, keyPEM); err != nil {
		writeErr(w, http.StatusBadRequest, "证书与私钥校验失败: "+err.Error())
		return
	}
	if err := a.st.SetSetting(r.Context(), "cfg_tls_cert_source", tlscert.SourceUpload); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.tlsStore.Check(r.Context())
	writeJSON(w, http.StatusOK, a.tlsStatus(r.Context()))
}

// adminTLSRotateCA 重新生成根证书：私钥可能泄漏、或名称约束需要放宽时用，
// 装过旧根的设备必须重装。
func (a *API) adminTLSRotateCA(w http.ResponseWriter, r *http.Request) {
	if err := a.tlsStore.RotateCA(r.Context()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.tlsStatus(r.Context()))
}

func formFile(w http.ResponseWriter, r *http.Request, field string) ([]byte, bool) {
	f, _, err := r.FormFile(field)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少文件字段: "+field)
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取上传文件失败")
		return nil, false
	}
	return b, true
}

// tlsStatus 状态回显（前后端契约，见 docs/plan-tls.md）。
func (a *API) tlsStatus(ctx context.Context) map[string]any {
	st := a.tlsStore.Status()
	out := map[string]any{
		"source":     st.Source,
		"mode":       a.tlsMode(),
		"http_addr":  a.cfg.Addr,
		"https_addr": a.cfg.HTTPSAddr,
		"cert":       nil,
		"ca":         nil,
		"cert_file":  strings.TrimSpace(a.dynVal(ctx, "tls_cert_file")),
		"key_file":   strings.TrimSpace(a.dynVal(ctx, "tls_key_file")),
	}
	if !a.cfg.TLSSplit() {
		out["https_addr"] = "" // 合并模式下 https 就在 http 那个地址上
	}
	if c := st.Cert; c != nil {
		out["cert"] = map[string]any{
			"subject":            c.Subject,
			"sans":               c.SANs,
			"not_after":          rfc3339(c.NotAfter),
			"fingerprint_sha256": c.FingerprintSHA256,
		}
	}
	if ca := st.CA; ca != nil {
		out["ca"] = map[string]any{
			"fingerprint_sha256": ca.FingerprintSHA256,
			"not_after":          rfc3339(ca.NotAfter),
			"constraint_stale":   ca.ConstraintStale,
		}
	}
	ext, probedAt := a.announcer.Snapshot()
	if ext == nil {
		ext = []string{}
	}
	out["external"] = map[string]any{"addresses": ext, "probed_at": rfc3339(probedAt)}
	out["portmap"] = a.portmapEcho(ctx)
	return out
}

// portmapEcho 端口映射诊断回显：字段直接映射 portmap.Status（mode 取动态配置）。
func (a *API) portmapEcho(ctx context.Context) map[string]any {
	echo := map[string]any{
		"mode":      a.dynVal(ctx, "portmap_mode"),
		"diagnosis": "",
		"detail":    "",
		"v6_detail": "",
		"pinholes":  []any{},
	}
	if a.portmapStatus == nil {
		return echo
	}
	st := a.portmapStatus()
	echo["diagnosis"] = string(st.Diagnosis)
	echo["detail"] = st.Detail
	echo["v6_detail"] = st.V6Detail
	holes := make([]any, 0, len(st.Pinholes))
	for _, h := range st.Pinholes {
		holes = append(holes, map[string]any{
			"proto": h.Proto, "port": h.Port, "gua": h.GUA.String(), "method": h.Method,
		})
	}
	echo["pinholes"] = holes
	return echo
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// tlsMode merged = 一个端口同时接明文与 TLS；split = 明文与 TLS 各一个端口。
func (a *API) tlsMode() string {
	if a.cfg.TLSSplit() {
		return "split"
	}
	return "merged"
}

// ---- 公开入口 ----

// caCert 根证书下载：无鉴权（要装它的设备还没信任本站），来源不是自签时 404。
func (a *API) caCert(w http.ResponseWriter, r *http.Request) {
	pem, err := a.tlsStore.CACertPEM()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="hearth-ca.crt"`)
	w.Write(pem)
}

// httpsPort 当前 https 的端口：合并模式与 http 同号，分开模式取 HTTPS_ADDR 的端口。
func (a *API) httpsPort() string {
	addr := a.cfg.Addr
	if a.cfg.TLSSplit() {
		addr = a.cfg.HTTPSAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}
