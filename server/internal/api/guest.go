// 访客：频道访客邀请（moderator+ 发）、访客入场（公开）、访客转正、过期清理。
// 入场判定本身不在这里——「谁能进哪个频道」仍只在 admission.go。
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

// guestPurgeInterval 过期访客的清理周期（启动时先清一次，之后每小时一次）。
// 在房的访客最多再多留一个凭证 TTL：凭证到期重签时 auth 已经 401。
const guestPurgeInterval = time.Hour

// guestDeviceIDRe 前端 localStorage 里的设备 ID（api.ts 的 deviceId()）：短 hex 串。
// 会话绑定的就是这个值，格式收紧免得把任意字符串写进绑定列。
var guestDeviceIDRe = regexp.MustCompile(`^[a-zA-Z0-9]{4,32}$`)

// guestTTL 站点默认访客寿命（注册邀请的「先以访客进入」用；频道访客邀请自带寿命）。
func (a *API) guestTTL(ctx context.Context) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(a.dynVal(ctx, "guest_ttl_sec")))
	if err != nil || n <= 0 {
		n = 86400
	}
	return time.Duration(n) * time.Second
}

// canClaimGuest 前端转正入口的显隐依据（`can_claim`）：当前身份是访客，且站点开了
// `guest_claim`。非访客恒 false——转正只对访客有意义。
func (a *API) canClaimGuest(ctx context.Context, u *store.User) bool {
	return u.Role == store.RoleGuest && strings.TrimSpace(a.dynVal(ctx, "guest_claim")) == "on"
}

// guestInviteTTLs 频道访客邀请的可选寿命档（前端只发这几个键）。
var guestInviteTTLs = map[string]time.Duration{
	"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
}

// createGuestInvite 频道访客邀请（moderator+）：链接产出的是绑在本频道的访客，
// 不是注册账号——因此不受注册策略影响，也不占 power 的发邀请能力。
func (a *API) createGuestInvite(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	var req struct {
		Note    string `json:"note"`
		MaxUses int    `json:"max_uses"`  // 0 = 不限
		TTL     string `json:"ttl"`       // 链接自身的有效期：1h / 24h / 7d
		Guest   string `json:"guest_ttl"` // 产出访客的寿命：1h / 24h / 7d
	}
	if !decode(w, r, &req) {
		return
	}
	ttl := guestInviteTTLs[req.TTL]
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	guestTTL := guestInviteTTLs[req.Guest]
	if guestTTL == 0 {
		guestTTL = a.guestTTL(r.Context())
	}
	if req.MaxUses < 0 || req.MaxUses > 100 {
		req.MaxUses = 1
	}
	inv, err := a.st.CreateInvite(r.Context(), userFrom(r).ID, store.InviteSpec{
		Kind: "guest", Note: strings.TrimSpace(req.Note), MaxUses: req.MaxUses, TTL: ttl,
		ChannelID: c.ID, GuestTTL: guestTTL,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	inv.ChannelName = c.Name
	writeJSON(w, http.StatusCreated, map[string]any{
		"invite": inv,
		"url":    a.publicBase(r) + "/#/join/" + inv.Code,
	})
}

func (a *API) listGuestInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := a.st.ListInvitesByChannel(r.Context(), channelFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invites, "base": a.publicBase(r)})
}

