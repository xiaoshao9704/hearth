// 聊天消息：hearth 只做落库与历史回放，实时扇出由前端经内核数据通道自己广播
// （POST 成功后拿返回的 Message 广播出去，接收方按 id 去重）。因此这里没有长连接、
// 没有推送，权威始终在库里——数据通道断了也不丢消息，重连时 after= 补齐即可。
// 文件消息只落"卡片"（名字/类型/大小），字节一律不经 hearth。
package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"hearth/server/internal/store"
)

const (
	messagesDefaultLimit = 50
	messagesMaxLimit     = 200
	messageMaxRunes      = 2000
	fileNameMaxRunes     = 200
	fileMimeMaxLen       = 128
)

// fileMimeRE type/subtype，字符集取 RFC 6838 的 token 常见子集；只做形状校验，
// 不维护白名单——是否内联渲染由前端按自己的白名单决定。
var fileMimeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,63}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,63}$`)

// chatFileLimit 文件大小上限（字节）。配置项以 MB 计，填非法值时回落默认。
func (a *API) chatFileLimit(r *http.Request) int64 {
	mb, err := strconv.ParseInt(strings.TrimSpace(a.dynVal(r.Context(), "chat_file_max_mb")), 10, 64)
	if err != nil || mb <= 0 {
		mb = 25
	}
	return mb << 20
}

// listMessages GET /api/channels/{channel}/messages?after=&limit=
// after 缺省/0 = 最近 limit 条；否则只回 id 更大的（重连补齐）。
func (a *API) listMessages(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	if _, ok := a.admitChat(w, r); !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := messagesDefaultLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, messagesMaxLimit)
	}
	msgs, err := a.st.MessagesAfter(r.Context(), c.ID, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// postMessage POST /api/channels/{channel}/messages
// body: {content} 或 {kind:"file", file:{name,mime,size}}；返回落库后的 Message（含 id）。
func (a *API) postMessage(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	adm, ok := a.admitChat(w, r)
	if !ok {
		return
	}
	// 禁言 = 禁全部发布，数据通道也在内（进房票的 CanPublishData 同源），这里是同一判定的 HTTP 侧
	if !adm.CanPublish {
		writeErr(w, http.StatusForbidden, "你已被禁言，无法发言")
		return
	}
	var req struct {
		Kind    string             `json:"kind"`
		Content string             `json:"content"`
		File    *store.MessageFile `json:"file"`
	}
	if !decode(w, r, &req) {
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = store.KindText
	}
	var content string
	var file *store.MessageFile
	switch kind {
	case store.KindText:
		content = strings.TrimSpace(req.Content)
		if content == "" {
			writeErr(w, http.StatusBadRequest, "消息内容为空")
			return
		}
		if utf8.RuneCountInString(content) > messageMaxRunes {
			writeErr(w, http.StatusBadRequest, "消息过长")
			return
		}
	case store.KindFile:
		if req.File == nil {
			writeErr(w, http.StatusBadRequest, "缺少文件信息")
			return
		}
		name := strings.TrimSpace(strings.Join(strings.Fields(req.File.Name), " "))
		if name == "" {
			writeErr(w, http.StatusBadRequest, "文件名为空")
			return
		}
		name = truncateUTF8(name, fileNameMaxRunes)
		mime := strings.TrimSpace(req.File.Mime)
		if mime == "" {
			mime = "application/octet-stream"
		}
		if len(mime) > fileMimeMaxLen || !fileMimeRE.MatchString(mime) {
			writeErr(w, http.StatusBadRequest, "文件类型无效")
			return
		}
		if req.File.Size <= 0 {
			writeErr(w, http.StatusBadRequest, "文件大小无效")
			return
		}
		if req.File.Size > a.chatFileLimit(r) {
			writeErr(w, http.StatusRequestEntityTooLarge, "文件超过服务器允许的大小上限")
			return
		}
		file = &store.MessageFile{Name: name, Mime: mime, Size: req.File.Size}
	default:
		writeErr(w, http.StatusBadRequest, "消息类型无效")
		return
	}
	msg, err := a.st.AddMessage(r.Context(), c.ID, u.ID, kind, content, file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// admitChat 聊天读写共用的入场判定（封禁/邀请制/禁言，与进房同一套规则）。
// 拒绝时响应已写好。
func (a *API) admitChat(w http.ResponseWriter, r *http.Request) (admission, bool) {
	adm, ok, reason, err := a.admitUser(r.Context(), channelFrom(r), userFrom(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return admission{}, false
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return admission{}, false
	}
	return adm, true
}
