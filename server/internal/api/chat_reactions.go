// 消息表情反应：PUT 加、DELETE 取消，两端都返回该消息最新的聚合结果。
// 实时同步不靠这里——前端拿到 200 后经数据通道广播一条 reaction 信封，
// 权威仍在库里（重连时 after= 拉回来的历史带着完整聚合）。
package api

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// reactionEmojis 允许的表情固定集合。收窄到一小把是刻意的：
// 存的是字符串主键，放开任意输入等于把无界文本塞进索引列，还得防伪装成表情的长串。
var reactionEmojis = map[string]bool{
	"👍": true, "❤️": true, "😂": true, "😮": true,
	"😢": true, "🔥": true, "👀": true, "🎉": true,
}

// emojiParam 取路径里的表情。chi 在 URL 有转义时按 RawPath 路由，取到的段仍是百分号编码，
// 必须自己解一次（前端 encodeURIComponent 过）。
func emojiParam(r *http.Request) string {
	raw := chi.URLParam(r, "emoji")
	if s, err := url.PathUnescape(raw); err == nil {
		return s
	}
	return raw
}

// putReaction PUT /api/channels/{channel}/messages/{msgID}/reactions/{emoji}
func (a *API) putReaction(w http.ResponseWriter, r *http.Request) {
	a.mutateReaction(w, r, true)
}

// deleteReaction DELETE /api/channels/{channel}/messages/{msgID}/reactions/{emoji}
func (a *API) deleteReaction(w http.ResponseWriter, r *http.Request) {
	a.mutateReaction(w, r, false)
}

// mutateReaction 加/取消一次反应。点反应属于"发言"，与发消息同一道禁言判定；
// 已撤回的消息不接受反应（它没有内容可反应了）。
func (a *API) mutateReaction(w http.ResponseWriter, r *http.Request, on bool) {
	u := userFrom(r)
	adm, ok := a.admitChat(w, r)
	if !ok {
		return
	}
	if !adm.CanPublish {
		writeErr(w, http.StatusForbidden, "你已被禁言，无法发言")
		return
	}
	emoji := emojiParam(r)
	if !reactionEmojis[emoji] {
		writeErr(w, http.StatusBadRequest, "不支持这个表情")
		return
	}
	m := a.lookupMessage(w, r)
	if m == nil {
		return
	}
	if m.Deleted {
		writeErr(w, http.StatusBadRequest, "消息已被撤回")
		return
	}
	var err error
	if on {
		err = a.st.AddReaction(r.Context(), m.ID, u.ID, emoji)
	} else {
		err = a.st.RemoveReaction(r.Context(), m.ID, u.ID, emoji)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	list, err := a.st.ReactionsOf(r.Context(), m.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": m.ID, "reactions": list})
}
