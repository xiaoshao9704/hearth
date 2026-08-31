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
// 上游地址逐请求解析（环境变量 / 后台设置），后台改配置即时生效。
func (a *API) RegisterProxies(r chi.Router) {
	// LiveKit 信令：/lk/rtc → 上游 /rtc（等价生产 Caddy 的 handle_path 剥前缀）
	r.Handle("/lk/*", a.dynProxy("/lk", "livekit_api_url"))
	// WHIP 推流：保留完整 /w/{streamKey} 路径（Ingress 需要 key 在路径里）
	r.Handle("/w/*", a.dynProxy("", "ingress_upstream_url"))
}

// dynProxy 目标按当前生效配置解析的反代；未配置返回 503。
func (a *API) dynProxy(stripPrefix, cfgName string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t := a.dynVal(req.Context(), cfgName)
		if t == "" {
			writeErr(w, http.StatusServiceUnavailable, "上游未配置")
			return
		}
		target, err := url.Parse(t)
		if err != nil || target.Host == "" {
			log.Printf("反代上游地址无效（%s=%q）: %v", cfgName, t, err)
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
