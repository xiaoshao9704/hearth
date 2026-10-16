// REST API：注册/登录/频道/进房令牌/频道管理。路由用 chi，鉴权用 Bearer token。
package api

import (
	"net/http"
	"regexp"

	"hearth/server/internal/perm"
	"hearth/server/internal/store"
)

// ---- 频道 ----

var channelNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// listChannels 大厅列表可见性：邀请制频道对非成员不可见（唯一例外是 super，
// 看得到但标 hidden，提示普通人看不到）；被封禁不影响可见性（banned 标记，进入按钮由前端禁用）
// ——封禁是"能不能进"，不是"看不看得见"。
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
	memberships, err := a.st.MembershipsOf(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	bans, err := a.st.BansOf(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 静音是每人每频道的提醒开关（见 push.go）：批量取一次，逐频道填 muted
	mutes, err := a.st.MutedChannelIDs(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	isSuper := perm.SysAtLeast(u, store.RoleSuper)
	// 系统 admin+ 在任何频道隐含频道主（与 perm.ChannelRole 同口径，批量填充免逐频道查询）
	implicit := ""
	if perm.SysAtLeast(u, store.RoleAdmin) {
		implicit = string(store.ChannelRoleOwner)
	}
	counts, _ := a.roomCounts(r.Context()) // LiveKit 不可达时在线数保持 0
	visible := chs[:0]
	for i := range chs {
		c := &chs[i]
		member := memberships[c.ID]
		if c.InviteOnly && !member {
			if !isSuper {
				continue // 非成员看不到邀请制频道，唯一例外是 super
			}
			c.Hidden = true // super 能看到，但标出来提示普通人看不到
		}
		c.MyRole = implicit
		if c.MyRole == "" {
			c.MyRole = string(roles[c.ID])
		}
		c.Online = counts[c.Name]
		c.Banned = bans[c.ID]
		c.Muted = mutes[c.ID]
		visible = append(visible, *c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": visible})
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
	// 纯数字名会与「路径段先按频道 id 解析」歧义（见 channelByRef）
	if numericRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "频道名不能是纯数字")
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

// requestScheme 请求的外部协议：尊重 X-Forwarded-Proto（反代后），否则看 TLS。
