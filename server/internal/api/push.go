// 离线推送（Web Push）与频道通知静音。
//
// 三条约束决定了这里的做法：
//  1. **只推被 @ 与被回复两种事件**：普通消息由页面自己的通知负责（页面开着才有），
//     离线推送的价值只在"有人点名找我"，普通消息推起来是骚扰。
//  2. **客户端传来的 mentions 只是线索，不是事实**：uid 由服务端逐个校验（用户存在 +
//     `@<其当前用户名>` 确实按边界规则出现在正文里），校验不过的丢弃。否则任何人都能
//     拿一串 uid 让服务器替他给任何人发通知。
//  3. **通道由浏览器决定，送不到就静默**：hearth 只按 VAPID 把密文交给订阅里的
//     endpoint（各厂商推送网关），不做任何厂商特判；404/410 说明地址作废即删订阅，
//     其它失败累计到上限再删。endpoint 与两把密钥从不进日志。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

const (
	mentionsMax      = 20  // 一条消息最多认这么多个提及（超出的不看）
	pushEndpointMax  = 512 // 与 push_subscriptions.endpoint 列宽一致（见 00008 迁移）
	pushKeyMax       = 255
	pushMaxFail      = 10               // 连续失败到这个数就删订阅
	pushSendTimeout  = 10 * time.Second // 整轮投递的预算（handler 已返回，走 WithoutCancel）
	pushPreviewRunes = 120
	pushTTL          = 600 // 推送网关最多替我们保留 10 分钟：过时的"有人@你"没有意义
)

// webpushKeys VAPID 身份：一对 P-256 密钥，浏览器订阅时按公钥绑定，之后只认这把私钥
// 签出来的推送。留空首次用到时生成并落库（随数据库备份），换密钥会让已有订阅全部失效。
var webpushKeys = []rtc.ConfigKey{
	{Name: "webpush_vapid_public", Group: "admin",
		Label: "Web Push 公钥 (VAPID)",
		Hint: "留空 = 首次用到时自动生成一对并落库。浏览器订阅时按这个公钥绑定，" +
			"改动会让所有已有的离线推送订阅立即失效（各设备下次打开页面时会自动重新订阅）"},
	{Name: "webpush_vapid_private", Group: "admin", Secret: true,
		Label: "Web Push 私钥 (VAPID)",
		Hint:  "与公钥配对，只用于给推送请求签名，不回显。删掉它等于让全部订阅失效"},
	{Name: "webpush_subject", Group: "admin",
		Label: "Web Push 联系地址",
		Hint: "写进 VAPID 断言的 sub，供推送网关在投递异常时联系站点管理员；" +
			"留空 = mailto:admin@<当前请求的 Host>。填也只填 mailto: 或 https: 形式"},
}

// ---- VAPID 密钥 ----

// ensureVAPIDKeys 取生效的密钥对，留空则生成一对落库（照 lkembed 密钥的先例）。
// 加锁是必需的：两个请求同时发现"没有密钥"会各生成一对，后写的那对让先返回给浏览器的
// 公钥所对应的订阅立刻作废。
func (a *API) ensureVAPIDKeys(ctx context.Context) (pub, priv string, err error) {
	a.webpushMu.Lock()
	defer a.webpushMu.Unlock()
	pub = a.dynVal(ctx, "webpush_vapid_public")
	priv = a.dynVal(ctx, "webpush_vapid_private")
	if pub != "" && priv != "" {
		return pub, priv, nil
	}
	priv, pub, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", err
	}
	if err = a.st.SetSetting(ctx, "cfg_webpush_vapid_public", pub); err != nil {
		return "", "", err
	}
	if err = a.st.SetSetting(ctx, "cfg_webpush_vapid_private", priv); err != nil {
		return "", "", err
	}
	return pub, priv, nil
}

// pushSubject VAPID 断言里的 sub：配置了就用，否则按当前请求的 Host 拼一个
// （推送网关只把它当联系方式，不校验可达性）。
func (a *API) pushSubject(ctx context.Context, host string) string {
	if v := strings.TrimSpace(a.dynVal(ctx, "webpush_subject")); v != "" {
		return v
	}
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	if host == "" {
		host = "localhost"
	}
	return "mailto:admin@" + host
}

// ---- 端点 ----

