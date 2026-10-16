// 注册策略、站点公开信息与邀请（公开查询 / 凭邀请注册 / 管理端 CRUD）。
package api

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hearth/server/internal/perm"
	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

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

// site 下发登录与邀请页需要的公开信息。tls_source 与 http_port 只用于前端拼地址
// （自签证书 OBS 不认，推流地址要用明文端口），不回显任何证书内容。
func (a *API) site(w http.ResponseWriter, r *http.Request) {
	port := 0
	if _, ps, err := net.SplitHostPort(a.cfg.Addr); err == nil {
		port, _ = strconv.Atoi(ps)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       a.cfg.SiteName,
		"policy":     a.regPolicy(r),
		"tls_source": a.dynVal(r.Context(), "tls_cert_source"),
		"http_port":  port,
		// 观看诊断是站点级总开关（cfg_watch_diag），客户端只听这一份、不再各自记本地开关
		"watch_diag": a.dynVal(r.Context(), "watch_diag") == "on",
	})
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
	// 加入页据 kind / allow_guest 决定给哪些入口（注册表单、访客入口或两者）
	writeJSON(w, http.StatusOK, map[string]any{
		"inviter":       inv.CreatedBy,
		"expires_at":    inv.ExpiresAt,
		"alive":         inv.Alive(time.Now()),
		"kind":          inv.Kind,
		"channel_name":  inv.ChannelName,
		"allow_guest":   inv.AllowGuest,
		"guest_ttl_sec": inv.GuestTTLSec,
		"role":          inv.Role,
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
		if inv.Kind == "guest" {
			// 频道访客邀请不产出注册账号：这条链接只有访客入口
			writeErr(w, http.StatusForbidden, "这是一条访客邀请，请用「以访客进入」")
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

// createInvite 发注册邀请（power+）：产出 user；admin+ 可在邀请上指定产出档（user/power）。
func (a *API) createInvite(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Note    string `json:"note"`
		MaxUses int    `json:"max_uses"` // 0 = 不限
		TTL     string `json:"ttl"`      // 1h / 24h / 7d
		Role    string `json:"role"`     // 产出档：user/power，仅 admin+ 可指定（空 = 跟随注册默认档）
		// AllowGuest 允许对方「先以访客进入」再注册转正（访客寿命取站点默认 guest_ttl_sec）
		AllowGuest bool `json:"allow_guest"`
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
	inv, err := a.st.CreateInvite(r.Context(), u.ID, store.InviteSpec{
		Kind: "register", Note: strings.TrimSpace(req.Note), MaxUses: req.MaxUses, TTL: ttl,
		Role: role, AllowGuest: req.AllowGuest,
	})
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

