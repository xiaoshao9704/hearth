// 聊天消息：hearth 只做落库与历史回放，实时扇出由前端经内核数据通道自己广播
// （POST 成功后拿返回的 Message 广播出去，接收方按 id 去重）。因此这里没有长连接、
// 没有推送，权威始终在库里——数据通道断了也不丢消息，重连时 after= 补齐即可。
// 文件消息只落"卡片"（名字/类型/大小），字节一律不经 hearth。
package api

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"hearth/server/internal/perm"
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
	// 反应整批一次查询：历史一屏几十条，按条查会把一次拉取放大成几十次往返
	if err := a.st.AttachReactions(r.Context(), msgs); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
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
		ReplyTo *int64             `json:"reply_to"`
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
	replyTo, ok := a.resolveReplyTo(w, r, req.ReplyTo)
	if !ok {
		return
	}
	msg, err := a.st.AddMessage(r.Context(), c.ID, u.ID, kind, content, file, replyTo)
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

// resolveReplyTo 校验引用回复的目标：必须是同频道存在且未撤回的消息，否则 400。
// 不做"引用链"检查——被引消息自己引用了谁与本条无关，前端只渲染一层。
func (a *API) resolveReplyTo(w http.ResponseWriter, r *http.Request, id *int64) (*int64, bool) {
	if id == nil {
		return nil, true
	}
	if *id <= 0 {
		writeErr(w, http.StatusBadRequest, "引用的消息不存在")
		return nil, false
	}
	target, err := a.st.MessageByID(r.Context(), channelFrom(r).ID, *id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "引用的消息不存在")
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return nil, false
	}
	if target.Deleted {
		writeErr(w, http.StatusBadRequest, "引用的消息已被撤回")
		return nil, false
	}
	return id, true
}

// messageIDParam 取路径里的消息 id；形状不对返回 0（调用方按 404 处理）。
func messageIDParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "msgID"), 10, 64)
	return id
}

// lookupMessage 取同频道的一条消息，顺带把 404/500 的响应写好（返回 nil 即已响应）。
func (a *API) lookupMessage(w http.ResponseWriter, r *http.Request) *store.Message {
	id := messageIDParam(r)
	if id <= 0 {
		writeErr(w, http.StatusNotFound, "消息不存在")
		return nil
	}
	m, err := a.st.MessageByID(r.Context(), channelFrom(r).ID, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "消息不存在")
		return nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return nil
	}
	return m
}

// deleteMessage DELETE /api/channels/{channel}/messages/{msgID}
// 作者本人撤回，或频道 moderator/owner 删除；软删（内容清空、行保留）。
// 已删的再删按成功返回：撤回是幂等动作，两端同时点不该有一边报错。
func (a *API) deleteMessage(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	// 禁言的人仍可撤回自己已发出的消息：禁言约束的是"发"，不是"收回"
	if _, ok := a.admitChat(w, r); !ok {
		return
	}
	m := a.lookupMessage(w, r)
	if m == nil {
		return
	}
	byMod := m.UserID != u.ID
	if byMod {
		cr, err := perm.ChannelRole(r.Context(), a.st, c, u)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "内部错误")
			return
		}
		if !perm.ChannelAtLeast(cr, store.ChannelRoleModerator) {
			writeErr(w, http.StatusForbidden, "只能撤回自己发的消息")
			return
		}
	}
	done, err := a.st.SoftDeleteMessage(r.Context(), c.ID, m.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 只记版主删他人消息：自己撤回自己的不是管制动作
	if done && byMod {
		a.audit(r.Context(), u.ID, store.AuditMessageDel, m.UserID, c.ID,
			"删除消息 #"+strconv.FormatInt(m.ID, 10))
	}
	w.WriteHeader(http.StatusNoContent)
}

// clearMessages DELETE /api/channels/{channel}/messages
// 清空整个频道的聊天记录（连同表情反应）；频道 owner 与系统 admin+（隐含 owner）可用。
func (a *API) clearMessages(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	u := userFrom(r)
	cr, err := perm.ChannelRole(r.Context(), a.st, c, u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !perm.ChannelAtLeast(cr, store.ChannelRoleOwner) {
		writeErr(w, http.StatusForbidden, "只有频道主能清空聊天记录")
		return
	}
	n, err := a.st.ClearChannelMessages(r.Context(), c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.audit(r.Context(), u.ID, store.AuditChannelClear, 0, c.ID,
		"清空聊天记录 "+strconv.FormatInt(n, 10)+" 条")
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

// ---- 保留策略 ----

const chatRetentionInterval = time.Hour

// chatRetentionDays 保留天数；<=0（含填了非法值）= 永久保留，不清理。
func (a *API) chatRetentionDays(ctx context.Context) int {
	d, err := strconv.Atoi(strings.TrimSpace(a.dynVal(ctx, "chat_retention_days")))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// PurgeExpiredMessages 按当前保留天数删一次过期消息，返回删掉的条数
// （0 = 策略关着、清理失败或没有过期的）。
func (a *API) PurgeExpiredMessages(ctx context.Context) int64 {
	days := a.chatRetentionDays(ctx)
	if days <= 0 {
		return 0
	}
	n, err := a.st.PurgeMessagesBefore(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		log.Printf("聊天保留策略清理失败: %v", err)
		return 0
	}
	if n > 0 {
		log.Printf("聊天保留策略: 清理超过 %d 天的消息 %d 条", days, n)
	}
	return n
}

// RunChatRetention 启动时清一次，之后每小时一次；保留天数改了下一轮即生效（每轮重读配置）。
func (a *API) RunChatRetention(ctx context.Context) {
	a.PurgeExpiredMessages(ctx)
	t := time.NewTicker(chatRetentionInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.PurgeExpiredMessages(ctx)
		case <-ctx.Done():
			return
		}
	}
}