// pushVAPID GET /api/push/vapid 前端订阅前要拿的公钥。
func (a *API) pushVAPID(w http.ResponseWriter, r *http.Request) {
	pub, _, err := a.ensureVAPIDKeys(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": pub})
}

// pushSubscribe POST /api/push/subscribe 记下浏览器给的订阅，绑当前会话
// （会话下线即删，见 store.DeleteSession）。同一 endpoint 重复提交是刷新，不是新增。
func (a *API) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if !decode(w, r, &req) {
		return
	}
	endpoint := strings.TrimSpace(req.Endpoint)
	p256dh := strings.TrimSpace(req.Keys.P256dh)
	auth := strings.TrimSpace(req.Keys.Auth)
	if endpoint == "" || p256dh == "" || auth == "" {
		writeErr(w, http.StatusBadRequest, "订阅信息不完整")
		return
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		writeErr(w, http.StatusBadRequest, "推送地址无效")
		return
	}
	if len(endpoint) > pushEndpointMax || len(p256dh) > pushKeyMax || len(auth) > pushKeyMax {
		writeErr(w, http.StatusBadRequest, "推送地址或密钥过长")
		return
	}
	sessionID := store.SessionID(BearerToken(r))
	if err := a.st.SavePushSubscription(r.Context(), userFrom(r).ID, sessionID, endpoint, p256dh, auth, r.UserAgent()); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushUnsubscribe DELETE /api/push/subscribe 退订（限本人的订阅）。
func (a *API) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := a.st.DeletePushSubscription(r.Context(), userFrom(r).ID, strings.TrimSpace(req.Endpoint)); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// muteChannel PUT /api/channels/{channel}/mute 静音该频道的提醒（本人视角的开关，
// 与 POST /mute 的禁言管制无关：那是对别人的，这是对自己的）。
func (a *API) muteChannel(w http.ResponseWriter, r *http.Request) {
	if err := a.st.MuteChannel(r.Context(), userFrom(r).ID, channelFrom(r).ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unmuteChannel DELETE /api/channels/{channel}/mute 取消静音。
func (a *API) unmuteChannel(w http.ResponseWriter, r *http.Request) {
	if err := a.st.UnmuteChannel(r.Context(), userFrom(r).ID, channelFrom(r).ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- 提及校验 ----

// 与前端 chat/mentions.ts 的两组边界字符一致（改一边必须改另一边，否则同一条消息在
// 页面里算被@、服务端却不推）：@ 前只允许空白或开括号，@名字 后只允许空白或标点。
var (
	mentionLeadingRunes  = []rune{'(', '（', '[', '【'}
	mentionBoundaryRunes = []rune{',', '.', ';', ':', '!', '?', '，', '。', '；', '：', '！', '？', '、', '"', '\'', '）', ')', ']', '】'}
)

func inRunes(set []rune, r rune) bool {
	for _, x := range set {
		if x == r {
			return true
		}
	}
	return false
}

// mentionsUsername 正文里是否真的按边界规则提到了这个名字。
// 边界是关键：用户名字符集含 `-`，裸子串匹配会让 @abc 命中用户 ab。
func mentionsUsername(content, username string) bool {
	if username == "" {
		return false
	}
	needle := "@" + username
	for i := 0; i+len(needle) <= len(content); {
		j := strings.Index(content[i:], needle)
		if j < 0 {
			return false
		}
		at := i + j
		okPrev := at == 0
		if !okPrev {
			prev, _ := utf8.DecodeLastRuneInString(content[:at])
			okPrev = unicode.IsSpace(prev) || inRunes(mentionLeadingRunes, prev)
		}
		rest := content[at+len(needle):]
		okNext := rest == ""
		if !okNext {
			next, _ := utf8.DecodeRuneInString(rest)
			okNext = unicode.IsSpace(next) || inRunes(mentionBoundaryRunes, next)
		}
		if okPrev && okNext {
			return true
		}
		i = at + 1
	}
	return false
}

// validMentions 校验客户端报来的提及：用户存在且正文里确实提到了他当前的用户名。
// 这是条安全边界——不校验就等于给所有人开了个"替我通知任意用户"的接口。
func (a *API) validMentions(ctx context.Context, content string, uids []int64) []int64 {
	if len(uids) > mentionsMax {
		uids = uids[:mentionsMax]
	}
	out := []int64{}
	seen := map[int64]bool{}
	for _, uid := range uids {
		if uid <= 0 || seen[uid] {
			continue
		}
		seen[uid] = true
		u, err := a.st.UserByID(ctx, uid)
		if err != nil || u == nil || !mentionsUsername(content, u.Username) {
			continue
		}
		out = append(out, uid)
	}
	return out
}

// ---- 推送判定与投递 ----

// pushPayload 推给浏览器的载荷（service-worker.js 直接按这个形状取字段）。
type pushPayload struct {
	Kind      string `json:"kind"` // mention | reply
	Channel   string `json:"channel"`
	ChannelID int64  `json:"channel_id"`
	MessageID int64  `json:"message_id"`
	From      string `json:"from"`
	Preview   string `json:"preview"`
}

// pushTargets 该推给谁：校验过的 mentions ∪ 被回复者，去掉发送者自己、去掉对该频道
// 静音的人。返回 uid → 事件类型（被回复优先于被@：点名回复比顺带提一句更具体）。
func (a *API) pushTargets(ctx context.Context, m *store.Message, mentions []int64) (map[int64]string, error) {
	targets := map[int64]string{}
	for _, uid := range mentions {
		targets[uid] = "mention"
	}
	if m.ReplyTo != nil {
		target, err := a.st.MessageByID(ctx, m.ChannelID, *m.ReplyTo)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if target != nil && target.UserID > 0 {
			targets[target.UserID] = "reply"
		}
	}
	delete(targets, m.UserID)
	if len(targets) == 0 {
		return targets, nil
	}
	muted, err := a.st.MutedUsersOf(ctx, m.ChannelID)
	if err != nil {
		return nil, err
	}
	for uid := range muted {
		delete(targets, uid)
	}
	return targets, nil
}

// notifyMessage 发一轮离线推送：判定目标 → 每个目标的每条订阅各发一条。
// 调用方另起 goroutine（handler 返回后 r.Context() 会被取消，这里用 WithoutCancel + 超时）。
func (a *API) notifyMessage(ctx context.Context, channel string, m *store.Message, mentions []int64, host string) {
	targets, err := a.pushTargets(ctx, m, mentions)
	if err != nil {
		log.Printf("推送目标判定失败: %v", err)
		return
	}
	if len(targets) == 0 {
		return
	}
	uids := make([]int64, 0, len(targets))
	for uid := range targets {
		uids = append(uids, uid)
	}
	subs, err := a.st.SubscriptionsOf(ctx, uids)
	if err != nil {
		log.Printf("推送订阅查询失败: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	pub, priv, err := a.ensureVAPIDKeys(ctx)
	if err != nil {
		log.Printf("推送密钥不可用: %v", err)
		return
	}
	subject := a.pushSubject(ctx, host)
	preview := m.Content
	if m.Kind == store.KindFile && m.File != nil {
		preview = m.File.Name
	}
	preview = truncateUTF8(preview, pushPreviewRunes)
	for _, sub := range subs {
		raw, err := json.Marshal(pushPayload{
			Kind: targets[sub.UserID], Channel: channel, ChannelID: m.ChannelID,
			MessageID: m.ID, From: m.Username, Preview: preview,
		})
		if err != nil {
			continue
		}
		a.deliverPush(ctx, sub, raw, pub, priv, subject)
	}
}

// deliverPush 投一条。404/410 = 这个地址已作废，立刻删；其它失败累计到上限再删
// （网关抖动不该让用户丢订阅）。日志只记 uid 与状态码，endpoint 与密钥不出现。
func (a *API) deliverPush(ctx context.Context, sub store.PushSubscription, payload []byte, pub, priv, subject string) {
	opts := &webpush.Options{
		Subscriber:      subject,
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		TTL:             pushTTL,
		HTTPClient:      a.pushHTTPClient(),
	}
	res, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, opts)
	code := 0
	if res != nil {
		code = res.StatusCode
		io.Copy(io.Discard, res.Body) // 连接复用要求把响应体读完
		res.Body.Close()
	}
	switch {
	case err != nil:
		log.Printf("推送投递失败: uid=%d err=%v", sub.UserID, err)
	case code == http.StatusNotFound || code == http.StatusGone:
		if err := a.st.DeletePushSubscriptionByID(ctx, sub.ID); err != nil {
			log.Printf("推送订阅删除失败: uid=%d err=%v", sub.UserID, err)
		}
		return
	case code >= 200 && code < 300:
		if err := a.st.TouchPushSubscription(ctx, sub.ID); err != nil {
			log.Printf("推送订阅刷新失败: uid=%d err=%v", sub.UserID, err)
		}
		return
	default:
		log.Printf("推送投递失败: uid=%d status=%d", sub.UserID, code)
	}
	n, err := a.st.IncPushFailure(ctx, sub.ID)
	if err != nil {
		log.Printf("推送失败计数更新失败: uid=%d err=%v", sub.UserID, err)
		return
	}
	if n >= pushMaxFail {
		if err := a.st.DeletePushSubscriptionByID(ctx, sub.ID); err != nil {
			log.Printf("推送订阅删除失败: uid=%d err=%v", sub.UserID, err)
		}
	}
}

// pushHTTPClient 投递用的 HTTP 客户端（测试注入假网关；默认带超时，不用共享的默认客户端）。
func (a *API) pushHTTPClient() webpush.HTTPClient {
	if a.pushHTTP != nil {
		return a.pushHTTP
	}
	return &http.Client{Timeout: pushSendTimeout}
}

// pushNotifyAsync 消息落库后的推送入口：不阻塞响应，也不受 handler 生命周期约束
// （websocket 之外同一个坑：handler 返回后 r.Context() 就被取消了）。
func (a *API) pushNotifyAsync(r *http.Request, channel string, m *store.Message, mentions []int64) {
	if len(mentions) == 0 && m.ReplyTo == nil {
		return // 既没点名也不是回复：不推
	}
	host := r.Host
	msg := *m
	// WithoutCancel 必须在 handler 还没返回时取：返回后 r.Context() 已被 net/http 取消
	base := context.WithoutCancel(r.Context())
	go func() {
		ctx, cancel := context.WithTimeout(base, pushSendTimeout)
		defer cancel()
		a.notifyMessage(ctx, channel, &msg, mentions, host)
	}()
}
