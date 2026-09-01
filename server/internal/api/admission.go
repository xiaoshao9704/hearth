// 入场判定统一：谁能进房、能否发布（封禁/邀请制/禁言）收敛到本文件，
// joinToken、/api/voice 信令、/w 推流拦截三个入口共用同一套判定。
package api

import (
	"context"
	"time"

	"hearth/server/internal/store"
)

// admission 入场判定结果。
type admission struct {
	Identity   string // 参与者用户名（内核侧 identity 前缀）
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
	return admission{Identity: u.Username, CanPublish: !gagged}, true, "", nil
}

// canPublishByStreamKey /w 推流拦截专用的按 streamKey 判定（此处只有 key，没有用户/频道对象）。
// 语义保持 fail-open：查不到 key 或判定出错时放行代理，仅确定被禁言时拒绝。
func (a *API) canPublishByStreamKey(ctx context.Context, streamKey string) bool {
	userID, channelID, err := a.st.IngressOwner(ctx, streamKey)
	if err != nil {
		return true
	}
	gagged, err := a.st.IsGagged(ctx, channelID, userID)
	return err != nil || !gagged
}

// ---- pion 线一次性入场票 ----
// joinToken 完成入场判定后签发，/api/voice 信令入口凭票直接入会，不再二次判定。

const voiceTicketTTL = 60 * time.Second

type voiceTicket struct {
	room     string // 频道名（与信令 URL 的 channel 参数比对）
	identity string // 入会 identity（用户名+设备标签，joinToken 已算好）
	name     string // 显示名（用户名）
	userID   int64  // 持票人（与信令入口的会话用户比对，防票据挪用）
	muted    bool   // 入会即禁言
	expires  time.Time
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
