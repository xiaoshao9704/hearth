// 频道访客：访客邀请（moderator+ 签发/管理）与访客入场（公开接口）。
// 访客 = role=guest 的临时用户：设备绑定（sessions.device_id）、有 expires_at、
// 只能进 channel_members 里有自己行的频道；约束的判定收口在 auth 中间件与 admitUser。
package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

// 访客邀请的有效期与产出访客寿命共用同一组挡位
var guestInviteTTLs = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

// createChannelGuestInvite 发访客邀请（moderator+）：频道固定为路径里的频道，
// ttl 是链接有效期，guest_ttl 是访客入场后的身份寿命（同组挡位）。
func (a *API) createChannelGuestInvite(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	var req struct {
		TTL      string `json:"ttl"`       // 链接有效期：1h / 24h / 7d
		GuestTTL string `json:"guest_ttl"` // 访客寿命：1h / 24h / 7d
		MaxUses  int    `json:"max_uses"`  // 0 = 不限
	}
	if !decode(w, r, &req) {
		return
	}
	ttl, ok := guestInviteTTLs[req.TTL]
	if !ok {
		ttl = 24 * time.Hour
	}
	guestTTL, ok := guestInviteTTLs[req.GuestTTL]
	if !ok {
		guestTTL = ttl // 缺省与链接同寿命
	}
	if req.MaxUses < 0 || req.MaxUses > 100 {
		req.MaxUses = 1
	}
	inv, err := a.st.CreateGuestInvite(r.Context(), userFrom(r).ID, c.ID, req.MaxUses, ttl, guestTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"invite": inv,
		"url":    a.publicBase(r) + "/#/join/" + inv.Code,
	})
}

// listChannelGuestInvites 列本频道的访客邀请（moderator+）。
func (a *API) listChannelGuestInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := a.st.ListGuestInvites(r.Context(), channelFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites, "base": a.publicBase(r)})
}

// revokeChannelGuestInvite 撤销/删除本频道的访客邀请（moderator+，跨频道拒 404 防探测）。
func (a *API) revokeChannelGuestInvite(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邀请 ID 无效")
		return
	}
	inv, err := a.st.InviteByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (inv.Kind != "guest" || inv.ChannelID == nil || *inv.ChannelID != c.ID)) {
		writeErr(w, http.StatusNotFound, "邀请不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if err := a.st.DeleteInvite(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guestJoin 访客入场（公开）：凭 guest 类邀请建临时账号（角色 guest、寿命 = 邀请的
// guest_ttl_sec、记 invite_id），写入频道成员行（member），签发绑定设备的会话。
// 前端拿到 token 与目标频道直接进房，不经大厅。
func (a *API) guestJoin(w http.ResponseWriter, r *http.Request) {
	inv, err := a.st.InviteByCode(r.Context(), chi.URLParam(r, "code"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "邀请链接不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if inv.Kind != "guest" || inv.ChannelID == nil {
		writeErr(w, http.StatusBadRequest, "该链接不是访客邀请")
		return
	}
	if !inv.Alive(time.Now()) {
		writeErr(w, http.StatusForbidden, "邀请链接已失效")
		return
	}
	var req struct {
		Username string `json:"username"`
		DeviceID string `json:"device_id"` // 前端 localStorage 持久设备 ID：会话绑定它，换设备即 401
	}
	if !decode(w, r, &req) {
		return
	}
	if !usernameRe.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "展示名需 2-32 位字母数字、-、_")
		return
	}
	dev := deviceIDRe.FindString(req.DeviceID)
	if dev == "" {
		writeErr(w, http.StatusBadRequest, "缺少设备标识")
		return
	}
	c, err := a.st.ChannelByID(r.Context(), *inv.ChannelID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return
	}
	// 先占名额再建号（与注册邀请同口径）：建号失败损失一个名额
	if err := a.st.ConsumeInvite(r.Context(), inv.ID); err != nil {
		writeErr(w, http.StatusForbidden, "邀请链接名额已用完或已失效")
		return
	}
	u, err := a.st.CreateGuest(r.Context(), req.Username, inv.ID, time.Duration(inv.GuestTTLSec)*time.Second)
	if store.IsUniqueViolation(err) {
		writeErr(w, http.StatusConflict, "这个名字已被占用，换一个")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if err := a.st.SetChannelRole(r.Context(), c.ID, u.ID, store.ChannelRoleMember); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	token, err := a.st.CreateSessionForDevice(r.Context(), u.ID, dev)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u, "channel": c.Name})
}
