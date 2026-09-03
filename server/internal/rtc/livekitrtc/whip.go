// LiveKit 自带 WHIP 入口（/whip/v1）的推流适配器。与 livekit-ingress 形态的区别在于
// 没有「令牌 → 上游 stream key」的持久端点：房间与身份都写在每次 POST 现签的短时效
// LiveKit JWT 里，hearth 终结用户令牌、向上游出示自己签的凭证，端点三方法因此是空实现。
//
// 上游是哪一个 LiveKit 由实例的 livekit_api_url 决定，三种形态同一份代码：进程内
// lkembed（回环）、远端 cmd/stage（私网地址）、外部 LiveKit 实例。远端形态因此不再需要
// Bellows 转发——OBS 的媒体与浏览器观众走同一条打洞路径。
//
// 反代由本类型自己做（ProxyUpstream 返回空，接入层把 /w 请求整个交过来）：会话资源地址
// 要换成不透明会话 id、PATCH/DELETE 要按会话现签新票，这两件事都在 rtc 的通用反代里
// 表达不出来。
package livekitrtc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"hearth/server/internal/lkroom"
	"hearth/server/internal/lktoken"
	"hearth/server/internal/rtc"
)

// whipPath LiveKit 服务端 WHIP 端点（RFC 9725，会话资源地址形如 {whipPath}/{participantId}）。
const whipPath = "/whip/v1"

// whipSessionTTL 会话记录的兜底存活期：推流端崩溃时不会有 DELETE 打回来，
// 靠它在下一次 POST 时把陈旧记录扫掉（真实会话早已被 LiveKit 的 ICE 断连检测清掉）。
const whipSessionTTL = 12 * time.Hour

// whipTimeout 上游 WHIP 请求的上限。POST 要等 LiveKit 那侧 gathering 完成才出 answer，
// 给得比常规调用宽一些。
const whipTimeout = 30 * time.Second

// ResolveFunc 按推流令牌取回接入层已做完的入场判定结果（房间 = 频道名，identity 与 meta
// 由接入层按 rtc.Identity / rtc.Meta 组好）。本类型不做任何身份拼装与二次判定。
type ResolveFunc func(ctx context.Context, token string) (room, identity string, meta rtc.Meta, err error)

// whipSession 一次进行中的推流会话。记的是「会话资源 id → 上游参与者」的映射：
// PATCH/DELETE 要按它反查上游路径并现签新票，同令牌换房间重推要按它把旧房间里的
// 同一发布身份移出（LiveKit 只保证房间内 identity 唯一，跨房间会两个会话并存，
// 而推流令牌的语义是一台设备同时只推一个房间）。
type whipSession struct {
	rid      string // 会话资源 id（对外 Location /w/sessions/{rid}）
	token    string // 推流令牌（撤销与重推顶替按它归组）
	room     string
	identity string
	meta     rtc.Meta
	path     string // 上游会话资源路径（/whip/v1/{participantId}）
	at       time.Time
}

// WHIP 实现 rtc.IngestProvider 与 rtc.WHIPServer。
type WHIP struct {
	cfg     rtc.ConfigFunc
	resolve ResolveFunc
	// ready 推流出口当前是否真的在跑（进程内 LiveKit 只在被选作舞台线时才启动）；
	// nil = 只看配置齐不齐。
	ready func(ctx context.Context) bool
	hc    *http.Client

	mu               sync.Mutex
	url, key, secret string
	rooms            *lkroom.Client
	sessions         map[string]*whipSession
}

// NewWHIP resolve 由接入层注入（判定结果经请求 ctx 传来）；ready 见字段注释。
func NewWHIP(cfg rtc.ConfigFunc, resolve ResolveFunc, ready func(ctx context.Context) bool) *WHIP {
	return &WHIP{cfg: cfg, resolve: resolve, ready: ready,
		hc: &http.Client{Timeout: whipTimeout}, sessions: map[string]*whipSession{}}
}

// client 按当前生效配置取房间客户端（移出参与者用）；配置变了就重建。
func (h *WHIP) client(ctx context.Context) *lkroom.Client {
	url, key, secret := h.apiURL(ctx), h.cfg(ctx, "livekit_api_key"), h.cfg(ctx, "livekit_api_secret")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms == nil || url != h.url || key != h.key || secret != h.secret {
		h.url, h.key, h.secret = url, key, secret
		h.rooms = lkroom.NewClient(url, key, secret)
	}
	return h.rooms
}

func (h *WHIP) apiURL(ctx context.Context) string {
	return strings.TrimSuffix(strings.TrimSpace(h.cfg(ctx, "livekit_api_url")), "/")
}

