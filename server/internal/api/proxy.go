// 内置反向代理与接入分发：内核接入路径统一挂在 /providers/{alias}/ 下，按 alias 找到
// 注册实例后按子路径分发——livekit 信令反代（/rtc/*，剥前缀，含 WebSocket Upgrade）、
// WHIP 推流（/w[/*]）。上游由实例逐请求声明（配置改动即时生效）。
// 部署因此不再强制需要 Caddy/nginx，TLS 由用户自选（也可裸 HTTP 内网用）。
package api

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"hearth/server/internal/rtc"
)

// RegisterProxies 注册接入分发路由（chi 的 "/providers/{alias}/*" 通配匹配子树，比静态托管的 "/*" 更具体，优先命中）。
func (a *API) RegisterProxies(r chi.Router) {
	r.Handle("/providers/{alias}/*", http.HandlerFunc(a.serveProvider))
}

// serveProvider 按 alias + 子路径分发（alias 与子路径都取自 chi 路由参数）。
// 子路径形状：
//
//	/providers/{alias}/rtc/*   livekit 信令反代（剥 /providers/{alias} 前缀）
//	/providers/{alias}/w[/*]   WHIP 推流（r.URL.Path 改写为 /w 段后复用 WHIP 逻辑）
func (a *API) serveProvider(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	inst := a.instance(alias)
	if inst == nil {
		writeErr(w, http.StatusNotFound, "服务实例不存在")
		return
	}
	sub := "/" + chi.URLParam(r, "*")
	switch {
	case sub == "/w" || strings.HasPrefix(sub, "/w/"):
		if inst.Ingest == nil {
			writeErr(w, http.StatusNotFound, "该实例无推流能力")
			return
		}
		r.URL.Path = sub // WHIP 逻辑与上游都按 /w 形状工作，剥到 /w 后原样复用
		a.serveWHIP(w, r, inst)
	case sub == "/rtc" || strings.HasPrefix(sub, "/rtc/"):
		// livekit 类型实例的信令代理：Voice/Stage 槽位是同一个 Provider 对象
		p := inst.Voice
		if p == nil {
			p = inst.Stage
		}
		if p == nil || (inst.Type != TypeLivekit && inst.Type != TypeLivekitEmbedded) {
			writeErr(w, http.StatusNotFound, "该实例无信令代理")
			return
		}
		a.proxyTo(w, r, p.SignalProxyUpstream(r.Context()), "/providers/"+alias)
	default:
		writeErr(w, http.StatusNotFound, "未知路径")
	}
}

// serveWHIP WHIP 推流处理（实例来自路径 alias，admitIngest 已保证它就是当前舞台实例）。
// 两种推流填法都支持：路径含令牌（POST /w/{channel}/{token}，ffmpeg 等）与 OBS 官方的
// bearer 模式（服务器填 /w/{channel}、令牌放 Authorization: Bearer）。
// 令牌是每用户一把的用户凭证，房间在 URL 里；POST 一律先过 admitIngest（definitive：
// 404/403/503，不再 fail-open），通过后交给实例推流面（livekitrtc.WHIP，现签短时效
// LiveKit JWT 换票反代到该实例自带的 /whip/v1）：判定结果挂 ctx，resolver 原样取回。
// 应答的 Location（/w/sessions/{rid}）改写成 /providers/{alias}/w/sessions/{rid} 再回客户端。
func (a *API) serveWHIP(w http.ResponseWriter, req *http.Request, inst *ProviderInstance) {
	if req.Method == http.MethodPost {
		channel, token, _ := rtc.WHIPToken(req)
		adm, ok := a.admitIngest(req.Context(), w, inst.Alias, channel, token)
		if !ok {
			return
		}
		ws, ok := inst.Ingest.(rtc.WHIPServer)
		if !ok {
			writeErr(w, http.StatusServiceUnavailable, "推流入口暂时不可用，请稍后再试")
			return
		}
		req = req.WithContext(context.WithValue(req.Context(), ingestCtxKey{}, adm))
		ws.ServeWHIP(whipLocationWriter{w, "/providers/" + inst.Alias}, req, token)
		return
	}
	// 会话收尾（/w/sessions/{rid}）按归属路由：推流中途切换 stage_provider 时，
	// PATCH/DELETE 仍要送达创建该会话的实例
	rid := rtc.WHIPSessionRID(req)
	for _, other := range a.listInstances(req.Context()) {
		if ws, ok := other.Ingest.(rtc.WHIPServer); ok && ws.HasSession(rid) {
			ws.ServeWHIP(w, req, rid)
			return
		}
	}
	if ws, ok := inst.Ingest.(rtc.WHIPServer); ok {
		ws.ServeWHIP(w, req, rid)
		return
	}
	writeErr(w, http.StatusNotFound, "该实例无推流能力")
}

