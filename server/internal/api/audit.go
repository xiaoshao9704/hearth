// 审计日志：写入辅助 + 管理后台查询 + 保留策略清理。
// 写入是旁路：管制动作已经落库生效，审计写失败只打日志，不改动作的成败。
package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hearth/server/internal/store"
)

// auditRetentionInterval 保留策略的清理周期（启动时先跑一次，之后每小时一次）。
const auditRetentionInterval = time.Hour

// audit 记一条审计。target/channel 传 0 表示不适用；detail 是给人看的补充说明。
// 不返回错误：调用点都在动作已经成功之后，审计写失败不该让请求失败。
func (a *API) audit(ctx context.Context, actorUID int64, action string, targetUID, channelID int64, detail string) {
	err := a.st.Audit(ctx, store.AuditRecord{
		ActorUID: actorUID, Action: action, TargetUID: targetUID, ChannelID: channelID, Detail: detail,
	})
	if err != nil {
		log.Printf("审计写入失败 action=%s actor=%d target=%d: %v", action, actorUID, targetUID, err)
	}
}

// auditAct 从请求上下文取操作者，记一条针对目标用户的频道内审计。
func (a *API) auditAct(r *http.Request, action string, target *store.User, channelID int64, detail string) {
	var targetUID int64
	if target != nil {
		targetUID = target.ID
	}
	a.audit(r.Context(), userFrom(r).ID, action, targetUID, channelID, detail)
}

// adminAudit 审计查询（admin+）：channel/actor 收 id，action 收动作名，
// after 是往更早翻页的游标（列表按 id 倒序，游标语义是"比这一条更早"）。
func (a *API) adminAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{
		ChannelID: qInt(q.Get("channel")),
		ActorUID:  qInt(q.Get("actor")),
		Action:    strings.TrimSpace(q.Get("action")),
		AfterID:   qInt(q.Get("after")),
		Limit:     int(qInt(q.Get("limit"))),
	}
	if f.Action != "" {
		known := false
		for _, name := range store.AuditActions {
			if name == f.Action {
				known = true
				break
			}
		}
		if !known {
			writeErr(w, http.StatusBadRequest, "未知的审计动作: "+f.Action)
			return
		}
	}
	entries, err := a.st.ListAudit(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// next 是继续往更早翻的游标；本页没满说明到底了，返回 0
	var next int64
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if len(entries) == f.Limit {
		next = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries, "next": next, "actions": store.AuditActions,
	})
}

func qInt(v string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

// auditRetentionDays 生效的保留天数；<=0 表示永久保留、不清理。
func (a *API) auditRetentionDays(ctx context.Context) int {
	n, err := strconv.Atoi(strings.TrimSpace(a.dynVal(ctx, "audit_retention_days")))
	if err != nil {
		return 0
	}
	return n
}

// purgeAudit 按当前保留策略清理一次超期审计记录。
func (a *API) purgeAudit(ctx context.Context) {
	days := a.auditRetentionDays(ctx)
	if days <= 0 {
		return
	}
	n, err := a.st.PurgeAudit(ctx, time.Now().AddDate(0, 0, -days))
	if err != nil {
		log.Printf("审计保留清理失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("审计保留清理: 删除 %d 条超过 %d 天的记录", n, days)
	}
}

// RunAuditRetention 启动时清一次，之后每小时一次（保留天数改了下一轮生效）。
func (a *API) RunAuditRetention(ctx context.Context) {
	a.purgeAudit(ctx)
	t := time.NewTicker(auditRetentionInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.purgeAudit(ctx)
		case <-ctx.Done():
			return
		}
	}
}
