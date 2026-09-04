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
	"hearth/server/internal/perm"
	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/lite"
	"hearth/server/internal/rtc/livekitembed"
	"hearth/server/internal/rtc/livekitrtc"
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
	// whipResolver 推流面的归属反查闭包：判定已在 serveWHIP 的 admitIngest
	// 做完并挂到请求 ctx（ingestCtxKey），这里原样取回四元组；无判定结果按未知令牌处理。
	whipResolver livekitrtc.ResolveFunc

	// 在房人数缓存（大厅频道列表用，避免每次列表都打内核）
	countsMu sync.Mutex
	counts   map[string]int
	countsAt time.Time

	// announcer 进程内唯一的宣告探测器（STUN/显式公网 IP + 端口映射 → 宣告候选）：
	// lkembed 的 ExternalIPs 回调从它的快照取外部地址（见 lkembed.go）
	announcer *lite.Announcer

	// mapped 端口映射结果查询，透传给进程内 ICE-Lite 内核做宣告（无映射来源时为 nil）
	mapped lite.MappedFunc

	// 进程内 LiveKit（内建实例 lkembed，见 lkembed.go）：实例对象常在，服务端只在
	// stage_provider 选中它时才跑。lkembedWHIP 是同一实例的推流面（反代到 LiveKit
	// 自带的 WHIP 入口），与 Stage 面共用 embedCfg
	lkembed     *livekitrtc.Provider
	lkembedWHIP *livekitrtc.WHIP
	embedMu     sync.Mutex
	embedSrv    *livekitembed.Server
}

func New(st *store.Store, cfg config.Config, hub *chat.Hub, mapped lite.MappedFunc) *API {
	a := &API{st: st, cfg: cfg, hub: hub, mapped: mapped,
		providers: map[string]*ProviderInstance{}}
	// 内建实例只有 lkembed（进程内 LiveKit，语音/舞台/推流三面齐全，见 providers.go）
	a.announcer = lite.NewAnnouncer(
		func(ctx context.Context) string { return a.dynVal(ctx, "lkembed_public_ip") },
		func(ctx context.Context) string { return a.dynVal(ctx, "lkembed_stun_servers") },
		a.mapped)
	a.whipResolver = func(ctx context.Context, _ string) (string, string, rtc.Meta, error) {
		adm, ok := ctx.Value(ingestCtxKey{}).(ingestAdmission)
		if !ok {
			return "", "", rtc.Meta{}, errors.New("未知推流令牌")
		}
		return adm.Room, adm.Identity, adm.Meta, nil
	}
	a.lkembed = livekitrtc.New(a.embedCfg)
	a.lkembedWHIP = livekitrtc.NewWHIP(a.embedCfg, a.whipResolver, a.stageKernelRunning)
	a.kernelKeys = livekitembed.ConfigKeys()
	// 注册表先种内建实例：启动期迁移或 ListProviders 失败（保留旧表）时，
	// 各选择器的默认/回落路径仍有内建对象可用
	for _, inst := range a.builtinInstances() {
		a.providers[inst.Alias] = inst
		a.providerOrder = append(a.providerOrder, inst.Alias)
	}
	ctx := context.Background()
	a.runMigrations(ctx) // 版本游标迁移，末尾完成首次 reloadProviders
	a.warnLegacyConfig()
	return a
}

