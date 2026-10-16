// REST API：注册/登录/频道/进房令牌/频道管理。路由用 chi，鉴权用 Bearer token。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"hearth/server/internal/config"
	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/lite"
	"hearth/server/internal/rtc/livekitembed"
	"hearth/server/internal/rtc/livekitrtc"
	"hearth/server/internal/store"
	"hearth/server/internal/tlscert"

	"github.com/go-chi/chi/v5"
)

type API struct {
	st      *store.Store
	cfg     config.Config
	version string // 构建版本号（main.version 注入，本地为 dev），随 X-Hearth-Version 响应头下发

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

	// 前端诊断只用于连接故障排查；按登录用户限频，避免浏览器异常循环刷满日志。
	clientLogMu    sync.Mutex
	clientLogRates map[int64]clientLogRate

	// 通行密钥：进行中的握手、按 (RP ID, origin) 缓存的 WebAuthn 实例、登录限频（见 passkey.go）
	passkey passkeyState

	// 桌面端浏览器跳转登录：已批准待换取的一次性码与换取限频（见 deviceauth.go）
	deviceAuth deviceAuthState

	// 离线推送（见 push.go）：VAPID 密钥的生成串行化；pushHTTP 非 nil 时替换投递用的
	// HTTP 客户端（测试注入假推送网关）
	webpushMu sync.Mutex
	pushHTTP  *http.Client

	// tlsStore HTTPS 证书（自签/外部文件/上传三来源，见 tls.go 与 internal/tlscert）；
	// portmapStatus 端口映射快照，由 cmd/server 接进来给 TLS 状态接口回显（nil = 未接）
	tlsStore      *tlscert.Store
	portmapStatus func() portmap.Status

	// announcer 进程内唯一的宣告探测器（STUN/显式公网 IP + 端口映射 → 宣告候选）：
	// lkembed 的 ExternalIPs 回调从它的快照取外部地址（见 lkembed.go）
	announcer *lite.Announcer

	// mapped 端口映射结果查询，透传给进程内 ICE-Lite 内核做宣告（无映射来源时为 nil）
	mapped lite.MappedFunc

	// lkembed（见 lkembed.go）：实例对象常在，服务端只在
	// stage_provider 选中它时才跑。lkembedWHIP 是同一实例的推流面（反代到 LiveKit
	// 自带的 WHIP 入口），与 Stage 面共用 embedCfg
	lkembed     *livekitrtc.Provider
	lkembedWHIP *livekitrtc.WHIP
	embedMu     sync.Mutex
	embedSrv    *livekitembed.Server
}

