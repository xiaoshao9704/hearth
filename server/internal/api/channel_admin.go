// 频道管理（房主操作）：踢出/封禁/禁言/白名单/管理员与转让。
package api

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"hearth/server/internal/perm"
	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// ---- 频道管理（房主操作）----

// resolveTargetUser 解析 body 里的目标 user_id：须存在且不能是自己。
// 目标一律用 user_id——用户名可改、改后旧名即释放，拿它做操作目标会在
// 改名或重注册后打到别人身上；前端从参与者元数据的 uid 取。
func (a *API) resolveTargetUser(w http.ResponseWriter, r *http.Request, u *store.User) *store.User {
	return a.resolveTargetUserWithSelf(w, r, u, false)
}

func (a *API) resolveTargetUserWithSelf(w http.ResponseWriter, r *http.Request, u *store.User, allowSelf bool) *store.User {
	var req struct {
		UserID int64 `json:"user_id"`
	}
	if !decode(w, r, &req) {
		return nil
	}
	if req.UserID == u.ID && !allowSelf {
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

// canModerateTarget 同时守住两条正交边界：admin+ 目标仍受严格系统阶梯保护，
// 频道内又必须严格高于目标的频道角色。解除自身状态是唯一例外。
func (a *API) canModerateTarget(w http.ResponseWriter, r *http.Request, c *store.Channel, actor, target *store.User, allowSelf bool) bool {
	if allowSelf && actor.ID == target.ID {
		return true
	}
	if perm.SysAtLeast(target, store.RoleAdmin) && !perm.CanActOn(actor, target) {
		writeErr(w, http.StatusForbidden, "不能操作受保护的管理员")
		return false
	}
	actorRole, err := perm.ChannelRole(r.Context(), a.st, c, actor)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return false
	}
	targetRole, err := perm.ChannelRole(r.Context(), a.st, c, target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return false
	}
	if !perm.CanActOnChannel(actorRole, targetRole) {
		writeErr(w, http.StatusForbidden, "不能操作频道角色不低于自己的用户")
		return false
	}
	return true
}

// evict 把用户从频道现场移除：identity 为空踢全部设备，非空只踢该设备
// （归属约束由内核侧按 user_id 保底）。聊天没有独立长连接——数据通道随参与者一起断。
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
	if !a.canModerateTarget(w, r, c, u, t, true) {
		return
	}
	n, err := a.evict(r, c, t, req.Identity)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "踢出失败（内核不可达）")
		return
	}
	if req.UserID != u.ID { // 踢自己的设备是自助操作，不进审计
		detail := "全部设备"
		if req.Identity != "" {
			detail = "设备 " + req.Identity
		}
		a.auditAct(r, store.AuditKick, t, c.ID, detail)
	}
	writeJSON(w, http.StatusOK, map[string]any{"kicked": n})
}

func (a *API) ban(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	t := a.resolveTargetUser(w, r, u)
	if t == nil {
		return
	}
	if !a.canModerateTarget(w, r, c, u, t, false) {
		return
	}
	if err := a.st.Ban(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 封禁立即生效：踢出现场（LiveKit 失败不阻塞，token/WS 入口已拦死）
	a.evict(r, c, t, "")
	a.auditAct(r, store.AuditBan, t, c.ID, "")
	writeJSON(w, http.StatusOK, map[string]string{"banned": t.Username})
}

func (a *API) unban(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	t := a.resolveTargetUserWithSelf(w, r, u, true)
	if t == nil {
		return
	}
	if !a.canModerateTarget(w, r, c, u, t, true) {
		return
	}
	if err := a.st.Unban(r.Context(), c.ID, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.auditAct(r, store.AuditUnban, t, c.ID, "")
	w.WriteHeader(http.StatusNoContent)
}

// setGag 禁言/解禁目标用户（模式同 ban：落库为权威，现场传播尽力）。
// 落库（channel_gags）保证离房/重进都生效——joinToken 与推流入场判定都按它签发/拦截；
// 内核调用只负责让"当前在房"的设备立即失声，目标不在房（ErrNoParticipant）不算失败。
func (a *API) setGag(w http.ResponseWriter, r *http.Request, muted bool) {
	c := channelFrom(r)
	u := userFrom(r)
	t := a.resolveTargetUserWithSelf(w, r, u, !muted)
	if t == nil {
		return
	}
	if !a.canModerateTarget(w, r, c, u, t, !muted) {
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
	key, action := "unmuted", store.AuditUnmute
	if muted {
		key, action = "muted", store.AuditMute
	}
	a.auditAct(r, action, t, c.ID, "")
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
	removed, err := a.st.RemoveMember(r.Context(), c.ID, t.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !removed {
		writeErr(w, http.StatusBadRequest, "对方不是可移出的普通成员")
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
	if t.Disabled {
		writeErr(w, http.StatusBadRequest, "停用账号不能持有频道角色")
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
	a.auditAct(r, store.AuditChannelRole, t, c.ID, "转让频道（对方成为频道主，原频道主降为频道管理员）")
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
	a.auditAct(r, store.AuditChannelRole, t, c.ID, "授予频道管理员")
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
	a.auditAct(r, store.AuditChannelRole, t, c.ID, "收回频道管理员")
	w.WriteHeader(http.StatusNoContent)
}

