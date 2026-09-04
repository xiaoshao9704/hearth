// 推流令牌 API：每用户一把（/api/ingest/token），房间在 WHIP URL 里。
// 旧 /api/ingress、/api/ingress/reset 随 ingresses 密钥模型删除，不留兼容。
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	neturl "net/url"
	"regexp"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// 推流设备标签规则（identity = {用户名}-{标签}，出现在内核参与者列表里）
var ingestTagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// writeIngestToken 回令牌、设备标签与同源 WHIP 基地址（/providers/{alias}/w/ 绝对地址，
// 前端拼上频道即完整服务器地址）。推流一律进当前舞台实例自带的 WHIP 入口；
// enabled=false 表示入口当前不可用（舞台线关闭，或实例推流面缺配置/未在跑）：
// 地址照常给出，前端据此提示，免得用户填进 OBS 推起来才撞 404。
func (a *API) writeIngestToken(w http.ResponseWriter, r *http.Request, t *store.IngestToken) {
	alias, ip := a.ingestInstance(r.Context())
	base := ""
	enabled := false
	if ip != nil {
		base = (&neturl.URL{Scheme: requestScheme(r), Host: r.Host, Path: "/providers/" + alias + "/w/"}).String()
		enabled = ip.Enabled(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": t.Token, "tag": t.Tag, "base": base, "enabled": enabled,
	})
}

// GET /api/ingest/token：取当前用户的推流令牌；无令牌时自动创建（tag=obs）。
func (a *API) ingestTokenGet(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	t, err := a.st.IngestTokenByUser(r.Context(), u.ID)
	if errors.Is(err, store.ErrNotFound) {
		t, err = a.st.CreateIngestToken(r.Context(), u.ID, "obs")
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.writeIngestToken(w, r, t)
}

// POST /api/ingest/token/reset：换令牌值——旧令牌立即失效，其名下进行中的会话全部掐断。
// 无令牌时等同创建。
func (a *API) ingestTokenReset(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	ctx := r.Context()
	if old, err := a.st.IngestTokenByUser(ctx, u.ID); err == nil {
		a.revokeIngestSessions(ctx, old.Token)
	}
	t, err := a.st.ResetIngestToken(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		t, err = a.st.CreateIngestToken(ctx, u.ID, "obs")
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.writeIngestToken(w, r, t)
}

// PUT /api/ingest/token {tag}：改设备标签（下次推流的 identity 生效）。无令牌时按新标签直接创建。
func (a *API) ingestTokenTag(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Tag string `json:"tag"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !ingestTagRe.MatchString(req.Tag) {
		writeErr(w, http.StatusBadRequest, "标签仅限 1-32 位小写字母、数字、-，且以字母或数字开头")
		return
	}
	ctx := r.Context()
	t, err := a.st.IngestTokenByUser(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		t, err = a.st.CreateIngestToken(ctx, u.ID, req.Tag)
	} else if err == nil {
		if err = a.st.UpdateIngestTokenTag(ctx, u.ID, req.Tag); err == nil {
			t.Tag = req.Tag
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.writeIngestToken(w, r, t)
}

// revokeIngestSessions 掐断该令牌名下的全部进行会话（尽力，逐实例忽略错误）：
// 进程内实例直接 RevokeToken，远端 bellows 经 revoke 通行证（RevokeRemoteSessions）。
func (a *API) revokeIngestSessions(ctx context.Context, token string) {
	for _, inst := range a.listInstances(ctx) {
		if inst.Ingest == nil {
			continue
		}
		var err error
		if gi, ok := inst.Ingest.(rtc.WHIPGrantIssuer); ok && inst.Ingest.ProxyUpstream(ctx) != "" {
			err = gi.RevokeRemoteSessions(ctx, token)
		} else {
			err = inst.Ingest.RevokeToken(ctx, token)
		}
		if err != nil {
			log.Printf("撤销推流会话失败（实例 %s）: %v", inst.Alias, err)
		}
	}
}
