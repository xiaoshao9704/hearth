// 账户设置 / 邀请注册 / 管理后台接口。
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
	"hearth/server/internal/rtc"
	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

var serverStart = time.Now()

// ---- 注册策略 ----

// regPolicy 当前注册策略：后台设置优先，否则取配置默认值。
func (a *API) regPolicy(r *http.Request) string {
	if v, err := a.st.GetSetting(r.Context(), "reg_policy"); err == nil {
		switch v {
		case "closed", "invite", "open":
			return v
		}
	}
	return a.cfg.DefaultRegPolicy()
}

// regDefaultRole 注册产出的默认系统角色（cfg_reg_default_role，user/power，默认 user）。
func (a *API) regDefaultRole(r *http.Request) store.Role {
	if v, err := a.st.GetSetting(r.Context(), "reg_default_role"); err == nil && store.Role(v) == store.RolePower {
		return store.RolePower
	}
	return store.RoleUser
}

// site 下发登录与邀请页需要的公开信息，不承载首启或 TLS 状态。
func (a *API) site(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"name":   a.cfg.SiteName,
		"policy": a.regPolicy(r),
	})
}

// ---- 账户 ----

func (a *API) updateUsername(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Username string `json:"username"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRe.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "用户名需 2-32 位字母数字、-、_")
		return
	}
	if req.Username == u.Username {
		writeErr(w, http.StatusBadRequest, "和当前用户名相同")
		return
	}
	err := a.st.UpdateUsername(r.Context(), u.ID, req.Username)
	if store.IsUniqueViolation(err) {
		writeErr(w, http.StatusConflict, "用户名已被占用")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	u.Username = req.Username
	writeJSON(w, http.StatusOK, u)
}

func (a *API) updatePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.New) < 8 {
		writeErr(w, http.StatusBadRequest, "新密码至少 8 位")
		return
	}
	hash, err := a.st.PasswordHash(r.Context(), u.ID)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Current)) != nil {
		// 400 而非 401：会话本身有效，401 会触发前端的"会话失效"全局登出
		writeErr(w, http.StatusBadRequest, "当前密码不正确")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if err := a.st.UpdatePassword(r.Context(), u.ID, string(newHash)); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 改密后其他设备会话全部退出，当前会话保留
	a.st.DeleteOtherSessions(r.Context(), u.ID, BearerToken(r))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listMyDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := a.st.ListDevices(r.Context(), userFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (a *API) deleteMyDevice(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DeleteDevice(r.Context(), userFrom(r).ID, chi.URLParam(r, "deviceID")); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 邀请（公开查询 + 凭邀请注册）----

func (a *API) inviteInfo(w http.ResponseWriter, r *http.Request) {
	inv, err := a.st.InviteByCode(r.Context(), chi.URLParam(r, "code"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "邀请链接不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"inviter":    inv.CreatedBy,
		"expires_at": inv.ExpiresAt,
		"alive":      inv.Alive(time.Now()),
	})
}

// registerWithPolicy 按注册策略处理注册：open 直接注册；invite 校验并消耗邀请；closed 拒绝。
func (a *API) registerWithPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Invite   string `json:"invite"`
	}
	if !decode(w, r, &req) {
		return
	}
	policy := a.regPolicy(r)
	var inv *store.Invite
	switch policy {
	case "closed":
		writeErr(w, http.StatusForbidden, "注册已关闭，请联系管理员开通账号")
		return
	case "invite":
		if req.Invite == "" {
			writeErr(w, http.StatusForbidden, "注册需要邀请链接")
			return
		}
		var err error
		inv, err = a.st.InviteByCode(r.Context(), req.Invite)
		if err != nil || !inv.Alive(time.Now()) {
			writeErr(w, http.StatusForbidden, "邀请链接无效或已过期")
			return
		}
	}
	if !usernameRe.MatchString(req.Username) || len(req.Password) < 6 {
		writeErr(w, http.StatusBadRequest, "用户名需 2-32 位字母数字，密码至少 6 位")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if inv != nil {
		// 先占名额再建号，防止并发超发；建号失败退还不做（损失一个名额，可再发）
		if err := a.st.ConsumeInvite(r.Context(), inv.ID); err != nil {
			writeErr(w, http.StatusForbidden, "邀请链接名额已用完")
			return
		}
	}
	// 产出档：邀请上指定的优先（admin 发邀请时可指定 user/power），否则跟随注册默认档
	role := a.regDefaultRole(r)
	if inv != nil {
		switch store.Role(inv.Role) {
		case store.RoleUser, store.RolePower:
			role = store.Role(inv.Role)
		}
	}
	u, err := a.st.CreateUserWithRole(r.Context(), req.Username, string(hash), role)
	if store.IsUniqueViolation(err) {
		writeErr(w, http.StatusConflict, "用户名已被占用")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.issueSession(w, r, u)
}

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

// createInvite 发注册邀请（power+）：产出 user；admin+ 可在邀请上指定产出档（user/power）。
func (a *API) createInvite(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Note    string `json:"note"`
		MaxUses int    `json:"max_uses"` // 0 = 不限
		TTL     string `json:"ttl"`      // 1h / 24h / 7d
		Role    string `json:"role"`     // 产出档：user/power，仅 admin+ 可指定（空 = 跟随注册默认档）
	}
	if !decode(w, r, &req) {
		return
	}
	ttl := map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}[req.TTL]
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	if req.MaxUses < 0 || req.MaxUses > 100 {
		req.MaxUses = 1
	}
	role := store.Role("")
	if req.Role != "" {
		if !perm.SysAtLeast(u, store.RoleAdmin) {
			writeErr(w, http.StatusForbidden, "只有管理员能指定邀请产出档")
			return
		}
		switch store.Role(req.Role) {
		case store.RoleUser, store.RolePower:
			role = store.Role(req.Role)
		default:
			writeErr(w, http.StatusBadRequest, "产出档只能是 user / power")
			return
		}
	}
	inv, err := a.st.CreateInvite(r.Context(), u.ID, strings.TrimSpace(req.Note), req.MaxUses, ttl, role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"invite": inv,
		"url":    a.publicBase(r) + "/#/join/" + inv.Code,
	})
}

// publicBase 站点公开地址：配置优先，否则按请求推导同源。
func (a *API) publicBase(r *http.Request) string {
	if a.cfg.PublicURL != "" {
		return strings.TrimSuffix(a.cfg.PublicURL, "/")
	}
	return requestScheme(r) + "://" + r.Host
}

// listInvites 列邀请：admin+ 看全部，power 只看自己发的。
func (a *API) listInvites(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var invites []store.Invite
	var err error
	if perm.SysAtLeast(u, store.RoleAdmin) {
		invites, err = a.st.ListInvites(r.Context())
	} else {
		invites, err = a.st.ListInvitesByCreator(r.Context(), u.ID)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites, "base": a.publicBase(r)})
}

// revokeInvite 有效邀请撤销、失效邀请删除（前端同一个按钮）：只能动自己发的，admin+ 任意。
func (a *API) revokeInvite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邀请 ID 无效")
		return
	}
	u := userFrom(r)
	if !perm.SysAtLeast(u, store.RoleAdmin) {
		inv, err := a.st.InviteByID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "邀请不存在")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "内部错误")
			return
		}
		if inv.CreatedByID != u.ID {
			writeErr(w, http.StatusForbidden, "只能撤销自己发的邀请")
			return
		}
	}
	if err := a.st.DeleteInvite(r.Context(), id); err != nil {
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

// ---- 频道参与者（房主视角频道管理用）----

func (a *API) channelParticipants(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	// 两条线的参与者并集（OBS 推流只在舞台线；同一设备两线 identity 一致会去重）
	seen := map[string]bool{}
	ps := []rtc.Participant{}
	_, vp := a.voiceInstance(r.Context())
	providers := []rtc.Provider{vp}
	if _, sp := a.stageInstance(r.Context()); sp != nil {
		providers = append(providers, sp)
	}
	for _, pr := range providers {
		if list, err := pr.ListParticipants(r.Context(), c.Name); err == nil {
			for _, x := range list {
				if !seen[x.Identity] {
					seen[x.Identity] = true
					ps = append(ps, x)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"participants": ps})
}
