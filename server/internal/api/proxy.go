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
)

// RegisterProxies 注册内置反代路由（chi 的 "/lk/*" 通配匹配子树，比静态托管的 "/*" 更具体，优先命中）。
// 上游地址由当前内核实现逐请求声明（配置改动即时生效）。
func (a *API) RegisterProxies(r chi.Router) {
	// 信令代理：/lk/rtc → 上游 /rtc（剥前缀，等价生产 Caddy 的 handle_path）
	r.Handle("/lk/*", a.dynProxy("/lk", func(req *http.Request) string {
		return a.rtcProvider(req.Context()).SignalProxyUpstream(req.Context())
	}))
	// 推流代理：保留完整 /w/{streamKey} 路径（推流端点需要 key 在路径里）
	r.Handle("/w/*", a.dynProxy("", func(req *http.Request) string {
		return a.ingestProvider(req.Context()).ProxyUpstream(req.Context())
	}))
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
