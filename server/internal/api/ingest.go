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
// 前端拼上频道即完整服务器地址）。enabled=false 表示推流入口当前不可用（所选实例缺配置，
// 或舞台线关闭让进程内 bellows 取不到发布出口）：地址照常给出，前端据此提示，
// 免得用户填进 OBS 推起来才撞 503。
func (a *API) writeIngestToken(w http.ResponseWriter, r *http.Request, t *store.IngestToken) {
	alias, ip, _ := a.ingestInstance(r.Context())
	base := (&neturl.URL{Scheme: requestScheme(r), Host: r.Host, Path: "/providers/" + alias + "/w/"}).String()
	writeJSON(w, http.StatusOK, map[string]any{
		"token": t.Token, "tag": t.Tag, "base": base, "enabled": ip.Enabled(r.Context()),
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

// POST /api/ingest/token/reset：换令牌值——旧令牌立即失效，其名下进行中的会话全部掐断，
// 各实例端点删除（下次推流重建）。无令牌时等同创建。
func (a *API) ingestTokenReset(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	ctx := r.Context()
	if old, err := a.st.IngestTokenByUser(ctx, u.ID); err == nil {
		a.revokeIngestSessions(ctx, old.Token)
		a.teardownIngestEndpoints(ctx, old.ID)
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

// PUT /api/ingest/token {tag}：改设备标签（下次推流的 identity 生效）。端点带旧 identity，
// 随标签一并删除重建——livekit-ingress 形态下删端点会终止其上正在进行的推流（进程内 bellows
// 无上游端点，不受影响）。无令牌时按新标签直接创建。
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
			a.teardownIngestEndpoints(ctx, t.ID)
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

// teardownIngestEndpoints 删除该令牌名下的全部实例端点：逐实例尽力 DeleteEndpoint
// （内核侧）后清空 ingest_endpoints 表，下次推流重建。
func (a *API) teardownIngestEndpoints(ctx context.Context, tokenID int64) {
	for _, inst := range a.listInstances(ctx) {
		if inst.Ingest == nil {
			continue
		}
		ep, err := a.st.IngestEndpoint(ctx, tokenID, inst.Alias)
		if err != nil { // 含 ErrNotFound（该实例无端点）；查询失败同样尽力跳过
			continue
		}
		if derr := inst.Ingest.DeleteEndpoint(ctx, ep.IngressID); derr != nil {
			log.Printf("删除 ingress 端点失败（实例 %s）: %v", inst.Alias, derr)
		}
	}
	if err := a.st.DeleteIngestEndpointsByToken(ctx, tokenID); err != nil {
		log.Printf("清空 ingress 端点记录失败: %v", err)
	}
}
