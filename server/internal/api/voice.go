// /providers/ember/voice：ember 语音内核的信令 WebSocket 入口。
// 鉴权与聊天 WS 同模式（浏览器 WS 设不了请求头，token 走 query）；
// 入场判定不在此重做——joinToken 已判定并签发一次性入场票（见 admission.go），
// 这里验票后把连接交给 ember.Provider 处理（阻塞至连接结束）。
package api

import (
	"context"
	"log"
	"net/http"

	"nhooyr.io/websocket"
)

func (a *API) voiceWS(w http.ResponseWriter, r *http.Request) {
	// 与 joinToken 的签发口径一致：按 voiceInstance 的解析结果判定
	//（选择器取未知值时语音回落 ember，签的就是 ember 票，不能按原始选择器串拒）
	if alias, _ := a.voiceInstance(r.Context()); alias != TypeEmber {
		http.Error(w, "语音内核未启用 ember", http.StatusConflict)
		return
	}
	u, err := a.st.UserByToken(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		http.Error(w, "登录已失效", http.StatusUnauthorized)
		return
	}
	// 验一次性入场票：取出即删，过期/重复使用都拒绝
	tk, ok := a.takeVoiceTicket(r.URL.Query().Get("ticket"))
	if !ok {
		http.Error(w, "入场票无效或已过期，请重新进房", http.StatusForbidden)
		return
	}
	// 票据与持有者/频道比对，防票据挪用
	if tk.userID != u.ID || tk.room != r.URL.Query().Get("channel") {
		http.Error(w, "入场票与身份不符", http.StatusForbidden)
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
	log.Printf("ember 入会: room=%s identity=%s", tk.room, tk.identity)
	// hijack 后 r.Context() 会被 net/http 取消，连接生命周期用独立 context
	a.ember.HandleJoin(context.WithoutCancel(r.Context()), tk.room, tk.identity, tk.name, tk.muted, conn)
}