// Router 构建 chi 路由：CORS 全局挂载，认证/房主校验为分组中间件。
func (a *API) Router() *chi.Mux {
	r := chi.NewRouter()
	r.Use(a.cors)

	r.Post("/api/register", a.registerWithPolicy)
	r.Post("/api/login", a.login)
	r.Get("/api/invites/{code}", a.inviteInfo)
	r.Get("/api/site", a.site)

	// 健康检查：只表示进程活着（宣告探测的刷新由进程内周期任务触发，不挂在这里）
	r.Get("/healthz", a.healthz)

	// 需登录
	r.Group(func(r chi.Router) {
		r.Use(a.auth)
		r.Post("/api/logout", a.logout)
		r.Get("/api/me", a.me)
		r.Get("/api/channels", a.listChannels)
		r.With(a.requireRole(store.RolePower)).Post("/api/channels", a.createChannel)
		r.Post("/api/token", a.joinToken)

		// 注册邀请：power+ 可发（产出 user）；admin+ 可指定产出档、可见/可撤全部邀请
		r.Route("/api/invites", func(r chi.Router) {
			r.Use(a.requireRole(store.RolePower))
			r.Post("/", a.createInvite)
			r.Get("/", a.listInvites)
			r.Delete("/{id}", a.revokeInvite)
		})

		// 账户设置
		r.Post("/api/account/username", a.updateUsername)
		r.Post("/api/account/password", a.updatePassword)
		r.Get("/api/account/devices", a.listMyDevices)
		r.Delete("/api/account/devices/{deviceID}", a.deleteMyDevice)

		// 推流令牌（每用户一把，房间在 WHIP URL 里）
		r.Get("/api/ingest/token", a.ingestTokenGet)
		r.Post("/api/ingest/token/reset", a.ingestTokenReset)
		r.Put("/api/ingest/token", a.ingestTokenTag)

		// 频道管理：频道解析与权限校验收敛到子路由中间件
		// （现场管制与白名单 = 频道管理员及以上，归属变更与邀请制开关 = 仅频道主）
		r.Route("/api/channels/{channel}", func(r chi.Router) {
			// kick 单独放行到登录层：踢"自己"的设备（远程下线忘关的 OBS/其他设备）
			// 不需要管理权限，踢别人在 handler 内要求频道管理员及以上
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
				r.Get("/members", a.listMembers)
				r.Post("/members", a.addMember)
				r.Delete("/members", a.removeMember)
				r.Get("/participants", a.channelParticipants)
			})
			r.Group(func(r chi.Router) {
				r.Use(a.requireOwner)
				r.Post("/invite-only", a.setInviteOnly)
				r.Post("/transfer", a.transferChannel)
				r.Get("/moderators", a.listModerators)
				r.Post("/moderators", a.addModerator)
				r.Delete("/moderators", a.removeModerator)
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
			r.Post("/users/{id}/role", a.adminSetUserRole)
			r.Post("/users/{id}/disable", a.adminSetUserDisabled(true))
			r.Post("/users/{id}/enable", a.adminSetUserDisabled(false))
			r.Delete("/users/{id}", a.adminDeleteUser)
			r.Delete("/channels/{id}", a.adminDeleteChannel)
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

// requireRole 系统角色门槛中间件：低于该档一律 403（挂在 auth 之后）。
func (a *API) requireRole(role store.Role) func(http.Handler) http.Handler {
	label := map[store.Role]string{
		store.RolePower: "需要高级用户权限",
		store.RoleAdmin: "需要管理员权限",
	}[role]
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !perm.SysAtLeast(userFrom(r), role) {
				writeErr(w, http.StatusForbidden, label)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireOwner 频道主校验中间件：解析 {channel} 频道并确认当前用户是频道主
// （含系统 admin+ 的隐含频道主），频道注入 context。
func (a *API) requireOwner(next http.Handler) http.Handler {
	return a.requireChannelRole(store.ChannelRoleOwner, "只有频道主能操作")(next)
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

// requireModerator 频道管理员及以上校验中间件（含频道主与系统 admin+），频道注入 context。
func (a *API) requireModerator(next http.Handler) http.Handler {
	return a.requireChannelRole(store.ChannelRoleModerator, "需要频道管理员权限")(next)
}

// requireChannelRole 频道角色门槛中间件：解析 {channel} 频道并校验当前用户的频道角色。
func (a *API) requireChannelRole(need store.ChannelRole, msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := a.channelOf(w, r)
			if c == nil {
				return
			}
			cr, err := perm.ChannelRole(r.Context(), a.st, c, userFrom(r))
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "内部错误")
				return
			}
			if !perm.ChannelAtLeast(cr, need) {
				writeErr(w, http.StatusForbidden, msg)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxChannel, c)))
		})
	}
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
	roles, err := a.st.ChannelRolesOf(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 系统 admin+ 在任何频道隐含频道主（与 perm.ChannelRole 同口径，批量填充免逐频道查询）
	implicit := ""
	if perm.SysAtLeast(u, store.RoleAdmin) {
		implicit = string(store.ChannelRoleOwner)
	}
	counts, _ := a.roomCounts(r.Context()) // LiveKit 不可达时在线数保持 0
	for i := range chs {
		chs[i].MyRole = implicit
		if chs[i].MyRole == "" {
			chs[i].MyRole = string(roles[chs[i].ID])
		}
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
	// WHIP 路径保留字：/w/sessions/{rid} 会话收尾与 /w/revoke/{token} 远端撤销
	if req.Name == "sessions" || req.Name == "revoke" {
		writeErr(w, http.StatusBadRequest, "该频道名为保留字")
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
	c.MyRole = string(store.ChannelRoleOwner) // 创建者即频道主
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
	// 参与者元数据组一次两线共用：身份判定走 identity 里的 user_id，
	// 用户名等展示信息随元数据下发（前端不解析 identity）
	meta := rtc.Meta{UID: adm.UID, Username: adm.Username, Tag: a.deviceTagFor(r, req.DeviceID, u.ID)}
	// 频道名即内核房间名，一一映射。语音线必发；舞台线可选；
	// 两线同一实例时标记 combined，前端用一条连接承担两种角色（即旧单线形态）。
	voiceAlias, voiceP := a.voiceInstance(r.Context())
	stageAlias, stageP := a.stageInstance(r.Context())
	vc, err := voiceP.JoinCredentials(r.Context(), c.Name, meta, adm.CanPublish)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	resp := map[string]any{"voice": a.fillCred(r, vc, voiceAlias)}
	combined := stageP != nil && voiceAlias == stageAlias
	resp["combined"] = combined
	if stageP != nil && !combined {
		sc, serr := stageP.JoinCredentials(r.Context(), c.Name, meta, adm.CanPublish)
		if serr != nil {
			log.Printf("舞台线签发失败（语音照常）: %v", serr)
		} else {
			resp["stage"] = a.fillCred(r, sc, stageAlias)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// fillCred 补全内核未声明的连接信息：走同源 /providers/{alias} 信令代理。
func (a *API) fillCred(r *http.Request, c rtc.Credentials, alias string) map[string]string {
	u := c.URL
	if u == "" {
		u = a.signalURL(r, alias)
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

// ---- 频道管理（房主操作）----

// resolveTargetUser 解析 body 里的目标 user_id：须存在且不能是自己（封禁/禁言对自己无意义；
// 踢出有独立 handler，支持踢自己的设备）。目标一律用 user_id——用户名可改、改后旧名即释放，
// 拿它做操作目标会在改名/重注册后打到别人身上；前端从参与者元数据的 uid 取。
func (a *API) resolveTargetUser(w http.ResponseWriter, r *http.Request, u *store.User) *store.User {
	var req struct {
		UserID int64 `json:"user_id"`
	}
	if !decode(w, r, &req) {
		return nil
	}
	if req.UserID == u.ID {
		writeErr(w, http.StatusBadRequest, "不能对自己操作")
		return nil
	}
	t, err := a.st.UserByID(r.Context(), req.UserID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return nil
	}
	return t
}

// evict 把用户从频道现场移除：identity 为空踢全部设备并断聊天 WS；
// 非空只踢该设备（归属约束由内核侧按 user_id 保底），聊天不动。
func (a *API) evict(r *http.Request, c *store.Channel, t *store.User, identity string) (int, error) {
	_, vp := a.voiceInstance(r.Context())
	n, err := vp.RemoveParticipantsOf(r.Context(), c.Name, t.ID, identity)
	if _, sp := a.stageInstance(r.Context()); sp != nil {
		if m, serr := sp.RemoveParticipantsOf(r.Context(), c.Name, t.ID, identity); serr == nil {
			n += m
		} else if err == nil {
			err = serr
		}
	}
	if err != nil {
		log.Printf("内核踢出 uid=%d（设备 %q）失败: %v", t.ID, identity, err)
	}
	if identity == "" {
		a.hub.CloseUserChannel(t.ID, c.ID)
	}
	return n, err
}

// kick 踢出目标用户：identity 为空踢全部设备，非空只踢该设备（须归属该 user_id）。
// 踢自己（含自己的单个设备）只需登录；踢别人要求频道管理员及以上。
func (a *API) kick(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	var req struct {
		UserID   int64  `json:"user_id"`
		Identity string `json:"identity"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.UserID != u.ID {
		cr, err := perm.ChannelRole(r.Context(), a.st, c, u)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "内部错误")
			return
		}
		if !perm.ChannelAtLeast(cr, store.ChannelRoleModerator) {
			writeErr(w, http.StatusForbidden, "需要频道管理员权限")
			return
		}
	}
	if req.Identity != "" && !rtc.MatchesUser(req.Identity, req.UserID) {
		writeErr(w, http.StatusBadRequest, "设备不属于该用户")
		return
	}
	t, err := a.st.UserByID(r.Context(), req.UserID)
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
// 落库（channel_gags）保证离房/重进都生效——joinToken 与推流入场判定都按它签发/拦截；
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
		if err := p.MuteUserAudio(r.Context(), c.Name, t.ID, muted); err != nil &&
			!errors.Is(err, rtc.ErrNoParticipant) {
			log.Printf("内核(%s)禁言传播 uid=%d 失败: %v", p.Name(), t.ID, err)
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
	bans, err := a.st.ListBans(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bans": bans})
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
	members, err := a.st.ListMembers(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

// addMember 把用户加入邀请制白名单。**这里按用户名收是有意的**：目标是「还没进过房的人」，
// 房主手输名字，界面上没有 uid 可选——与登录同属「名字 → 用户」的一次查找，
// 查到后立即换成 user_id 落库，用户名不进任何判定。
// 移出白名单走 removeMember（从名单里点，带 uid）。
func (a *API) addMember(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	var req struct {
		Username string `json:"username"`
	}
	if !decode(w, r, &req) {
		return
	}
	t, _, err := a.st.UserByName(r.Context(), strings.TrimSpace(req.Username))
	if err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if t.ID == u.ID {
		writeErr(w, http.StatusBadRequest, "不用把自己加进白名单（房主天然可进）")
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

// resolveRoleTarget 解析 {user_id} 目标并校验其可持有频道角色：须存在、不是自己、
// 非访客（访客不持有 owner/moderator，授予即拒绝）。转让与管理员授予共用。
func (a *API) resolveRoleTarget(w http.ResponseWriter, r *http.Request) *store.User {
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return nil
	}
	if t.Role == store.RoleGuest {
		writeErr(w, http.StatusBadRequest, "访客不能持有频道角色")
		return nil
	}
	return t
}

// transferChannel 转让频道（owner）：目标须 user 及以上；旧主自动降为 moderator。
func (a *API) transferChannel(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveRoleTarget(w, r)
	if t == nil {
		return
	}
	if err := a.st.TransferChannel(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"owner": t.Username})
}

func (a *API) listModerators(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	mods, err := a.st.ListModerators(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"moderators": mods})
}

// addModerator 授予频道管理员（owner）；目标是房主时拒绝（房主无需再授）。
func (a *API) addModerator(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveRoleTarget(w, r)
	if t == nil {
		return
	}
	if cr, err := a.st.ChannelRoleOf(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	} else if cr == store.ChannelRoleOwner {
		writeErr(w, http.StatusBadRequest, "对方是频道主，无需授予")
		return
	}
	if err := a.st.SetChannelRole(r.Context(), c.ID, t.ID, store.ChannelRoleModerator); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeModerator 收回频道管理员（owner）：降为 member（白名单行保留）。
func (a *API) removeModerator(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	t := a.resolveTargetUser(w, r, userFrom(r))
	if t == nil {
		return
	}
	if cr, err := a.st.ChannelRoleOf(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	} else if cr != store.ChannelRoleModerator {
		writeErr(w, http.StatusBadRequest, "对方不是频道管理员")
		return
	}
	if err := a.st.SetChannelRole(r.Context(), c.ID, t.ID, store.ChannelRoleMember); err != nil {
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