// rewriteWHIPLocation 把 WHIP 应答的会话资源地址改写回同源代理路径，
// 使客户端的 PATCH/DELETE 能打回 hearth 而不是上游。三种形态：
//
//	绝对 URL   http://upstream:8080/w/abc → {prefix}/w/abc（上游主机对客户端不可达，只取路径）
//	根相对     /w/sessions/rid           → {prefix}/w/sessions/rid
//	纯相对     sessions/rid              → 原样（客户端按请求路径解析，本就落在代理路径下）
//
// 上游返回什么形态由它自己决定（hearth 自产的是根相对形式，外部 LiveKit 则不受我们
// 控制），所以三种都要认。非 /w/ 开头的一律不动。
func rewriteWHIPLocation(loc, prefix string) string {
	if loc == "" {
		return loc
	}
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	// 纯相对（无 scheme/host 且不以 / 开头）本就相对请求路径解析，改写反而会错
	if u.Scheme == "" && u.Host == "" && !strings.HasPrefix(loc, "/") {
		return loc
	}
	if !strings.HasPrefix(u.Path, "/w/") {
		return loc
	}
	out := prefix + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// whipLocationWriter WHIP 应答的 Location 改写（反代形态的等价处理见 proxyToLoc）。
type whipLocationWriter struct {
	http.ResponseWriter
	prefix string
}

func (w whipLocationWriter) WriteHeader(code int) {
	if loc := w.Header().Get("Location"); loc != "" {
		w.Header().Set("Location", rewriteWHIPLocation(loc, w.prefix))
	}
	w.ResponseWriter.WriteHeader(code)
}

// proxyTo 反代到逐请求解析的上游；未配置 503，地址无效 502。stripPrefix 非空时剥路径前缀。
func (a *API) proxyTo(w http.ResponseWriter, req *http.Request, upstream, stripPrefix string) {
	a.proxyToLoc(w, req, upstream, stripPrefix, "")
}

// proxyToLoc 同 proxyTo；locPrefix 非空时把应答 Location 的 /w/... 改写为
// {locPrefix}/w/...（WHIP 会话资源地址回指同源代理路径，进程内形态见 whipLocationWriter）。
func (a *API) proxyToLoc(w http.ResponseWriter, req *http.Request, upstream, stripPrefix, locPrefix string) {
	if upstream == "" {
		writeErr(w, http.StatusServiceUnavailable, "上游未配置")
		return
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" {
		log.Printf("反代上游地址无效（%q）: %v", upstream, err)
		writeErr(w, http.StatusBadGateway, "上游地址无效")
		return
	}
	newReverseProxy(target, stripPrefix, locPrefix).ServeHTTP(w, req)
}

// newReverseProxy 转发到 target；stripPrefix 非空时剥掉路径前缀。
// 上游 URL 可带路径前缀与固定参数（如 http://host/sub?key=x）：
// 路径拼在请求路径前，参数并入请求 query（与标准库单主机反代语义一致）。
// locPrefix 非空时把应答 Location 改写回同源代理路径（见 rewriteWHIPLocation）。
func newReverseProxy(target *url.URL, stripPrefix, locPrefix string) http.Handler {
	rp := &httputil.ReverseProxy{
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
	if locPrefix != "" {
		rp.ModifyResponse = func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteWHIPLocation(loc, locPrefix))
			}
			return nil
		}
	}
	return rp
}
