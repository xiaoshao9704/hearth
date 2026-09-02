// 内置反向代理与接入分发：内核接入路径统一挂在 /providers/{alias}/ 下，按 alias 找到
// 注册实例后按子路径分发——livekit 信令反代（/rtc/*，剥前缀，含 WebSocket Upgrade）、
// ember WS 信令（/voice）、WHIP 推流（/w[/*]）。上游由实例逐请求声明（配置改动即时生效）。
// 部署因此不再强制需要 Caddy/nginx，TLS 由用户自选（也可裸 HTTP 内网用）。
package api

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// RegisterProxies 注册接入分发路由（chi 的 "/providers/{alias}/*" 通配匹配子树，比静态托管的 "/*" 更具体，优先命中）。
func (a *API) RegisterProxies(r chi.Router) {
	r.Handle("/providers/{alias}/*", http.HandlerFunc(a.serveProvider))
}

// serveProvider 按 alias + 子路径分发（alias 与子路径都取自 chi 路由参数）。
// 子路径形状：
//
//	/providers/{alias}/rtc/*   livekit 信令反代（剥 /providers/{alias} 前缀）
//	/providers/{alias}/voice   ember 类型实例的 WS 信令（当前仅内建 ember 可达）
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
	case sub == "/voice" && inst.Type == TypeEmber:
		a.voiceWS(w, r)
	case sub == "/w" || strings.HasPrefix(sub, "/w/"):
		if inst.Ingest == nil {
			writeErr(w, http.StatusNotFound, "该实例无推流能力")
			return
		}
		r.URL.Path = sub // WHIP 逻辑与上游都按 /w 形状工作，剥到 /w 后原样复用
		a.serveWHIP(w, r, inst)
	case sub == "/rtc" || strings.HasPrefix(sub, "/rtc/"):
		if inst.Type != TypeLivekit {
			writeErr(w, http.StatusNotFound, "该实例无信令代理")
			return
		}
		a.proxyTo(w, r, inst.Voice.SignalProxyUpstream(r.Context()), "/providers/"+alias)
	default:
		writeErr(w, http.StatusNotFound, "未知路径")
	}
}

// serveWHIP WHIP 推流处理（实例来自路径 alias）。
// 反代保留完整 /w/{streamKey} 路径（推流端点需要 key 在路径里）。两种推流填法都支持：
// 路径含 key（POST /w/{key}，ffmpeg 等）与 OBS 官方的 bearer 模式（服务器填 /w、密钥放
// Authorization: Bearer——ingress 要求精确 /w，带尾斜杠的 /w/ 会 404，这里做规范化宽容）。
// WHIP publish（POST）按路径 alias 的实例裁决：远端 Bellows（实现了通行证签发且有上游）
// 判定在反代前做完并签成 grant 带给远端，definitive（404/403/503），不再 fail-open；
// 其他类型保持 fail-open 拦截（channel_gags：被禁言者不能靠重推 OBS 绕过禁言——
// ingress 参与者自带发布权限，不经过 joinToken 的 canPublish 检查）。
func (a *API) serveWHIP(w http.ResponseWriter, req *http.Request, inst *ProviderInstance) {
	key, bearer := rtc.WHIPToken(req)
	if req.Method == http.MethodPost {
		if bearer {
			req.URL.Path = "/w" // bearer 模式端点规范化为精确 /w
		}
		if gi, ok := inst.Ingest.(rtc.WHIPGrantIssuer); ok && inst.Ingest.ProxyUpstream(req.Context()) != "" {
			if !a.admitWhipRemote(w, req, gi, inst.Alias, key) {
				return
			}
		} else if allow, mismatch := a.canPublishByStreamKey(req.Context(), key, inst.Alias); !allow {
			if mismatch {
				writeErr(w, http.StatusNotFound, "推流密钥与该入口不匹配")
			} else {
				writeErr(w, http.StatusForbidden, "你已被禁言，无法推流")
			}
			return
		}
	} else {
		// 会话收尾按归属路由：推流中途切换 ingest_provider 时，
		// PATCH/DELETE 仍要送达创建该会话的进程内网关
		for _, other := range a.listInstances(req.Context()) {
			if other.Ingest == nil {
				continue
			}
			if ws, ok := other.Ingest.(rtc.WHIPServer); ok && ws.HasSession(key) {
				ws.ServeWHIP(w, req, key)
				return
			}
		}
	}
	// 有上游就反代（外部 ingress / 远端 bellows）；没有上游且能进程内处理（bellows）就直接处理
	if ws, ok := inst.Ingest.(rtc.WHIPServer); ok && inst.Ingest.ProxyUpstream(req.Context()) == "" {
		ws.ServeWHIP(w, req, key)
		return
	}
	a.proxyTo(w, req, inst.Ingest.ProxyUpstream(req.Context()), "")
}

// admitWhipRemote 远端 Bellows 的 POST 处理：读取并回填请求体（保持显式 ContentLength，
// 避免反代变 chunked），definitive 入场判定后签发通行证随反代带给远端。
// 通行证模型下远端不再回调，这里就是最终裁决：密钥不存在 404、归属与路径 alias 不符 404、
// 不许推 403、查询出错 503。返回 false 时响应已写好。
func (a *API) admitWhipRemote(w http.ResponseWriter, req *http.Request, gi rtc.WHIPGrantIssuer, alias, key string) bool {
	offer, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 256<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取 SDP offer 失败")
		return false
	}
	req.Body = io.NopCloser(bytes.NewReader(offer))
	req.ContentLength = int64(len(offer))
	c, u, provider, err := a.ingressOwner(req.Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "推流密钥无效或已重置")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "推流网关暂时不可用，请稍后再试")
		return false
	}
	// 密钥是全局命名空间：记录归属必须等于路径实例，否则能用任一远端实例的
	// secret 签 grant 把流发进另一套 LiveKit 的同名房间
	if provider != alias {
		writeErr(w, http.StatusNotFound, "推流密钥与该入口不匹配")
		return false
	}
	adm, ok, reason, err := a.admitUser(req.Context(), c, u)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "推流网关暂时不可用，请稍后再试")
		return false
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return false
	}
	if !adm.CanPublish {
		writeErr(w, http.StatusForbidden, "你已被禁言，无法推流")
		return false
	}
	h, v, err := gi.IssueWHIPGrant(req.Context(), key, c.Name, u.Username, offer)
	if err != nil {
		log.Printf("签发推流通行证失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return false
	}
	req.Header.Set(h, v)
	return true
}

// proxyTo 反代到逐请求解析的上游；未配置 503，地址无效 502。stripPrefix 非空时剥路径前缀。
func (a *API) proxyTo(w http.ResponseWriter, req *http.Request, upstream, stripPrefix string) {
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
	newReverseProxy(target, stripPrefix).ServeHTTP(w, req)
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
