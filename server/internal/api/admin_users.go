// 管理后台：概览、用户操作与注册策略设置。
package api

import (
	"errors"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"hearth/server/internal/perm"
	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

var serverStart = time.Now()

// ---- 管理后台 ----

// requireAdmin 管理员校验中间件（挂在 auth 之后）。
func (a *API) requireAdmin(next http.Handler) http.Handler {
	return a.requireRole(store.RoleAdmin)(next)
}

// adminOverview 概览：计数、运行信息、组件可达性、资源占用（尽力而为）。
func (a *API) adminOverview(w http.ResponseWriter, r *http.Request) {
	users, channels, err := a.st.Counts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	online := 0
	rtcOK := false
	if counts, err := a.roomCounts(r.Context()); err == nil {
		rtcOK = true
		for _, n := range counts {
			online += n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users":          users,
		"channels":       channels,
		"online":         online,
		"uptime_seconds": int(time.Since(serverStart).Seconds()),
		"go_version":     runtime.Version(),
		"policy":         a.regPolicy(r),
		"services": func() map[string]any {
			_, vp := a.voiceInstance(r.Context())
			_, ip := a.ingestInstance(r.Context())
			ingest := map[string]any{"name": "none", "ok": false, "url": ""}
			if ip != nil {
				ingest = map[string]any{"name": ip.Name(), "ok": ip.Enabled(r.Context()),
					"url": ip.ProxyUpstream(r.Context())}
			}
			sv := map[string]any{
				"voice":  map[string]any{"name": vp.Name(), "ok": rtcOK, "url": vp.SignalProxyUpstream(r.Context())},
				"ingest": ingest,
				"db":     map[string]any{"ok": true, "url": dbLabel(a.cfg.DatabaseDSN())},
			}
			if _, sp := a.stageInstance(r.Context()); sp != nil {
				stageOK := true
				if _, err := sp.RoomCounts(r.Context()); err != nil {
					stageOK = false
				}
				sv["stage"] = map[string]any{"name": sp.Name(), "ok": stageOK, "url": sp.SignalProxyUpstream(r.Context())}
			} else {
				sv["stage"] = map[string]any{"name": "none", "ok": false, "url": ""}
			}
			return sv
		}(),
		"resources": hostResources(),
	})
}

// dbLabel 数据库连接串的展示形式（掩掉可能的密码）。
func dbLabel(dsn string) string {
	if strings.HasPrefix(dsn, "mysql://") || strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if i := strings.Index(dsn, "@"); i >= 0 {
			if j := strings.Index(dsn, "://"); j >= 0 {
				return dsn[:j+3] + "***" + dsn[i:]
			}
		}
		return dsn
	}
	return "sqlite: " + strings.TrimPrefix(dsn, "sqlite:")
}

// hostResources 读取宿主资源占用：仅 Linux 有效，读不到的项为 null。
func hostResources() map[string]any {
	out := map[string]any{"load": nil, "cpus": runtime.NumCPU(), "mem_used_mb": nil, "mem_total_mb": nil, "temp_c": nil}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				out["load"] = v
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail float64
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseFloat(f[1], 64)
			switch f[0] {
			case "MemTotal:":
				total = v
			case "MemAvailable:":
				avail = v
			}
		}
		if total > 0 {
			out["mem_total_mb"] = int(total / 1024)
			out["mem_used_mb"] = int((total - avail) / 1024)
		}
	}
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			out["temp_c"] = v / 1000
		}
	}
	return out
}

func (a *API) adminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.st.ListUsersAdmin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 每行的可授角色候选由服务端按阶梯算好（前端不推导权限，只做显隐）
	actor := userFrom(r)
	for i := range users {
		users[i].CanSetRoles = perm.SettableRoles(actor, &store.User{Role: users[i].Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// adminTargetUser 解析路径里的用户 ID：须存在且不能是自己。
func (a *API) adminTargetUser(w http.ResponseWriter, r *http.Request) *store.User {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "用户 ID 无效")
		return nil
	}
	if id == userFrom(r).ID {
		writeErr(w, http.StatusBadRequest, "不能对自己操作")
		return nil
	}
	t, err := a.st.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return nil
	}
	return t
}

// requireCanActOn 阶梯约束：只能操作比自己低档的用户（super 不可被动）。
func (a *API) requireCanActOn(w http.ResponseWriter, r *http.Request, t *store.User) bool {
	if !perm.CanActOn(userFrom(r), t) {
		writeErr(w, http.StatusForbidden, "不能操作同级或更高权限的用户")
		return false
	}
	return true
}

func (a *API) adminSetUserDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := a.adminTargetUser(w, r)
		if t == nil || !a.requireCanActOn(w, r, t) {
			return
		}
		if err := a.st.SetUserDisabled(r.Context(), t.ID, disabled); err != nil {
			writeErr(w, http.StatusInternalServerError, "内部错误")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// adminSetUserRole 授予/收回系统角色：受 CanActOn 与「只能授到自己以下」双重约束；
// 响应带目标名下频道数（降级 admin 时前端提示「其名下仍有 N 个频道」）。
func (a *API) adminSetUserRole(w http.ResponseWriter, r *http.Request) {
	t := a.adminTargetUser(w, r)
	if t == nil || !a.requireCanActOn(w, r, t) {
		return
	}
	var req struct {
		Role store.Role `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	switch req.Role {
	case store.RoleUser, store.RolePower, store.RoleAdmin:
	default:
		writeErr(w, http.StatusBadRequest, "角色只能是 user / power / admin")
		return
	}
	actor := userFrom(r)
	if req.Role.Rank() >= actor.Role.Rank() {
		writeErr(w, http.StatusForbidden, "只能授予比自己低的角色")
		return
	}
	if t.Role == store.RoleGuest {
		writeErr(w, http.StatusBadRequest, "访客不能授予系统角色（走注册邀请升级）")
		return
	}
	if err := a.st.SetUserRole(r.Context(), t.ID, req.Role); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	owned, _ := a.st.CountOwnedChannels(r.Context(), t.ID)
	writeJSON(w, http.StatusOK, map[string]any{"role": req.Role, "owned_channels": owned})
}

func (a *API) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	t := a.adminTargetUser(w, r)
	if t == nil || !a.requireCanActOn(w, r, t) {
		return
	}
	// 名下频道过户给执行删除的管理员（不再是拒绝删除，避免误伤活跃频道）
	adopted, err := a.st.DeleteUser(r.Context(), t.ID, userFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"adopted_channels": adopted})
}

func (a *API) adminDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "频道 ID 无效")
		return
	}
	// 先查一次只为把"频道不存在"与删除失败区分开
	if _, err = a.st.ChannelByID(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if err := a.st.DeleteChannel(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) adminGetPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"policy":       a.regPolicy(r),
		"default_role": string(a.regDefaultRole(r)),
	})
}

func (a *API) adminSetPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy      string `json:"policy"`
		DefaultRole string `json:"default_role"` // 注册产出默认档：user/power
	}
	if !decode(w, r, &req) {
		return
	}
	switch req.Policy {
	case "closed", "invite", "open":
	default:
		writeErr(w, http.StatusBadRequest, "策略只能是 closed / invite / open")
		return
	}
	switch store.Role(req.DefaultRole) {
	case store.RoleUser, store.RolePower:
	default:
		writeErr(w, http.StatusBadRequest, "默认档只能是 user / power")
		return
	}
	if err := a.st.SetSetting(r.Context(), "reg_policy", req.Policy); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if err := a.st.SetSetting(r.Context(), "reg_default_role", req.DefaultRole); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"policy": req.Policy, "default_role": req.DefaultRole})
}

