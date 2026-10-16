// REST API：注册/登录/频道/进房令牌/频道管理。路由用 chi，鉴权用 Bearer token。
package api

import (
	"net/http"
	"regexp"

	"hearth/server/internal/store"

	"golang.org/x/crypto/bcrypt"
)

// ---- 注册 / 登录 ----

type credReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authResp struct {
	Token string      `json:"token"`
	User  *store.User `json:"user"`
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,32}$`)

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var req credReq
	if !decode(w, r, &req) {
		return
	}
	u, hash, err := a.st.UserByName(r.Context(), req.Username)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if u.Disabled {
		writeErr(w, http.StatusForbidden, "账号已被停用，请联系管理员")
		return
	}
	a.issueSession(w, r, u)
}

func (a *API) issueSession(w http.ResponseWriter, r *http.Request, u *store.User) {
	token, err := a.st.CreateSessionWithUA(r.Context(), u.ID, r.UserAgent())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, authResp{Token: token, User: u})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	a.st.DeleteSession(r.Context(), BearerToken(r))
	w.WriteHeader(http.StatusNoContent)
}

// me 当前用户 + passkey_count（登录后推荐卡片据此判断「这个账号还没有通行密钥」，
// 免得前端为一个计数再打一次列表接口）+ can_claim（转正入口的显隐依据，见 canClaimGuest）。
// 计数查不到按 0 处理，不让 /api/me 因此失败。
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	n, _ := a.st.CountPasskeys(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, struct {
		*store.User
		PasskeyCount int  `json:"passkey_count"`
		CanClaim     bool `json:"can_claim"`
	}{u, n, a.canClaimGuest(r.Context(), u)})
}