// whipBase 上游 WHIP 入口的基地址。LiveKit 把 /whip/v1 与 Twirp API 挂在同一个 HTTP
// 端口上，所以直接由 livekit_api_url 推导，不另开配置键；ws(s):// 写法（照抄浏览器信令
// 地址的常见填法）归一成 http(s)://，否则 http.Client 认不得这个 scheme。
func (h *WHIP) whipBase(ctx context.Context) string {
	return httpScheme(h.apiURL(ctx))
}

// httpScheme ws→http、wss→https；其余原样返回（含解析不了与没有 host 的填法）。
func httpScheme(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return raw
	}
	return strings.TrimSuffix(u.String(), "/")
}

// ---- rtc.IngestProvider ----

func (h *WHIP) Name() string { return "livekit-whip" }

func (h *WHIP) Enabled(ctx context.Context) bool {
	if h.apiURL(ctx) == "" || h.cfg(ctx, "livekit_api_key") == "" || h.cfg(ctx, "livekit_api_secret") == "" {
		return false
	}
	return h.ready == nil || h.ready(ctx)
}

// ProxyUpstream 恒为空：反代在 ServeWHIP 里自己做（见包注释）。
func (h *WHIP) ProxyUpstream(context.Context) string { return "" }

// RevokeToken 掐断该令牌名下的全部进行会话（令牌重置时调用；幂等尽力）。
func (h *WHIP) RevokeToken(ctx context.Context, token string) error {
	for _, s := range h.takeSessions(func(s *whipSession) bool { return s.token == token }) {
		h.removeParticipant(ctx, s)
	}
	return nil
}

// EnsureEndpoint 无持久端点：凭证是每次 POST 现签的短时效 JWT（见包注释）。
func (h *WHIP) EnsureEndpoint(context.Context, string, string, rtc.Meta) (id, upstreamKey string, err error) {
	return "", "", nil
}

// BindRoom 无持久端点，空操作：房间写在每次现签的票里。
func (h *WHIP) BindRoom(context.Context, string, string) error { return nil }

// DeleteEndpoint 无持久端点，空操作（进行会话的掐断走 RevokeToken）。
func (h *WHIP) DeleteEndpoint(context.Context, string) error { return nil }

// ---- rtc.WHIPServer ----

func (h *WHIP) HasSession(rid string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[rid] != nil
}

