// Package livekitrtc 是 rtc.Provider / rtc.StageProvider 的 LiveKit 实现
// （推流面 = LiveKit 自带 WHIP 入口的适配，见 whip.go）。
// 配置逐请求动态解析（环境变量 / 后台设置），客户端随配置变化重建。
package livekitrtc

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"

	"hearth/server/internal/lkroom"
	"hearth/server/internal/lktoken"
	"hearth/server/internal/rtc"
)

// apiURLDefault Twirp API 地址的兜底值：从 LIVEKIT_URL 推导（wss→https、去路径），否则本机默认。
func apiURLDefault() string {
	wss := os.Getenv("LIVEKIT_URL")
	if wss == "" {
		return "http://localhost:7880"
	}
	u := strings.TrimSuffix(wss, "/")
	u = strings.TrimPrefix(u, "wss://")
	u = strings.TrimPrefix(u, "ws://")
	scheme := "http"
	if strings.HasPrefix(wss, "wss://") {
		scheme = "https"
	}
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return scheme + "://" + u
}

// ConfigKeys 本实现声明的配置键（命名空间 livekit_*；推流面复用同一组键，见 whip.go）。
func ConfigKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "livekit_api_url", Env: "LIVEKIT_API_URL", Group: "livekit", Default: apiURLDefault(),
			Label: "Twirp API 地址", Hint: "服务端调 LiveKit 的地址，也是信令代理的上游；ws(s):// 写法会归一成 http(s)://"},
		{Name: "livekit_api_key", Env: "LIVEKIT_API_KEY", Group: "livekit",
			Label: "API Key", Hint: "与 livekit.yaml 的 keys 一致"},
		{Name: "livekit_api_secret", Env: "LIVEKIT_API_SECRET", Group: "livekit", Secret: true,
			Label: "API Secret", Hint: "签发进房令牌用"},
		{Name: "livekit_url", Env: "LIVEKIT_URL", Group: "livekit",
			Label: "浏览器可见地址", Hint: "留空 = 同源信令代理（推荐）"},
	}
}

// Provider 是房间/舞台内核（推流面由 WHIP 类型承担，见 whip.go）。
type Provider struct {
	cfg rtc.ConfigFunc

	mu               sync.Mutex
	url, key, secret string
	rooms            *lkroom.Client
}

func New(cfg rtc.ConfigFunc) *Provider {
	return &Provider{cfg: cfg}
}

// apiURL 生效的 Twirp API 地址：ws(s):// 写法（照抄浏览器信令地址的常见填法）归一成
// http(s)://——Twirp 客户端与信令反代都认不得 ws scheme（WHIP 面的归一见 whip.go）。
func (p *Provider) apiURL(ctx context.Context) string {
	return httpScheme(strings.TrimSpace(p.cfg(ctx, "livekit_api_url")))
}

// client 按当前生效配置取房间客户端；配置变了就重建。
func (p *Provider) client(ctx context.Context) *lkroom.Client {
	url := p.apiURL(ctx)
	key := p.cfg(ctx, "livekit_api_key")
	secret := p.cfg(ctx, "livekit_api_secret")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rooms == nil || url != p.url || key != p.key || secret != p.secret {
		p.url, p.key, p.secret = url, key, secret
		p.rooms = lkroom.NewClient(url, key, secret)
	}
	return p.rooms
}

// ---- rtc.Provider ----

func (p *Provider) Name() string { return "livekit" }

func (p *Provider) JoinCredentials(ctx context.Context, room string, meta rtc.Meta, canPublish bool) (rtc.Credentials, error) {
	tok, err := lktoken.Sign(p.cfg(ctx, "livekit_api_key"), p.cfg(ctx, "livekit_api_secret"), room, meta, canPublish)
	if err != nil {
		return rtc.Credentials{}, err
	}
	return rtc.Credentials{URL: p.cfg(ctx, "livekit_url"), Token: tok, Engine: "livekit"}, nil
}

func (p *Provider) RoomCounts(ctx context.Context) (map[string]int, error) {
	return p.client(ctx).RoomCounts(ctx)
}

func (p *Provider) ListParticipants(ctx context.Context, room string) ([]rtc.Participant, error) {
	ps, err := p.client(ctx).ListParticipants(ctx, room)
	if err != nil {
		return nil, err
	}
	out := make([]rtc.Participant, 0, len(ps))
	for _, x := range ps {
		pt := rtc.Participant{Identity: x.Identity, Name: x.Name, JoinedAt: x.JoinedAt}
		// 元数据是 hearth 组好的 rtc.Meta JSON（进房令牌 lktoken.Sign 与推流换票 whip.go
		// 两条路径都写）；非 JSON 或缺字段按无归属信息的参与者处理，展示侧自行兜底
		var meta rtc.Meta
		if json.Unmarshal([]byte(x.Metadata), &meta) == nil {
			pt.UID, pt.Username, pt.Kind, pt.Tag = meta.UID, meta.Username, meta.Kind, meta.Tag
		}
		out = append(out, pt)
	}
	return out, nil
}

func (p *Provider) RemoveParticipantsOf(ctx context.Context, room string, userID int64, device string) (int, error) {
	return p.client(ctx).RemoveParticipantsOf(ctx, room, userID, device)
}

func (p *Provider) MuteUserAudio(ctx context.Context, room string, userID int64, muted bool) error {
	return p.client(ctx).MuteUserAudio(ctx, room, userID, muted)
}

func (p *Provider) SignalProxyUpstream(ctx context.Context) string {
	return p.apiURL(ctx)
}
