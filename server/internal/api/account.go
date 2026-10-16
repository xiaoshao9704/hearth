// 账号自助：改名/改密/设备与登录会话列表、远程下线。
package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
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