func (h *WHIP) ServeWHIP(w http.ResponseWriter, r *http.Request, token string) {
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r, token)
	case http.MethodPatch:
		h.handlePatch(w, r, token)
	case http.MethodDelete:
		h.handleDelete(w, r, token)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *WHIP) handlePost(w http.ResponseWriter, r *http.Request, token string) {
	ctx := r.Context()
	if token == "" {
		http.Error(w, "缺少推流令牌", http.StatusBadRequest)
		return
	}
	if h.resolve == nil {
		http.Error(w, "推流入口未接入判定", http.StatusServiceUnavailable)
		return
	}
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		http.Error(w, "读取 SDP offer 失败", http.StatusBadRequest)
		return
	}
	room, identity, meta, err := h.resolve(ctx, token)
	if err != nil {
		http.Error(w, "推流入口暂时不可用，请稍后再试", http.StatusServiceUnavailable)
		return
	}
	resp, body, err := h.upstream(ctx, http.MethodPost, whipPath, room, meta, offer,
		map[string]string{"Content-Type": "application/sdp"})
	if err != nil {
		log.Printf("WHIP 上游不可达（identity=%s 房间=%s）: %v", identity, room, err)
		http.Error(w, "推流上游不可达", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusCreated {
		// 上游的拒绝原因（编码不受支持等）原样回给推流端，别吞成笼统错误
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}
	rid, err := randHex(16)
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	s := &whipSession{rid: rid, token: token, room: room, identity: identity, meta: meta,
		path: resp.Header.Get("Location"), at: time.Now()}
	// 同令牌旧会话顶替：同房间同身份的那条由 LiveKit 自己按 identity 唯一顶掉
	//（此刻新参与者已在房，按 identity 移除会误伤新会话），只处理换房间/换标签的情况
	for _, old := range h.takeSessions(func(o *whipSession) bool { return o.token == token }) {
		if old.room == room && old.identity == identity {
			continue
		}
		h.removeParticipant(ctx, old)
	}
	h.mu.Lock()
	h.pruneLocked()
	h.sessions[rid] = s
	h.mu.Unlock()

	// 资源地址用一次性会话 id：既不让 bearer 模式的令牌回流进 URL/访问日志，
	// 也不把上游的参与者 id 暴露给推流端
	w.Header().Set("Content-Type", "application/sdp")
	if et := resp.Header.Get("ETag"); et != "" {
		w.Header().Set("ETag", et) // PATCH 的 If-Match 用
	}
	w.Header().Set("Location", "/w/sessions/"+rid)
	// ffmpeg 的 WHIP muxer 读不了 chunked 响应，必须显式 Content-Length
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// 上游的 Link: rel="ice-server" 不透传（与 Bellows 一致）：那是 LiveKit 内置的默认
	// STUN 列表，不一定可达，推流端照着 gather 只会拖慢建连
	w.WriteHeader(http.StatusCreated)
	w.Write(body)
	log.Printf("WHIP 会话建立: identity=%s 房间=%s", identity, room)
}

// handlePatch trickle / ICE restart：按会话资源 id 反查上游路径，现签新票转发。
func (h *WHIP) handlePatch(w http.ResponseWriter, r *http.Request, rid string) {
	s := h.session(rid)
	if s == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}
	frag, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		http.Error(w, "读取 SDP 片段失败", http.StatusBadRequest)
		return
	}
	head := map[string]string{"Content-Type": r.Header.Get("Content-Type")}
	if m := r.Header.Get("If-Match"); m != "" {
		head["If-Match"] = m
	}
	resp, body, err := h.upstream(r.Context(), http.MethodPatch, s.path, s.room, s.meta, frag, head)
	if err != nil {
		log.Printf("WHIP 上游 PATCH 失败（identity=%s）: %v", s.identity, err)
		http.Error(w, "推流上游不可达", http.StatusBadGateway)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if et := resp.Header.Get("ETag"); et != "" {
		w.Header().Set("ETag", et)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// handleDelete 结束推流：摘会话后向上游转发，幂等回 204（上游 DELETE 不幂等，
// 会话已消失时的 404 不该翻给推流端）。
func (h *WHIP) handleDelete(w http.ResponseWriter, r *http.Request, rid string) {
	h.mu.Lock()
	s := h.sessions[rid]
	delete(h.sessions, rid)
	h.mu.Unlock()
	if s != nil {
		if _, _, err := h.upstream(r.Context(), http.MethodDelete, s.path, s.room, s.meta, nil, nil); err != nil {
			log.Printf("WHIP 上游 DELETE 失败（identity=%s）: %v", s.identity, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 内部 ----

// upstream 向上游 WHIP 端点发一次请求，Authorization 用为该会话现签的短时效 LiveKit JWT
//（房间与 identity 都在票里，上游不看 URL）。返回响应与已读完的响应体。
func (h *WHIP) upstream(ctx context.Context, method, path, room string, meta rtc.Meta,
	body []byte, header map[string]string) (*http.Response, []byte, error) {

	target, err := h.resolveUpstream(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	tok, err := lktoken.Sign(h.cfg(ctx, "livekit_api_key"), h.cfg(ctx, "livekit_api_secret"), room, meta, true)
	if err != nil {
		return nil, nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range header {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, out, nil
}

// resolveUpstream 把上游给的会话资源地址（相对路径或绝对 URL）解析成本次请求的目标地址。
func (h *WHIP) resolveUpstream(ctx context.Context, path string) (string, error) {
	base := h.whipBase(ctx)
	if base == "" {
		return "", errors.New("livekit_api_url 未配置")
	}
	if strings.HasPrefix(path, "/") {
		return base + path, nil
	}
	u, err := url.Parse(base + whipPath + "/")
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	return u.ResolveReference(ref).String(), nil
}

func (h *WHIP) session(rid string) *whipSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[rid]
}

// takeSessions 摘出并返回满足条件的会话（摘出后调用方负责收尾）。
func (h *WHIP) takeSessions(match func(*whipSession) bool) []*whipSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*whipSession
	for rid, s := range h.sessions {
		if match(s) {
			delete(h.sessions, rid)
			out = append(out, s)
		}
	}
	return out
}

// pruneLocked 扫掉超期的会话记录（须持 mu）：推流端崩溃时没有 DELETE 打回来，
// 真实会话早被 LiveKit 的断连检测清掉，记录留着只会白占内存。
func (h *WHIP) pruneLocked() {
	deadline := time.Now().Add(-whipSessionTTL)
	for rid, s := range h.sessions {
		if s.at.Before(deadline) {
			delete(h.sessions, rid)
		}
	}
}

// removeParticipant 把该会话的发布身份移出房间（立刻关掉推流端的 PeerConnection）。
// 归属约束仍走 rtc.MatchesUser：传的 identity 不属于 meta.UID 时不会误伤别人。
func (h *WHIP) removeParticipant(ctx context.Context, s *whipSession) {
	if _, err := h.client(ctx).RemoveParticipantsOf(ctx, s.room, s.meta.UID, s.identity); err != nil {
		log.Printf("移出 WHIP 推流参与者失败（identity=%s 房间=%s）: %v", s.identity, s.room, err)
	}
}

func randHex(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
