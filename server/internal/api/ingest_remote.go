// 远端 Bellows（cmd/bellows，通常跑在 LiveKit 同一局域网）回调 hearth 的内部接口：
// 按推流密钥反查归属并做入场判定。远端进程没有数据库，只问这里，
// 「谁能推流」的决策仍只在 admitUser 一处。
package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"hearth/server/internal/store"
)

// ingestResolve GET /api/internal/ingest/resolve?key=…，Authorization: Bearer <bellows_shared_secret>。
// 200 {room, username}；401 密钥不对；403 不许推流；404 key 不存在；503 未配置共享密钥。
func (a *API) ingestResolve(w http.ResponseWriter, r *http.Request) {
	secret := a.dynVal(r.Context(), "bellows_shared_secret")
	if secret == "" {
		writeErr(w, http.StatusServiceUnavailable, "未配置远端 Bellows 共享密钥")
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		writeErr(w, http.StatusUnauthorized, "共享密钥不匹配")
		return
	}
	c, u, err := a.ingressOwner(r.Context(), r.URL.Query().Get("key"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "推流密钥无效或已重置")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	adm, ok, reason, err := a.admitUser(r.Context(), c, u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return
	}
	if !adm.CanPublish {
		writeErr(w, http.StatusForbidden, "你已被禁言，无法推流")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"room": c.Name, "username": u.Username})
}
