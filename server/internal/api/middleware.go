// REST API：注册/登录/频道/进房令牌/频道管理。路由用 chi，鉴权用 Bearer token。
package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"hearth/server/internal/perm"
	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

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
// 访客的两条额外约束在此执行（入场判定的第三条「频道范围」在 admission.go）：
// 会话绑定的设备必须与请求头 X-Device-Id 一致，且账号未到期——都按 401 处理，
// 让前端走统一的「登录已失效」路径。普通会话不绑定设备，请求头带不带都无所谓。
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "缺少登录凭证")
			return
		}
		u, deviceID, err := a.st.UserByToken(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "登录已失效")
			return
		}
		if deviceID != "" && r.Header.Get("X-Device-Id") != deviceID {
			writeErr(w, http.StatusUnauthorized, "这条访客链接绑定在原来的浏览器上，请用原设备打开或另拿一条")
			return
		}
		if u.ExpiresAt != nil && !time.Now().Before(*u.ExpiresAt) {
			writeErr(w, http.StatusUnauthorized, "访客身份已到期")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	})
}

// channelOf 解析 {channel} 频道（id 优先、名字兜底，见 channelByRef）；已写错误响应时返回 nil。
func (a *API) channelOf(w http.ResponseWriter, r *http.Request) *store.Channel {
	c, err := a.channelByRef(r.Context(), chi.URLParam(r, "channel"))
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
		store.RoleUser:  "访客不能使用这个功能，注册一个账号即可",
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

// desktopOrigins 桌面壳里那份网页的本地 origin（macOS/Linux 是 tauri://localhost，
// Windows 是 http(s)://tauri.localhost）。它们不是部署侧能预见的来源，内建放行，
// 不受 CORS_ORIGIN 收紧影响——否则管理员一改配置就把所有桌面端打死。
var desktopOrigins = []string{"tauri://localhost", "http://tauri.localhost", "https://tauri.localhost"}

// corsOrigin 本次请求要回的 Allow-Origin：桌面壳的本地 origin 原样回，其余按配置。
func (a *API) corsOrigin(r *http.Request) string {
	origin := r.Header.Get("Origin")
	for _, d := range desktopOrigins {
		if origin == d {
			return origin
		}
	}
	return a.cfg.CORSOrigin
}

// cors 跨域中间件：开发期前端在 vite dev server（不同端口），桌面壳则是本地 origin。
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin") // 回值随请求的 Origin 变，缓存不能混用
		w.Header().Set("Access-Control-Allow-Origin", a.corsOrigin(r))
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Device-Id")
		w.Header().Set("Access-Control-Expose-Headers", "X-Hearth-Version")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// versionHeader 给 /api/* 响应回填构建版本号：前端凭它探测服务端升级（新旧值不同）
// 并提示刷新，不影响静态资源与 /healthz。
func (a *API) versionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("X-Hearth-Version", a.version)
		}
		next.ServeHTTP(w, r)
	})
}

// BearerToken 从请求头取 token（聊天 WS 也复用）。
func BearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
