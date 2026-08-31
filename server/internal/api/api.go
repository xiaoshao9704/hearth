// REST API：注册/登录/频道/LiveKit 令牌/频道管理。路由用 chi，鉴权用 Bearer token。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"hearth/server/internal/chat"
	"hearth/server/internal/config"
	"hearth/server/internal/lkingress"
	"hearth/server/internal/lkroom"
	"hearth/server/internal/lktoken"
	"hearth/server/internal/store"

	"crypto/rand"
	"encoding/hex"
	"log"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

type API struct {
	st    *store.Store
	cfg   config.Config
	ing   *lkingress.Client
	rooms *lkroom.Client
	hub   *chat.Hub

	// LiveKit 在房人数缓存（大厅频道列表用，避免每次列表都打 LiveKit）
	countsMu sync.Mutex
	counts   map[string]int
	countsAt time.Time
}

func New(st *store.Store, cfg config.Config, hub *chat.Hub) *API {
	return &API{
		st:    st,
		cfg:   cfg,
		ing:   lkingress.NewClient(cfg.LiveKitAPIURL, cfg.LiveKitKey, cfg.LiveKitSecret),
		rooms: lkroom.NewClient(cfg.LiveKitAPIURL, cfg.LiveKitKey, cfg.LiveKitSecret),
		hub:   hub,
	}
}

// Router 构建 chi 路由：CORS 全局挂载，认证/房主校验为分组中间件。
func (a *API) Router() *chi.Mux {
	r := chi.NewRouter()
	r.Use(a.cors)

	r.Post("/api/register", a.registerWithPolicy)
	r.Post("/api/login", a.login)
	r.Get("/api/invites/{code}", a.inviteInfo)

	// 需登录
	r.Group(func(r chi.Router) {
		r.Use(a.auth)
		r.Post("/api/logout", a.logout)
		r.Get("/api/me", a.me)
		r.Get("/api/channels", a.listChannels)
		r.Post("/api/channels", a.createChannel)
		r.Post("/api/token", a.livekitToken)
		r.Post("/api/ingress", a.getIngress)
		r.Post("/api/ingress/reset", a.resetIngress)

		// 账户设置
		r.Post("/api/account/username", a.updateUsername)
		r.Post("/api/account/password", a.updatePassword)
		r.Get("/api/account/devices", a.listMyDevices)
		r.Delete("/api/account/devices/{deviceID}", a.deleteMyDevice)

		// 频道管理（房主）：频道解析与房主校验收敛到子路由中间件
		r.Route("/api/channels/{channel}", func(r chi.Router) {
			r.Use(a.requireOwner)
			r.Post("/kick", a.kick)
			r.Post("/ban", a.ban)
			r.Post("/unban", a.unban)
			r.Get("/bans", a.listBans)
			r.Post("/invite-only", a.setInviteOnly)
			r.Get("/members", a.listMembers)
			r.Post("/members", a.addMember)
			r.Delete("/members", a.removeMember)
			r.Get("/participants", a.channelParticipants)
		})

		// 管理后台
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(a.requireAdmin)
			r.Get("/overview", a.adminOverview)
			r.Get("/policy", a.adminGetPolicy)
			r.Post("/policy", a.adminSetPolicy)
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
	counts, err := a.rooms.RoomCounts(ctx)
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

// channelFrom 取 requireOwner 中间件注入的频道。
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

// requireOwner 房主校验中间件：解析 {channel} 频道并确认当前用户是房主，频道注入 context。
func (a *API) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := a.st.ChannelByName(r.Context(), chi.URLParam(r, "channel"))
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "频道不存在")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "内部错误")
			return
		}
		if c.OwnerID != userFrom(r).ID {
			writeErr(w, http.StatusForbidden, "只有房主能操作")
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

// ---- LiveKit 令牌 ----

func (a *API) livekitToken(w http.ResponseWriter, r *http.Request) {
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
	// 设备标签 = UA 推断 + 前端持久设备 ID(缺省时随机,不建档)
	tag := deviceTag(r.UserAgent())
	dev := deviceIDRe.FindString(req.DeviceID)
	if dev != "" {
		if err := a.st.RecordDevice(r.Context(), u.ID, dev, tag); err != nil {
			log.Printf("记录设备失败: %v", err)
		}
		tag += "-" + dev
	} else {
		tag += "-" + randHex(2)
	}
	// 频道名即 LiveKit room 名，一一映射
	tok, err := lktoken.Sign(a.cfg.LiveKitKey, a.cfg.LiveKitSecret, c.Name, u.Username, tag)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok, "url": a.livekitURL(r)})
}

// livekitURL 返回给前端的 LiveKit WSS 地址：未配置时按请求推导同源 /lk 代理地址。
func (a *API) livekitURL(r *http.Request) string {
	if a.cfg.LiveKitURL != "" {
		return a.cfg.LiveKitURL
	}
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
	base := a.cfg.IngressPublicURL
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
	if a.cfg.IngressUpstreamURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "Ingress 未启用（未配置 INGRESS_UPSTREAM_URL）")
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
	if a.cfg.IngressUpstreamURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "Ingress 未启用（未配置 INGRESS_UPSTREAM_URL）")
		return
	}
	c := a.resolveIngressChannel(w, r)
	if c == nil {
		return
	}
	if rec, err := a.st.IngressByUserChannel(r.Context(), u.ID, c.ID); err == nil {
		if derr := a.ing.Delete(r.Context(), rec.IngressID); derr != nil {
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

// createIngress 调 LiveKit 创建 ingress 并落库。
func (a *API) createIngress(r *http.Request, u *store.User, c *store.Channel) (*store.Ingress, error) {
	ingressID, streamKey, err := a.ing.Create(r.Context(), c.Name, u.Username)
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
	n, err := a.rooms.RemoveParticipantsOf(r.Context(), c.Name, t.Username)
	if err != nil {
		log.Printf("LiveKit 踢出 %s 失败: %v", t.Username, err)
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
