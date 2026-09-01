// /api/voice：pion 语音内核的信令 WebSocket 入口。
// 鉴权与聊天 WS 同模式（浏览器 WS 设不了请求头，token 走 query），
// 通过后把连接交给 pionvoice.Provider 处理（阻塞至连接结束）。
package api

import (
	"context"
	"log"
	"net/http"

	"nhooyr.io/websocket"
)

func (a *API) voiceWS(w http.ResponseWriter, r *http.Request) {
	if a.dynVal(r.Context(), "voice_provider") != "pion" {
		http.Error(w, "语音内核未启用 pion", http.StatusConflict)
		return
	}
	u, err := a.st.UserByToken(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		http.Error(w, "登录已失效", http.StatusUnauthorized)
		return
	}
	ch, err := a.st.ChannelByName(r.Context(), r.URL.Query().Get("channel"))
	if err != nil {
		http.Error(w, "频道不存在", http.StatusNotFound)
		return
	}
	ok, reason, err := a.st.CanJoin(r.Context(), ch, u.ID)
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	// 设备标签与 LiveKit 线同规则，identity 两条线可对齐
	tag := a.deviceTagFor(r, r.URL.Query().Get("device_id"), u.ID)
	identity := u.Username + "-" + tag

	// 禁言随入会生效：joinToken 的 canPublish 对进程内内核无处安放，在此等效拦截
	gagged, err := a.st.IsGagged(r.Context(), ch.ID, u.ID)
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}

	originPatterns := []string{a.cfg.CORSOrigin}
	if a.cfg.CORSOrigin == "*" {
		originPatterns = nil
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:     originPatterns,
		InsecureSkipVerify: a.cfg.CORSOrigin == "*",
	})
	if err != nil {
		return
	}
	log.Printf("pion-voice 入会: room=%s identity=%s", ch.Name, identity)
	// hijack 后 r.Context() 会被 net/http 取消，连接生命周期用独立 context
	a.pion.HandleJoin(context.WithoutCancel(r.Context()), ch.Name, identity, u.Username, gagged, conn)
}
