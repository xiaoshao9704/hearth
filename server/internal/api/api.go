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
	"hearth/server/internal/rtc/bellows"
	"hearth/server/internal/rtc/ember"
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

	// 内核实例注册表：接入层只依赖 rtc 接口，按选择器（实例 alias）取实例对象（见 providers.go）
	reloadMu      sync.Mutex // 串行化「写 DB → 重建注册表」（mutateProviders/reloadProviders）
	providersMu   sync.RWMutex
	providers     map[string]*ProviderInstance
	providerOrder []string        // listInstances 顺序：内建 → env 锁定 → DB
	kernelKeys    []rtc.ConfigKey // 内建实例的全局配置键汇总
	ember         *ember.Provider // /providers/ember/voice 信令端点直连（进程内实现）
	// ingressResolver 内建 bellows 的归属反查闭包（ingressOwner → ErrUnknownKey 映射）
	ingressResolver bellows.ResolveFunc

	// 在房人数缓存（大厅频道列表用，避免每次列表都打内核）
	countsMu sync.Mutex
	counts   map[string]int
	countsAt time.Time

	// ember 线一次性入场票表（见 admission.go）
	ticketMu sync.Mutex
	tickets  map[string]voiceTicket
}

func New(st *store.Store, cfg config.Config, hub *chat.Hub) *API {
	a := &API{st: st, cfg: cfg, hub: hub, tickets: map[string]voiceTicket{}, providers: map[string]*ProviderInstance{}}
	// 内建实例：ember 是进程内纯音频语音内核；bellows 是进程内 WHIP 直通推流网关
	//（OBS HEVC/AV1 的接入路径），其余形态由 env/DB 注册成实例（见 providers.go）
	a.ember = ember.New(a.dynVal)
	a.ingressResolver = func(ctx context.Context, streamKey string) (string, string, error) {
		// 查不到 → ErrUnknownKey（404）；瞬时 DB 故障原样透传（503），不能误报「密钥已重置」。
		// 入场判定不在这里做：进程内形态由 WHIP 拦截（canPublishByStreamKey）先判过了
		c, u, _, err := a.ingressOwner(ctx, streamKey)
		if errors.Is(err, store.ErrNotFound) {
			return "", "", bellows.ErrUnknownKey
		}
		if err != nil {
			return "", "", err
		}
		return c.Name, u.Username, nil
	}
	a.kernelKeys = append(ember.ConfigKeys(), bellows.ConfigKeys()...)
	// 注册表先种内建实例：启动期迁移或 ListProviders 失败（保留旧表）时，
	// voiceInstance/ingestInstance 的回落路径仍有 ember/bellows 对象可用
	for _, inst := range a.builtinInstances() {
		a.providers[inst.Alias] = inst
		a.providerOrder = append(a.providerOrder, inst.Alias)
	}
	ctx := context.Background()
	a.runMigrations(ctx) // 版本游标迁移，末尾完成首次 reloadProviders
	a.warnLegacyConfig(ctx)
	return a
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
			// kick 单独放行到登录层：踢"自己"的设备（远程下线忘关的 OBS/其他设备）
			// 不需要管理权限，踢别人在 handler 内要求房主/管理员
			r.Group(func(r chi.Router) {
				r.Use(a.requireChannel)
				r.Post("/kick", a.kick)
			})
			r.Group(func(r chi.Router) {
				r.Use(a.requireModerator)
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
			r.Get("/providers", a.adminListProviders)
			r.Post("/providers", a.adminCreateProvider)
			r.Put("/providers/{alias}", a.adminUpdateProvider)
			r.Delete("/providers/{alias}", a.adminDeleteProvider)
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
	_, vp := a.voiceInstance(ctx)
	counts, err := vp.RoomCounts(ctx)
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

// requireChannel 仅解析 {channel} 注入 context（权限由 handler 自行判定）。
func (a *API) requireChannel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := a.channelOf(w, r)
		if c == nil {
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
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
	// 入场判定：封禁/邀请制决定能否进入，禁言随进房凭证生效（无发布权限）
	adm, ok, reason, err := a.admitUser(r.Context(), c, u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return
	}
	tag := a.deviceTagFor(r, req.DeviceID, u.ID)
	// 频道名即内核房间名，一一映射。语音线必发；舞台线可选；
	// 两线同一实例时标记 combined，前端用一条连接承担两种角色（即旧单线形态）。
	voiceAlias, voiceP := a.voiceInstance(r.Context())
	stageAlias, stageP := a.stageInstance(r.Context())
	vc, err := voiceP.JoinCredentials(r.Context(), c.Name, adm.Identity, tag, adm.CanPublish)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	// ember 信令走一次性入场票：判定结果在此定格，/providers/ember/voice 凭票直接入会（见 admission.go）
	ticket := ""
	if vc.Engine == "ember" {
		ticket = a.issueVoiceTicket(voiceTicket{
			room:     c.Name,
			identity: adm.Identity + "-" + tag,
			name:     adm.Identity,
			userID:   u.ID,
			muted:    !adm.CanPublish,
		})
	}
	resp := map[string]any{"voice": a.fillCred(r, vc, c.Name, ticket, voiceAlias)}
	combined := stageP != nil && voiceAlias == stageAlias
	resp["combined"] = combined
	if stageP != nil && !combined {
		sc, serr := stageP.JoinCredentials(r.Context(), c.Name, adm.Identity, tag, adm.CanPublish)
		if serr != nil {
			log.Printf("舞台线签发失败（语音照常）: %v", serr)
		} else {
			resp["stage"] = a.fillCred(r, sc, c.Name, "", stageAlias)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// fillCred 补全内核未声明的连接信息：ember 语音走同源 /providers/{alias}/voice 信令并
// 透传会话 token（附一次性入场票），livekit 等走同源 /providers/{alias} 信令代理。
func (a *API) fillCred(r *http.Request, c rtc.Credentials, channel, ticket, alias string) map[string]string {
	u := c.URL
	if u == "" {
		if c.Engine == "ember" {
			scheme := "ws"
			if requestScheme(r) == "https" {
				scheme = "wss"
			}
			q := neturl.Values{"channel": {channel}}
			if ticket != "" {
				q.Set("ticket", ticket)
			}
			u = (&neturl.URL{Scheme: scheme, Host: r.Host, Path: "/providers/" + alias + "/voice", RawQuery: q.Encode()}).String()
		} else {
			u = a.signalURL(r, alias)
		}
	}
	token := c.Token
	if token == "" {
		token = BearerToken(r)
	}
	return map[string]string{"engine": c.Engine, "url": u, "token": token}
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

// signalURL 按请求推导同源信令代理地址（/providers/{alias} 路由）。
func (a *API) signalURL(r *http.Request, alias string) string {
	scheme := "ws"
	if requestScheme(r) == "https" {
		scheme = "wss"
	}
	return (&neturl.URL{Scheme: scheme, Host: r.Host, Path: "/providers/" + alias}).String()
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

// ingressURL 按请求推导同源 /providers/{alias}/w/ 推流地址。
func (a *API) ingressURL(r *http.Request, alias, streamKey string) string {
	return (&neturl.URL{Scheme: requestScheme(r), Host: r.Host, Path: "/providers/" + alias + "/w/" + streamKey}).String()
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

// getIngress 返回当前用户在该频道的推流地址；没有则调推流实例创建并落库。
func (a *API) getIngress(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	ingestAlias, ip, fellBack := a.ingestInstance(r.Context())
	if !ip.Enabled(r.Context()) {
		writeErr(w, http.StatusServiceUnavailable, "推流入口未启用（所选内核缺少必需配置）")
		return
	}
	c := a.resolveIngressChannel(w, r)
	if c == nil {
		return
	}
	rec, err := a.st.IngressByUserChannel(r.Context(), u.ID, c.ID)
	if err == nil && rec.Provider != ingestAlias {
		if fellBack {
			// 回落 = 选择器配置无效的临时态，不做归属自愈（不删端点不重建）：
			// 按记录原归属实例返回地址；归属实例已注销则配置本身不可用
			if orig := a.instance(rec.Provider); orig == nil || orig.Ingest == nil {
				writeErr(w, http.StatusServiceUnavailable, "推流入口配置无效")
				return
			}
			log.Printf("推流选择器 %q 无效已回落，保留既有记录（归属 %s）",
				a.dynVal(r.Context(), "ingest_provider"), rec.Provider)
			writeJSON(w, http.StatusOK, ingressResp{URL: a.ingressURL(r, rec.Provider, rec.StreamKey), StreamKey: rec.StreamKey})
			return
		}
		// 换了推流入口实例：旧记录对新入口无效（key 不被上游认识），
		// 清掉旧端点与记录后按当前实例重建，而不是把死地址原样返回
		a.deleteOldEndpoint(r, rec)
		a.st.DeleteIngress(r.Context(), u.ID, c.ID)
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) {
		rec, err = a.createIngress(r, u, c)
	}
	if errors.Is(err, errIngestFallback) {
		// 与 resetIngress 前置拦截同口径：回落 = 选择器配置无效，不创建端点
		writeErr(w, http.StatusServiceUnavailable, "推流入口配置无效")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "获取推流地址失败")
		return
	}
	writeJSON(w, http.StatusOK, ingressResp{URL: a.ingressURL(r, ingestAlias, rec.StreamKey), StreamKey: rec.StreamKey})
}

// deleteOldEndpoint 尽力删除记录归属内核侧的旧端点：删除必须发给创建它的内核
// （比如 livekit 期间建的 ingress 切到 bellows 后仍要在 LiveKit 侧删掉，否则旧 key 一直有效）。
// 归属内核是远端网关（WHIPGrantIssuer）时再通知其掐断该 key 的远端会话——
// 通行证是短时效入场券不管会话生命周期，重置密钥要能把正在推的旧会话掐掉。
func (a *API) deleteOldEndpoint(r *http.Request, rec *store.Ingress) {
	inst := a.instance(rec.Provider)
	if inst == nil || inst.Ingest == nil {
		// 归属实例已注销：内核侧删除无从下手，只清库记录（旧 key 随反查不到归属自然失效）
		log.Printf("旧 ingress %s 的归属实例 %s 已不存在，跳过内核侧删除", rec.IngressID, rec.Provider)
		return
	}
	if derr := inst.Ingest.DeleteEndpoint(r.Context(), rec.IngressID); derr != nil {
		// 内核侧删不掉不阻塞重建（记录可能已是残缺的）
		log.Printf("删除旧 ingress %s（内核 %s）失败: %v", rec.IngressID, rec.Provider, derr)
	}
	if gi, ok := inst.Ingest.(rtc.WHIPGrantIssuer); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if rerr := gi.RevokeRemoteSessions(ctx, rec.StreamKey); rerr != nil {
			log.Printf("撤销远端会话（key 内核 %s）失败: %v", rec.Provider, rerr)
		}
	}
}

// resetIngress 删除旧 ingress（内核侧 + 库记录）后重建，旧地址随之失效。
func (a *API) resetIngress(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	ingestAlias, ip, fellBack := a.ingestInstance(r.Context())
	if fellBack {
		// 回落 = 选择器配置无效：重建会误用回落实例，拒绝到配置修正为止
		writeErr(w, http.StatusServiceUnavailable, "推流入口配置无效（所选实例无推流能力）")
		return
	}
	if !ip.Enabled(r.Context()) {
		writeErr(w, http.StatusServiceUnavailable, "推流入口未启用（所选内核缺少必需配置）")
		return
	}
	c := a.resolveIngressChannel(w, r)
	if c == nil {
		return
	}
	if rec, err := a.st.IngressByUserChannel(r.Context(), u.ID, c.ID); err == nil {
		a.deleteOldEndpoint(r, rec)
		a.st.DeleteIngress(r.Context(), u.ID, c.ID)
	}
	rec, err := a.createIngress(r, u, c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "重置推流地址失败")
		return
	}
	writeJSON(w, http.StatusOK, ingressResp{URL: a.ingressURL(r, ingestAlias, rec.StreamKey), StreamKey: rec.StreamKey})
}

// errIngestFallback 选择器回落状态下拒绝创建端点（回落实例不是管理员的选择）。
var errIngestFallback = errors.New("推流入口配置无效")

// createIngress 调推流实例创建端点并落库（记录带上实例 alias，删除/失效判断按归属方路由）。
func (a *API) createIngress(r *http.Request, u *store.User, c *store.Channel) (*store.Ingress, error) {
	alias, ip, fellBack := a.ingestInstance(r.Context())
	if fellBack {
		return nil, errIngestFallback
	}
	ingressID, streamKey, err := ip.CreateEndpoint(r.Context(), c.Name, u.Username)
	if err != nil {
		log.Printf("创建 ingress 失败: %v", err)
		return nil, err
	}
	return a.st.CreateIngress(r.Context(), u.ID, c.ID, ingressID, streamKey, alias)
}

// ---- 频道管理（房主操作）----

// resolveTargetUser 解析 body 里的目标用户名：须存在且不能是自己（封禁/禁言对自己无意义；
// 踢出有独立 handler，支持踢自己的设备）。
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

// evict 把用户从频道现场移除：identity 为空踢全部设备并断聊天 WS；
// 非空只踢该设备（RemoveParticipantsOf 的 MatchesUser 对完整 identity 即精确匹配），聊天不动。
func (a *API) evict(r *http.Request, c *store.Channel, t *store.User, identity string) (int, error) {
	target := t.Username
	if identity != "" {
		target = identity
	}
	_, vp := a.voiceInstance(r.Context())
	n, err := vp.RemoveParticipantsOf(r.Context(), c.Name, target)
	if _, sp := a.stageInstance(r.Context()); sp != nil {
		if m, serr := sp.RemoveParticipantsOf(r.Context(), c.Name, target); serr == nil {
			n += m
		} else if err == nil {
			err = serr
		}
	}
	if err != nil {
		log.Printf("内核踢出 %s 失败: %v", target, err)
	}
	if identity == "" {
		a.hub.CloseUserChannel(t.ID, c.ID)
	}
	return n, err
}

// kick 踢出目标用户：identity 为空踢全部设备，非空只踢该设备（须归属 username）。
// 踢自己（含自己的单个设备）只需登录；踢别人要求房主/管理员。
func (a *API) kick(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	var req struct {
		Username string `json:"username"`
		Identity string `json:"identity"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Username != u.Username && c.OwnerID != u.ID && !u.IsAdmin {
		writeErr(w, http.StatusForbidden, "只有房主或管理员能踢出他人")
		return
	}
	if req.Identity != "" && !rtc.MatchesUser(req.Identity, req.Username) {
		writeErr(w, http.StatusBadRequest, "设备不属于该用户")
		return
	}
	t, _, err := a.st.UserByName(r.Context(), req.Username)
	if err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	n, err := a.evict(r, c, t, req.Identity)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "踢出失败（内核不可达）")
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
	a.evict(r, c, t, "")
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
// 落库（channel_gags）保证离房/重进都生效——joinToken 与 ember 信令入会都按它签发/拦截；
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
	vAlias, vp := a.voiceInstance(r.Context())
	kernels := []rtc.Provider{vp}
	// 舞台线是独立连接时同步（两线同一实例时 combined 单连接已覆盖）
	if sAlias, sp := a.stageInstance(r.Context()); sp != nil && vAlias != sAlias {
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
