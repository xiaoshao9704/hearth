// 内置反向代理：/lk/ → LiveKit 信令（剥前缀，含 WebSocket Upgrade），/w/ → Ingress WHIP（保留路径）。
// 部署因此不再强制需要 Caddy/nginx，TLS 由用户自选（也可裸 HTTP 内网用）。
package api

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// RegisterProxies 注册内置反代路由（chi 的 "/lk/*" 通配匹配子树，比静态托管的 "/*" 更具体，优先命中）。
func (a *API) RegisterProxies(r chi.Router) {
	// LiveKit 信令：/lk/rtc → 上游 /rtc（等价生产 Caddy 的 handle_path 剥前缀）
	if target, err := url.Parse(a.cfg.LiveKitAPIURL); err == nil && target.Host != "" {
		r.Handle("/lk/*", newReverseProxy(target, "/lk"))
	} else {
		log.Printf("LIVEKIT_API_URL 无效（%v），/lk/ 代理未启用", err)
	}
	// WHIP 推流：保留完整 /w/{streamKey} 路径（Ingress 需要 key 在路径里）
	if a.cfg.IngressUpstreamURL != "" {
		if target, err := url.Parse(a.cfg.IngressUpstreamURL); err == nil && target.Host != "" {
			r.Handle("/w/*", newReverseProxy(target, ""))
		} else {
			log.Printf("INGRESS_UPSTREAM_URL 无效（%v），/w/ 代理未启用", err)
		}
	}
}

// newReverseProxy 转发到 target；stripPrefix 非空时剥掉路径前缀。
func newReverseProxy(target *url.URL, stripPrefix string) http.Handler {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			if stripPrefix != "" {
				p := strings.TrimPrefix(req.URL.Path, stripPrefix)
				if p == "" {
					p = "/"
				}
				req.URL.Path = p
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("反代上游失败: %v", err)
			writeErr(w, http.StatusBadGateway, "上游不可达")
		},
	}
}
