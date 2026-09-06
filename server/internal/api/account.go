// 账号自助：登录会话列表与远程下线。
// 改密走已有的 /api/account/password（admin.go 的 updatePassword，成功后 DeleteOtherSessions
// 作废其它会话），这里只补会话面。
package api

import (
	"net/http"
	"sync"
	"time"

	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

// touchInterval 会话最近活跃时间的写库间隔：每个请求都写库太贵，
// 展示精度到分钟级就够，同一 token 在这个窗口内只写一次。
const touchInterval = 5 * time.Minute

// 节流表按 token 记上次写库时间。进程内状态，不放进 API 结构体：
// 它与实例配置无关，重启丢掉只是让每条会话多写一次库。
var touched = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// shouldTouch 该 token 是否到了写库刷新 last_seen 的时候。
func shouldTouch(token string) bool {
	now := time.Now()
	touched.Lock()
	defer touched.Unlock()
	if last, ok := touched.at[token]; ok && now.Sub(last) < touchInterval {
		return false
	}
	// 上限保底：会话过期后不会有人再来清这张表，超量时整表丢弃重来（只影响节流精度）
	if len(touched.at) > 10000 {
		touched.at = map[string]time.Time{}
	}
	touched.at[token] = now
	return true
}

// touchSession 中间件：节流刷新当前会话的最近活跃时间（挂在 auth 之后）。
func (a *API) touchSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := BearerToken(r); token != "" && shouldTouch(token) {
			// 刷新失败不影响请求：last_seen 只是展示用
			a.st.TouchSession(r.Context(), token)
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) listMySessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.st.ListSessions(r.Context(), userFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	cur := store.SessionID(BearerToken(r))
	for i := range sessions {
		sessions[i].Current = sessions[i].ID == cur
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// deleteMySession 下线自己的一条会话（含当前这条：等价于登出这台设备）。
func (a *API) deleteMySession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	removed, err := a.st.DeleteSessionByID(r.Context(), userFrom(r).ID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "该登录会话已不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
