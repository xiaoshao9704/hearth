// REST API：注册/登录/频道/进房令牌/频道管理。路由用 chi，鉴权用 Bearer token。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"hearth/server/internal/chat"
	"hearth/server/internal/config"
	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/livekitrtc"
	"hearth/server/internal/rtc/pionvoice"
	"hearth/server/internal/store"

	"crypto/rand"
	"encoding/hex"
	"log"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

type API struct {
	st  *store.Store
	cfg config.Config
	hub *chat.Hub

	// 内核注册表：接入层只依赖 rtc 接口，具体实现按选择器配置取（见 dyncfg.go）
	voiceKernels  map[string]rtc.Provider
	stageKernels  map[string]rtc.Provider
	ingestKernels map[string]rtc.IngestProvider
	kernelKeys    []rtc.ConfigKey // 各实现自带的配置键汇总
	pion          *pionvoice.Provider // /api/voice 信令端点直连（进程内实现）

	// 在房人数缓存（大厅频道列表用，避免每次列表都打内核）
	countsMu sync.Mutex
	counts   map[string]int
	countsAt time.Time
}

func New(st *store.Store, cfg config.Config, hub *chat.Hub) *API {
	a := &API{st: st, cfg: cfg, hub: hub}
	// 注册内核实现：LiveKit 可同时任语音/舞台/推流入口；pion 是进程内纯音频语音内核
	lk := livekitrtc.New(a.dynVal)
	a.pion = pionvoice.New(a.dynVal)
	a.voiceKernels = map[string]rtc.Provider{"livekit": lk, "pion": a.pion}
	a.stageKernels = map[string]rtc.Provider{"livekit": lk}
	a.ingestKernels = map[string]rtc.IngestProvider{"livekit": lk}
	a.kernelKeys = append(livekitrtc.ConfigKeys(), pionvoice.ConfigKeys()...)
	return a
}

// Router 构建 chi 路由：CORS 全局挂载，认证/房主校验为分组中间件。
func (a *API) Router() *chi.Mux {
	r := chi.NewRouter()
	r.Use(a.cors)

	r.Post("/api/register", a.registerWithPolicy)
	r.Post("/api/login", a.login)
	r.Get("/api/invites/{code}", a.inviteInfo)
	r.Get("/api/voice", a.voiceWS)

	// 需登录
	r.Group(func(r chi.Router) {
		r.Use(a.auth)
		r.Post("/api/logout", a.logout)
		r.Get("/api/me", a.me)
		r.Get("/api/channels", a.listChannels)
		r.Post("/api/channels", a.createChannel)
		r.Post("/api/token", a.joinToken)
		r.Post("/api/ingress", a.getIngress)
		r.Post("/api/ingress/reset", a.resetIngress)

		// 账户设置
		r.Post("/api/account/username", a.updateUsername)
		r.Post("/api/account/password", a.updatePassword)
		r.Get("/api/account/devices", a.listMyDevices)
		r.Delete("/api/account/devices/{deviceID}", a.deleteMyDevice)

		// 频道管理：频道解析与权限校验收敛到子路由中间件
		// （踢人/封禁/静音等管理操作 = 房主或管理员，其余 = 仅房主）
		r.Route("/api/channels/{channel}", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(a.requireModerator)
				r.Post("/kick", a.kick)
				r.Post("/ban", a.ban)
				r.Post("/unban", a.unban)
				r.Post("/mute", a.mute)
				r.Post("/unmute", a.unmute)
				r.Get("/bans", a.listBans)
			})
			r.Group(func(r chi.Router) {
				r.Use(a.requireOwner)
				r.Post("/invite-only", a.setInviteOnly)
				r.Get("/members", a.listMembers)
				r.Post("/members", a.addMember)
				r.Delete("/members", a.removeMember)
				r.Get("/participants", a.channelParticipants)
			})
		})

		// 管理后台
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(a.requireAdmin)
			r.Get("/overview", a.adminOverview)
			r.Get("/policy", a.adminGetPolicy)
			r.Post("/policy", a.adminSetPolicy)
			r.Get("/config", a.adminGetConfig)
			r.Post("/config", a.adminSetConfig)
			r.Get("/users", a.adminListUsers)
			r.Post("/users/{id}/disable", a.adminSetUserDisabled(true))
			r.Post("/users/{id}/enable", a.adminSetUserDisabled(false))
			r.Delete("/users/{id}", a.adminDeleteUser)
			r.Delete("/channels/{id}", a.adminDeleteChannel)
			r.Get("/invites", a.adminListInvites)
			r.Post("/invites", a.adminCreateInvite)
			r.Delete("/invites/{id}", a.adminRevokeInvite)
		})
	})
	return r
}