func New(st *store.Store, cfg config.Config, mapped lite.MappedFunc, version string) *API {
	a := &API{st: st, cfg: cfg, mapped: mapped, version: version,
		providers: map[string]*ProviderInstance{}}
	// 内建实例只有 lkembed（语音/舞台/推流三面齐全，见 providers.go）
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
	a.tlsStore = tlscert.New(filepath.Join(cfg.DataDir, "tls"), a.tlsSettings)
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
	r.Use(a.versionHeader)

	r.Post("/api/register", a.registerWithPolicy)
	r.Post("/api/login", a.login)
	r.Get("/api/invites/{code}", a.inviteInfo)
	r.Post("/api/invites/{code}/guest", a.guestEntry)
	r.Get("/api/site", a.site)
	// 通行密钥登录：可发现凭证，未鉴权（还不知道是谁），按来源 IP 限频（见 passkey.go）
	r.Post("/api/auth/passkey/login/begin", a.passkeyLoginBegin)
	r.Post("/api/auth/passkey/login/finish", a.passkeyLoginFinish)
	// 桌面端浏览器跳转登录的换取端：拿的就是会话，自然无鉴权，按来源 IP 限频（见 deviceauth.go）
	r.Post("/api/auth/device/exchange", a.deviceExchange)

	// 根证书下载与安装说明页：无鉴权——要装根证书的设备还没信任本站，也可能还在明文侧
	r.Get("/ca.crt", a.caCert)
	r.Get("/ca", a.caPage)

	// 健康检查：只表示进程活着（宣告探测的刷新由进程内周期任务触发，不挂在这里）
	r.Get("/healthz", a.healthz)
	// 延迟标尺的时钟对齐（见 docs/plan-latency.md）：与 healthz 同档，匿名可达、无副作用
	r.Get("/api/time", a.serverTime)

	// 需登录
	r.Group(func(r chi.Router) {
		r.Use(a.auth)
		r.Use(a.touchSession) // 节流刷新会话最近活跃时间（会话列表用，见 account.go）
		r.Post("/api/logout", a.logout)
		r.Get("/api/me", a.me)
		// 桌面端浏览器跳转登录的批准端：在浏览器里点了「允许」才调（见 deviceauth.go）
		r.Post("/api/auth/device/approve", a.deviceApprove)
		r.Post("/api/client-log", a.clientLog)
		r.Get("/api/channels", a.listChannels)
		// 离线推送订阅（见 push.go）：公钥、订阅、退订
		r.Get("/api/push/vapid", a.pushVAPID)
		r.Post("/api/push/subscribe", a.pushSubscribe)
		r.Delete("/api/push/subscribe", a.pushUnsubscribe)
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
		r.Post("/api/account/claim", a.claimGuest)
		r.Post("/api/account/password", a.updatePassword)
		r.Get("/api/account/devices", a.listMyDevices)
		r.Delete("/api/account/devices/{deviceID}", a.deleteMyDevice)
		r.Get("/api/account/sessions", a.listMySessions)
		r.Delete("/api/account/sessions/{id}", a.deleteMySession)

		// 通行密钥管理：访客不参与（账号会过期、又绑浏览器，挂长期凭证没有意义）
		r.Route("/api/account/passkeys", func(r chi.Router) {
			r.Use(a.requirePasskeyAccount)
			r.Get("/", a.listPasskeys)
			r.Post("/begin", a.passkeyRegisterBegin)
			r.Post("/finish", a.passkeyRegisterFinish)
			r.Patch("/{id}", a.renamePasskey)
			r.Delete("/{id}", a.deletePasskey)
		})

		// 推流令牌（每用户一把，房间在 WHIP URL 里）；
		// 访客拿不到：令牌不绑设备、跨会话有效，与「跟浏览器走、到期即删」的访客性质冲突
		r.Route("/api/ingest/token", func(r chi.Router) {
			r.Use(a.requireRole(store.RoleUser))
			r.Get("/", a.ingestTokenGet)
			r.Post("/reset", a.ingestTokenReset)
			r.Put("/", a.ingestTokenTag)
		})

		// 频道管理：频道解析与权限校验收敛到子路由中间件
		// （现场管制与白名单 = 频道管理员及以上，归属变更与邀请制开关 = 仅频道主）
		r.Route("/api/channels/{channel}", func(r chi.Router) {
			// kick 单独放行到登录层：踢"自己"的设备（远程下线忘关的 OBS/其他设备）
			// 不需要管理权限，踢别人在 handler 内要求频道管理员及以上
			r.Group(func(r chi.Router) {
				r.Use(a.requireChannel)
				r.Post("/kick", a.kick)
				// 桌面端原生投屏的设备票：权限按入场判定（要能发布），在 handler 内判
				r.Post("/cast-ticket", a.castTicketIssue)
				// 聊天：权限按入场判定（封禁/邀请制/禁言），在 handler 内判，不需要频道管理权限
				r.Get("/messages", a.listMessages)
				r.Post("/messages", a.postMessage)
				r.Delete("/messages", a.clearMessages)
				r.Delete("/messages/{msgID}", a.deleteMessage)
				r.Put("/messages/{msgID}/reactions/{emoji}", a.putReaction)
				r.Delete("/messages/{msgID}/reactions/{emoji}", a.deleteReaction)
				// 频道通知静音：本人视角的提醒开关（PUT/DELETE），与上面 requireModerator
				// 组里对别人施加的禁言（POST /mute）同路径不同方法，互不相干
				r.Put("/mute", a.muteChannel)
				r.Delete("/mute", a.unmuteChannel)
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
				r.Post("/invites", a.createGuestInvite)
				r.Get("/invites", a.listGuestInvites)
				r.Delete("/invites/{id}", a.deleteGuestInvite)
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
			r.Get("/audit", a.adminAudit)
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
			r.Get("/tls", a.adminTLS)
			r.Post("/tls/upload", a.adminTLSUpload)
			r.Post("/tls/ca/rotate", a.adminTLSRotateCA)
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
