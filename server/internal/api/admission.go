// 入场判定统一：谁能进房、能否发布（封禁/邀请制/禁言）收敛到本文件，
// joinToken、ember 信令（/providers/ember/voice）与 WHIP 推流（/w POST 的 admitIngest）
// 三个入口共用同一套判定。
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// admission 入场判定结果。身份一律以 user_id 为准（identity 由 rtc.Identity 组），
// 用户名只作为展示信息随参与者元数据下发。
type admission struct {
	UID        int64  // 参与者归属用户 id
	Username   string // 展示名
	CanPublish bool   // 能否发布（未被禁言）
}

// admitUser 统一入场判定：封禁/邀请制决定能否进入，禁言决定能否发布。
// 返回 (判定结果, 是否允许进入, 拒绝原因, 内部错误)。
func (a *API) admitUser(ctx context.Context, c *store.Channel, u *store.User) (admission, bool, string, error) {
	ok, reason, err := a.st.CanJoin(ctx, c, u.ID)
	if err != nil {
		return admission{}, false, "", err
	}
	if !ok {
		return admission{}, false, reason, nil
	}
	gagged, err := a.st.IsGagged(ctx, c.ID, u.ID)
	if err != nil {
		return admission{}, false, "", err
	}
	return admission{UID: u.ID, Username: u.Username, CanPublish: !gagged}, true, "", nil
}

// ---- ember 线一次性入场票 ----
// joinToken 完成入场判定后签发，ember 信令入口凭票直接入会，不再二次判定。

const voiceTicketTTL = 60 * time.Second

type voiceTicket struct {
	room    string   // 频道名（与信令 URL 的 channel 参数比对）
	meta    rtc.Meta // 入会身份与展示信息（joinToken 已组好，ember 据此建 identity）
	userID  int64    // 持票人（与信令入口的会话用户比对，防票据挪用）
	muted   bool     // 入会即禁言
	expires time.Time
}

// issueVoiceTicket 签发一次性入场票，返回票号（拼进信令 URL query）。
func (a *API) issueVoiceTicket(t voiceTicket) string {
	t.expires = time.Now().Add(voiceTicketTTL)
	nonce := randHex(16)
	a.ticketMu.Lock()
	defer a.ticketMu.Unlock()
	a.cleanTicketsLocked()
	a.tickets[nonce] = t
	return nonce
}

// takeVoiceTicket 取票（一次性：取出即删）；票不存在或已过期返回 false。
func (a *API) takeVoiceTicket(nonce string) (voiceTicket, bool) {
	a.ticketMu.Lock()
	defer a.ticketMu.Unlock()
	a.cleanTicketsLocked()
	t, ok := a.tickets[nonce]
	if ok {
		delete(a.tickets, nonce)
	}
	return t, ok
}

// cleanTicketsLocked 惰性清理过期票（须持有 ticketMu，签发/取票时顺带执行）。
func (a *API) cleanTicketsLocked() {
	now := time.Now()
	for k, t := range a.tickets {
		if now.After(t.expires) {
			delete(a.tickets, k)
		}
	}
}

// ---- 推流入场判定（/w POST）----

// ingestCtxKey 推流判定结果的传递：admitIngest 在 serveWHIP 做完后把组好的
// 身份四元组挂到请求 ctx，ingressResolver（ResolveFunc 只有令牌参数，频道在 URL 里
// 由接入层解析）原样取回。
type ingestCtxKey struct{}

// ingestAdmission 推流入场判定结果：Room=频道名（=内核房间名），Identity 见 rtc.Identity，
// Meta 是随参与者下发的元数据（前端据 meta.uid 认人、meta.username 展示）。
type ingestAdmission struct {
	Room     string
	Identity string
	Meta     rtc.Meta
}

// admitIngest 统一推流入场判定（替代旧 canPublishByStreamKey 与 admitWhipRemote）。
// 推流不再是独立选择器：URL 的 alias 段必须是当前舞台实例（stage_provider 选中的那个，
// 推进别的实例观众看不到），且该实例有 WHIP 推流能力，否则 definitive 404——
// 内建 bellows 等旧形态因此天然 404。
// 其后：令牌反查用户 + URL 取频道 → admitUser（封禁/邀请制/禁言）。全部 definitive：
// 令牌不存在 404、频道不存在 404、不许推（封禁/邀请制/禁言/账号停用）403、查询出错 503——
// 不再有 fail-open：上游收到的已是 hearth 出示的实例凭证，不再承担鉴权。
// 返回 false 时响应已写好。
func (a *API) admitIngest(ctx context.Context, w http.ResponseWriter, alias, channel, token string) (ingestAdmission, bool) {
	stageAlias, sp := a.stageInstance(ctx)
	inst := a.instance(alias)
	if sp == nil || alias != stageAlias || inst == nil || inst.Ingest == nil {
		writeErr(w, http.StatusNotFound, "推流入口不存在（OBS 请指向当前舞台内核实例的地址）")
		return ingestAdmission{}, false
	}
	if token == "" {
		writeErr(w, http.StatusBadRequest, "缺少推流令牌")
		return ingestAdmission{}, false
	}
	fail := func(err error) (ingestAdmission, bool) {
		log.Printf("推流入场判定查询失败（实例 %s）: %v", alias, err)
		writeErr(w, http.StatusServiceUnavailable, "推流入口暂时不可用，请稍后再试")
		return ingestAdmission{}, false
	}
	it, err := a.st.IngestTokenByToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "推流令牌无效或已重置")
		return ingestAdmission{}, false
	}
	if err != nil {
		return fail(err)
	}
	u, err := a.st.UserByID(ctx, it.UserID)
	if errors.Is(err, store.ErrNotFound) { // 用户已删除，令牌等同失效
		writeErr(w, http.StatusNotFound, "推流令牌无效或已重置")
		return ingestAdmission{}, false
	}
	if err != nil {
		return fail(err)
	}
	c, err := a.st.ChannelByName(ctx, channel)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return ingestAdmission{}, false
	}
	if err != nil {
		return fail(err)
	}
	if u.Disabled {
		writeErr(w, http.StatusForbidden, "账号已被停用")
		return ingestAdmission{}, false
	}
	adm, ok, reason, err := a.admitUser(ctx, c, u)
	if err != nil {
		return fail(err)
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return ingestAdmission{}, false
	}
	if !adm.CanPublish {
		writeErr(w, http.StatusForbidden, "你已被禁言，无法推流")
		return ingestAdmission{}, false
	}
	return ingestAdmission{
		Room:     c.Name,
		Identity: rtc.Identity(u.ID, it.Tag),
		Meta:     rtc.Meta{UID: u.ID, Username: u.Username, Kind: "ingest", Tag: it.Tag},
	}, true
}