// roomCounts 各频道在房人数（3 秒缓存）。
func (a *API) roomCounts(ctx context.Context) (map[string]int, error) {
	a.countsMu.Lock()
	defer a.countsMu.Unlock()
	if a.counts != nil && time.Since(a.countsAt) < 3*time.Second {
		return a.counts, nil
	}
	counts, err := a.voiceProvider(ctx).RoomCounts(ctx)
	if err != nil {
		return nil, err
	}
	a.counts = counts
	a.countsAt = time.Now()
	return counts, nil
}

// ---- 中间件与上下文 ----

type ctxKey string

const (
	ctxUser    ctxKey = "user"
	ctxChannel ctxKey = "channel"
)

// userFrom 取认证中间件注入的当前用户（挂载了 auth 的路由里必非 nil）。
func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	return u
}

// channelFrom 取 requireOwner / requireModerator 中间件注入的频道。
func channelFrom(r *http.Request) *store.Channel {
	c, _ := r.Context().Value(ctxChannel).(*store.Channel)
	return c
}

// auth 认证中间件：Bearer token → 当前用户注入 context。
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "缺少登录凭证")
			return
		}
		u, err := a.st.UserByToken(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "登录已失效")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	})
}

// channelOf 解析 {channel} 频道；已写错误响应时返回 nil。
func (a *API) channelOf(w http.ResponseWriter, r *http.Request) *store.Channel {
	c, err := a.st.ChannelByName(r.Context(), chi.URLParam(r, "channel"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return nil
	}
	return c
}

// requireOwner 房主校验中间件：解析 {channel} 频道并确认当前用户是房主，频道注入 context。
func (a *API) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := a.channelOf(w, r)
		if c == nil {
			return
		}
		if c.OwnerID != userFrom(r).ID {
			writeErr(w, http.StatusForbidden, "只有房主能操作")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxChannel, c)))
	})
}

// requireModerator 房主或管理员校验中间件：用于踢人/封禁/静音等频道管理操作，频道注入 context。
func (a *API) requireModerator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := a.channelOf(w, r)
		if c == nil {
			return
		}
		if u := userFrom(r); c.OwnerID != u.ID && !u.IsAdmin {
			writeErr(w, http.StatusForbidden, "只有房主或管理员能操作")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxChannel, c)))
	})
}

// cors 跨域中间件：开发期前端在 vite dev server（不同端口）。
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", a.cfg.CORSOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BearerToken 从请求头取 token（聊天 WS 也复用）。
func BearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// ---- 注册 / 登录 ----

type credReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authResp struct {
	Token string      `json:"token"`
	User  *store.User `json:"user"`
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,32}$`)

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var req credReq
	if !decode(w, r, &req) {
		return
	}
	u, hash, err := a.st.UserByName(r.Context(), req.Username)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if u.Disabled {
		writeErr(w, http.StatusForbidden, "账号已被停用，请联系管理员")
		return
	}
	a.issueSession(w, r, u)
}

func (a *API) issueSession(w http.ResponseWriter, r *http.Request, u *store.User) {
	token, err := a.st.CreateSession(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, authResp{Token: token, User: u})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	a.st.DeleteSession(r.Context(), BearerToken(r))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, userFrom(r))
}

// ---- 频道 ----

var channelNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (a *API) listChannels(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	chs, err := a.st.ListChannels(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	counts, _ := a.roomCounts(r.Context()) // LiveKit 不可达时在线数保持 0
	for i := range chs {
		chs[i].IsOwner = chs[i].OwnerID == u.ID
		chs[i].Online = counts[chs[i].Name]
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": chs})
}

func (a *API) createChannel(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !channelNameRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "频道名仅限 1-64 位字母数字、-、_")
		return
	}
	c, err := a.st.CreateChannel(r.Context(), req.Name, u.ID)
	if store.IsUniqueViolation(err) {
		writeErr(w, http.StatusConflict, "频道已存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// ---- 进房令牌 ----

func (a *API) joinToken(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Channel  string `json:"channel"`
		DeviceID string `json:"device_id"` // 前端 localStorage 持久化的设备 ID
	}
	if !decode(w, r, &req) {
		return
	}
	c, err := a.st.ChannelByName(r.Context(), req.Channel)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 进入权限：封禁 / 邀请制白名单
	ok, reason, err := a.st.CanJoin(r.Context(), c, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return
	}
	// 禁言状态随进房凭证生效：被禁言用户签无发布权限的令牌
	gagged, err := a.st.IsGagged(r.Context(), c.ID, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	tag := a.deviceTagFor(r, req.DeviceID, u.ID)
	// 频道名即内核房间名，一一映射。语音线必发；舞台线可选；
	// 两线同一内核时标记 combined，前端用一条连接承担两种角色（即旧单线形态）。
	voiceP := a.voiceProvider(r.Context())
	stageP := a.stageProvider(r.Context())
	vc, err := voiceP.JoinCredentials(r.Context(), c.Name, u.Username, tag, !gagged)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	resp := map[string]any{"voice": a.fillCred(r, vc, c.Name)}
	combined := stageP != nil && a.dynVal(r.Context(), "voice_provider") == a.dynVal(r.Context(), "stage_provider")
	resp["combined"] = combined
	if stageP != nil && !combined {
		sc, serr := stageP.JoinCredentials(r.Context(), c.Name, u.Username, tag, !gagged)
		if serr != nil {
			log.Printf("舞台线签发失败（语音照常）: %v", serr)
		} else {
			resp["stage"] = a.fillCred(r, sc, c.Name)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// fillCred 补全内核未声明的连接信息：pion 语音走同源 /api/voice 信令并透传会话 token，
// livekit 走同源 /lk 代理。
func (a *API) fillCred(r *http.Request, c rtc.Credentials, channel string) map[string]string {
	url := c.URL
	if url == "" {
		if c.Engine == "pion-voice" {
			scheme := "ws"
			if requestScheme(r) == "https" {
				scheme = "wss"
			}
			url = scheme + "://" + r.Host + "/api/voice?channel=" + neturl.QueryEscape(channel)
		} else {
			url = a.signalURL(r)
		}
	}
	token := c.Token
	if token == "" {
		token = BearerToken(r)
	}
	return map[string]string{"engine": c.Engine, "url": url, "token": token}
}

// deviceTagFor 设备标签 = UA 推断 + 前端持久设备 ID（缺省时随机，不建档）；
// 语音线与舞台线共用，保证同一设备两线 identity 一致。
func (a *API) deviceTagFor(r *http.Request, deviceID string, userID int64) string {
	tag := deviceTag(r.UserAgent())
	if dev := deviceIDRe.FindString(deviceID); dev != "" {
		if err := a.st.RecordDevice(r.Context(), userID, dev, tag); err != nil {
			log.Printf("记录设备失败: %v", err)
		}
		return tag + "-" + dev
	}
	return tag + "-" + randHex(2)
}

// signalURL 按请求推导同源信令代理地址（/lk 路由）。
func (a *API) signalURL(r *http.Request) string {
	scheme := "ws"
	if requestScheme(r) == "https" {
		scheme = "wss"
	}
	return scheme + "://" + r.Host + "/lk"
}

// requestScheme 请求的外部协议：尊重 X-Forwarded-Proto（反代后），否则看 TLS。
func requestScheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

var deviceIDRe = regexp.MustCompile(`[a-zA-Z0-9]{4,16}`)

// randHex 生成 n 字节的随机 hex 字符串。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "x"
	}
	return hex.EncodeToString(b)
}

// deviceTag 从 User-Agent 提取简短设备标签(如 mac/iphone/win-edge),区分同账号多设备。
func deviceTag(ua string) string {
	ua = strings.ToLower(ua)
	tag := "web"
	switch {
	case strings.Contains(ua, "iphone"):
		tag = "iphone"
	case strings.Contains(ua, "ipad"):
		tag = "ipad"
	case strings.Contains(ua, "android"):
		tag = "android"
	case strings.Contains(ua, "windows"):
		tag = "win"
	case strings.Contains(ua, "mac os"), strings.Contains(ua, "macintosh"):
		tag = "mac"
	case strings.Contains(ua, "linux"):
		tag = "linux"
	}
	// 非主流浏览器补充标注(Chrome 省略)
	switch {
	case strings.Contains(ua, "edg/"):
		tag += "-edge"
	case strings.Contains(ua, "firefox/"):
		tag += "-ff"
	case !strings.Contains(ua, "chrome/") && strings.Contains(ua, "safari/"):
		tag += "-safari"
	}
	return tag
}

// ---- Ingress（OBS WHIP 推流端点，每用户每频道一个）----

type ingressResp struct {
	URL       string `json:"url"`
	StreamKey string `json:"stream_key"`
}

func (a *API) ingressURL(r *http.Request, streamKey string) string {
	base := a.ingestProvider(r.Context()).PublicBase(r.Context())
	if base == "" {
		// 未配置时按请求推导同源 /w/ 代理地址
		base = requestScheme(r) + "://" + r.Host + "/w/"
	}
	return strings.TrimSuffix(base, "/") + "/" + streamKey
}

// resolveIngressChannel 解析请求里的频道，不存在时写 404 并返回 nil。
func (a *API) resolveIngressChannel(w http.ResponseWriter, r *http.Request) *store.Channel {
	var req struct {
		Channel string `json:"channel"`
	}
	if !decode(w, r, &req) {
		return nil
	}
	c, err := a.st.ChannelByName(r.Context(), req.Channel)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return nil
	}
	return c
}

// getIngress 返回当前用户在该频道的推流地址；没有则调 LiveKit 创建并落库。
func (a *API) getIngress(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if !a.ingestProvider(r.Context()).Enabled(r.Context()) {
		writeErr(w, http.StatusServiceUnavailable, "推流入口未启用（未配置上游地址）")
		return
	}
	c := a.resolveIngressChannel(w, r)
	if c == nil {
		return
	}
	rec, err := a.st.IngressByUserChannel(r.Context(), u.ID, c.ID)
	if errors.Is(err, store.ErrNotFound) {
		rec, err = a.createIngress(r, u, c)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "获取推流地址失败")
		return
	}
	writeJSON(w, http.StatusOK, ingressResp{URL: a.ingressURL(r, rec.StreamKey), StreamKey: rec.StreamKey})
}

// resetIngress 删除旧 ingress（LiveKit 侧 + 库记录）后重建，旧地址随之失效。
func (a *API) resetIngress(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if !a.ingestProvider(r.Context()).Enabled(r.Context()) {
		writeErr(w, http.StatusServiceUnavailable, "推流入口未启用（未配置上游地址）")
		return
	}
	c := a.resolveIngressChannel(w, r)
	if c == nil {
		return
	}
	if rec, err := a.st.IngressByUserChannel(r.Context(), u.ID, c.ID); err == nil {
		if derr := a.ingestProvider(r.Context()).DeleteEndpoint(r.Context(), rec.IngressID); derr != nil {
			// LiveKit 侧删不掉不阻塞重建（记录可能已是残缺的）
			log.Printf("删除旧 ingress %s 失败: %v", rec.IngressID, derr)
		}
		a.st.DeleteIngress(r.Context(), u.ID, c.ID)
	}
	rec, err := a.createIngress(r, u, c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "重置推流地址失败")
		return
	}
	writeJSON(w, http.StatusOK, ingressResp{URL: a.ingressURL(r, rec.StreamKey), StreamKey: rec.StreamKey})
}

// createIngress 调推流入口内核创建端点并落库。
func (a *API) createIngress(r *http.Request, u *store.User, c *store.Channel) (*store.Ingress, error) {
	ingressID, streamKey, err := a.ingestProvider(r.Context()).CreateEndpoint(r.Context(), c.Name, u.Username)
	if err != nil {
		log.Printf("创建 ingress 失败: %v", err)
		return nil, err
	}
	return a.st.CreateIngress(r.Context(), u.ID, c.ID, ingressID, streamKey)
}

// ---- 频道管理（房主操作）----

// resolveTargetUser 解析 body 里的目标用户名：须存在且不能是房主自己。
func (a *API) resolveTargetUser(w http.ResponseWriter, r *http.Request, u *store.User) *store.User {
	var req struct {
		Username string `json:"username"`
	}
	if !decode(w, r, &req) {
		return nil
	}
	if req.Username == u.Username {
		writeErr(w, http.StatusBadRequest, "不能对自己操作")
		return nil
	}
	t, _, err := a.st.UserByName(r.Context(), req.Username)
	if err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return nil
	}
	return t
}

// evict 把用户从频道现场移除：LiveKit 侧踢全部设备 + 断开聊天 WS。
func (a *API) evict(r *http.Request, c *store.Channel, t *store.User) (int, error) {
	n, err := a.voiceProvider(r.Context()).RemoveParticipantsOf(r.Context(), c.Name, t.Username)
	if sp := a.stageProvider(r.Context()); sp != nil {
		if m, serr := sp.RemoveParticipantsOf(r.Context(), c.Name, t.Username); serr == nil {
			n += m
		} else if err == nil {
			err = serr
		}
	}
	if err != nil {
		log.Printf("内核踢出 %s 失败: %v", t.Username, err)
	}
	a.hub.CloseUserChannel(t.ID, c.ID)
	return n, err
}

func (a *API) kick(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return
	}
	n, err := a.evict(r, c, t)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "踢出失败（LiveKit 不可达）")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kicked": n})
}

func (a *API) ban(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return
	}
	if err := a.st.Ban(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 封禁立即生效：踢出现场（LiveKit 失败不阻塞，token/WS 入口已拦死）
	a.evict(r, c, t)
	writeJSON(w, http.StatusOK, map[string]string{"banned": t.Username})
}

func (a *API) unban(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return
	}
	if err := a.st.Unban(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setGag 禁言/解禁目标用户（模式同 ban：落库为权威，现场传播尽力）。
// 落库（channel_gags）保证离房/重进都生效——joinToken 与 /api/voice 入会都按它签发/拦截；
// 内核调用只负责让"当前在房"的设备立即失声，目标不在房（ErrNoParticipant）不算失败。
func (a *API) setGag(w http.ResponseWriter, r *http.Request, muted bool) {
	c := channelFrom(r)
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return
	}
	var err error
	if muted {
		err = a.st.Gag(r.Context(), c.ID, t.ID)
	} else {
		err = a.st.Ungag(r.Context(), c.ID, t.ID)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	kernels := []rtc.Provider{a.voiceProvider(r.Context())}
	// 舞台线是独立连接时同步（与语音同一内核时 combined 单连接已覆盖）
	if sp := a.stageProvider(r.Context()); sp != nil &&
		a.dynVal(r.Context(), "voice_provider") != a.dynVal(r.Context(), "stage_provider") {
		kernels = append(kernels, sp)
	}
	for _, p := range kernels {
		if err := p.MuteUserAudio(r.Context(), c.Name, t.Username, muted); err != nil &&
			!errors.Is(err, rtc.ErrNoParticipant) {
			log.Printf("内核(%s)禁言传播 %s 失败: %v", p.Name(), t.Username, err)
		}
	}
	key := "unmuted"
	if muted {
		key = "muted"
	}
	writeJSON(w, http.StatusOK, map[string]string{key: t.Username})
}

func (a *API) mute(w http.ResponseWriter, r *http.Request)   { a.setGag(w, r, true) }
func (a *API) unmute(w http.ResponseWriter, r *http.Request) { a.setGag(w, r, false) }

func (a *API) listBans(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	names, err := a.st.ListBans(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bans": names})
}

func (a *API) setInviteOnly(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := a.st.SetInviteOnly(r.Context(), c.ID, req.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"invite_only": req.Enabled})
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	names, err := a.st.ListMembers(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": names})
}

func (a *API) addMember(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return
	}
	if err := a.st.AddMember(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return
	}
	if err := a.st.RemoveMember(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 工具 ----

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
