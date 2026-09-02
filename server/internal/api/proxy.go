// 内置反向代理：/lk/ → 房间内核信令（剥前缀，含 WebSocket Upgrade），/w/ → 推流入口（保留路径）。
// 上游由当前内核实现声明。部署因此不再强制需要 Caddy/nginx，TLS 由用户自选（也可裸 HTTP 内网用）。
package api

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"hearth/server/internal/rtc"
)

// RegisterProxies 注册内置反代路由（chi 的 "/lk/*" 通配匹配子树，比静态托管的 "/*" 更具体，优先命中）。
// 上游地址由当前内核实现逐请求声明（配置改动即时生效）。
func (a *API) RegisterProxies(r chi.Router) {
	// 信令代理：/lk/rtc → 上游 /rtc（剥前缀，等价生产 Caddy 的 handle_path）
	r.Handle("/lk/*", a.dynProxy("/lk", func(req *http.Request) string {
		if sp := a.stageProvider(req.Context()); sp != nil {
			if u := sp.SignalProxyUpstream(req.Context()); u != "" {
				return u
			}
		}
		return a.voiceProvider(req.Context()).SignalProxyUpstream(req.Context())
	}))
	// 推流代理：保留完整 /w/{streamKey} 路径（推流端点需要 key 在路径里）。
	// WHIP publish（POST）先按 channel_gags 拦截：被禁言者不能靠重推 OBS 绕过禁言
	//（ingress 参与者自带发布权限，不经过 joinToken 的 canPublish 检查）。
	wProxy := a.dynProxy("", func(req *http.Request) string {
		return a.ingestProvider(req.Context()).ProxyUpstream(req.Context())
	})
	// 两种推流填法都支持：路径含 key（POST /w/{key}，ffmpeg 等）与 OBS 官方的
	// bearer 模式（服务器填 /w、密钥放 Authorization: Bearer——ingress 要求精确 /w，
	// 带尾斜杠的 /w/ 会 404，这里做规范化宽容）。
	whipHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key, bearer := rtc.WHIPToken(req)
		if req.Method == http.MethodPost {
			if bearer {
				req.URL.Path = "/w" // bearer 模式端点规范化为精确 /w
			}
			if !a.canPublishByStreamKey(req.Context(), key) {
				writeErr(w, http.StatusForbidden, "你已被禁言，无法推流")
				return
			}
		} else {
			// 会话收尾按归属路由：推流中途切换 ingest_provider 时，
			// PATCH/DELETE 仍要送达创建该会话的进程内网关
			for _, ik := range a.ingestKernels {
				if ws, ok := ik.(rtc.WHIPServer); ok && ws.HasSession(key) {
					ws.ServeWHIP(w, req, key)
					return
				}
			}
		}
		// 有上游就反代（外部 ingress / 远端 bellows）；没有上游且能进程内处理（bellows）就直接处理
		ip := a.ingestProvider(req.Context())
		if ws, ok := ip.(rtc.WHIPServer); ok && ip.ProxyUpstream(req.Context()) == "" {
			ws.ServeWHIP(w, req, key)
			return
		}
		wProxy.ServeHTTP(w, req)
	})
	r.Handle("/w", whipHandler)
	r.Handle("/w/*", whipHandler)
}

// dynProxy 目标逐请求解析的反代；未配置返回 503。
func (a *API) dynProxy(stripPrefix string, upstream func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t := upstream(req)
		if t == "" {
			writeErr(w, http.StatusServiceUnavailable, "上游未配置")
			return
		}
		target, err := url.Parse(t)
		if err != nil || target.Host == "" {
			log.Printf("反代上游地址无效（%q）: %v", t, err)
			writeErr(w, http.StatusBadGateway, "上游地址无效")
			return
		}
		newReverseProxy(target, stripPrefix).ServeHTTP(w, req)
	})
}

// newReverseProxy 转发到 target；stripPrefix 非空时剥掉路径前缀。
// 上游 URL 可带路径前缀与固定参数（如 http://host/sub?key=x）：
// 路径拼在请求路径前，参数并入请求 query（与标准库单主机反代语义一致）。
func newReverseProxy(target *url.URL, stripPrefix string) http.Handler {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			p := req.URL.Path
			if stripPrefix != "" {
				p = strings.TrimPrefix(p, stripPrefix)
			}
			if base := strings.TrimSuffix(target.Path, "/"); base != "" {
				p = base + p
			}
			if p == "" {
				p = "/"
			}
			req.URL.Path = p
			if target.RawQuery != "" {
				if req.URL.RawQuery == "" {
					req.URL.RawQuery = target.RawQuery
				} else {
					req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("反代上游失败: %v", err)
			writeErr(w, http.StatusBadGateway, "上游不可达")
		},
	}
}