// deleteGuestInvite 撤销/删除本频道的访客邀请：只认属于这个频道的 guest 类邀请
// （频道管理员的权限止于自己的频道，不能顺着 id 动别处的邀请）。
func (a *API) deleteGuestInvite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邀请 ID 无效")
		return
	}
	inv, err := a.st.InviteByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "邀请不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	c := channelFrom(r)
	if inv.Kind != "guest" || inv.ChannelID == nil || *inv.ChannelID != c.ID {
		writeErr(w, http.StatusNotFound, "邀请不存在")
		return
	}
	if err := a.st.DeleteInvite(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guestEntry 访客入场（公开）：kind=guest 的邀请，或勾了「允许先以访客进入」的注册邀请。
// 产出 role=guest 的账号（无密码、有 expires_at）、绑定这台浏览器的设备 ID，
// 消耗一次邀请名额，返回与登录同形状的 {token,user}；guest 类还把访客写进该频道的白名单。
func (a *API) guestEntry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		DeviceID string `json:"device_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	inv, err := a.st.InviteByCode(r.Context(), chi.URLParam(r, "code"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "邀请链接不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !inv.Alive(time.Now()) {
		writeErr(w, http.StatusForbidden, "邀请链接无效或已过期")
		return
	}
	var channelID int64
	var ttl time.Duration
	switch {
	case inv.Kind == "guest":
		if inv.ChannelID == nil {
			writeErr(w, http.StatusForbidden, "这条访客邀请没有指定频道，请重新生成")
			return
		}
		channelID = *inv.ChannelID
		ttl = time.Duration(inv.GuestTTLSec) * time.Second
		if ttl <= 0 {
			ttl = a.guestTTL(r.Context())
		}
	case inv.AllowGuest: // 注册邀请的「先以访客进入」：不绑频道，可进普通用户能进的地方
		ttl = a.guestTTL(r.Context())
	default:
		writeErr(w, http.StatusForbidden, "这条邀请不支持以访客进入，请注册账号")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRe.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "展示名需 2-32 位字母数字、-、_")
		return
	}
	if !guestDeviceIDRe.MatchString(req.DeviceID) {
		writeErr(w, http.StatusBadRequest, "缺少设备标识，请刷新页面重试")
		return
	}
	// 先占名额再建号，防止并发超发（与注册同口径：建号失败不退还名额）
	if err := a.st.ConsumeInvite(r.Context(), inv.ID); err != nil {
		writeErr(w, http.StatusForbidden, "邀请链接名额已用完")
		return
	}
	u, err := a.st.CreateGuest(r.Context(), req.Username, time.Now().Add(ttl), inv.ID)
	if store.IsUniqueViolation(err) {
		writeErr(w, http.StatusConflict, "这个名字已经被占用了，换一个")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if channelID > 0 {
		// 访客的频道授予就是一行 member（邀请制频道也因此进得去）
		if err := a.st.SetChannelRole(r.Context(), channelID, u.ID, store.ChannelRoleMember); err != nil {
			writeErr(w, http.StatusInternalServerError, "内部错误")
			return
		}
	}
	token, err := a.st.CreateSessionWithDevice(r.Context(), u.ID, r.UserAgent(), req.DeviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		// user 与 /api/me 同形状（带 can_claim）：进大厅那一刻就要知道转正入口该不该出
		"token": token,
		"user": struct {
			*store.User
			CanClaim bool `json:"can_claim"`
		}{u, a.canClaimGuest(r.Context(), u)},
		"channel": inv.ChannelName,
	})
}

// claimGuest 访客转正（仅 role=guest）：user_id 不变，因此聊天记录、频道成员关系、
// 管制状态都自然延续；解除会话的设备绑定后当前这台设备继续用同一个 token。
// 不再消耗邀请名额——进入时已经消耗过一次。
func (a *API) claimGuest(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u.Role != store.RoleGuest {
		writeErr(w, http.StatusBadRequest, "当前账号已经是注册账号")
		return
	}
	if !a.canClaimGuest(r.Context(), u) {
		writeErr(w, http.StatusForbidden, "本站未开放访客转正")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRe.MatchString(req.Username) || len(req.Password) < 6 {
		writeErr(w, http.StatusBadRequest, "用户名需 2-32 位字母数字，密码至少 6 位")
		return
	}
	// 产出档：来源邀请上指定的优先（注册邀请可带 user/power），否则跟随注册默认档
	role := a.regDefaultRole(r)
	if inv, err := a.st.GuestSourceInvite(r.Context(), u.ID); err == nil {
		switch store.Role(inv.Role) {
		case store.RoleUser, store.RolePower:
			role = store.Role(inv.Role)
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if req.Username != u.Username {
		// 改名与转正同一步：先探一次占用，把「名字被占」与内部错误分开（唯一索引仍是最终裁判）
		if _, _, err := a.st.UserByName(r.Context(), req.Username); err == nil {
			writeErr(w, http.StatusConflict, "用户名已被占用")
			return
		}
	}
	if err := a.st.ClaimGuest(r.Context(), u.ID, req.Username, string(hash), role); err != nil {
		if store.IsUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "用户名已被占用")
			return
		}
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	u.Username = req.Username
	u.Role = role
	u.IsAdmin = false
	u.ExpiresAt = nil
	a.audit(r.Context(), u.ID, store.AuditGuestClaim, u.ID, 0, "访客转正为注册账号（user_id 不变）")
	writeJSON(w, http.StatusOK, u)
}

// purgeGuests 清理一次过期访客。
func (a *API) purgeGuests(ctx context.Context) {
	n, err := a.st.PurgeExpiredGuests(ctx, time.Now())
	if err != nil {
		log.Printf("过期访客清理失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("过期访客清理: 删除 %d 个访客账号", n)
	}
}

// RunGuestPurge 启动时清一次，之后每小时一次。
func (a *API) RunGuestPurge(ctx context.Context) {
	a.purgeGuests(ctx)
	t := time.NewTicker(guestPurgeInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.purgeGuests(ctx)
		case <-ctx.Done():
			return
		}
	}
}
