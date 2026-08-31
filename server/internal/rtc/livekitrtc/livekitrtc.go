// Package livekitrtc 是 rtc.Provider / rtc.IngestProvider 的 LiveKit 实现。
// 配置逐请求动态解析（环境变量 / 后台设置），客户端随配置变化重建。
package livekitrtc

import (
	"context"
	"os"
	"strings"
	"sync"

	"hearth/server/internal/lkingress"
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

// ConfigKeys 本实现声明的配置键（命名空间 livekit_* / ingress_*）。
func ConfigKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "livekit_api_url", Env: "LIVEKIT_API_URL", Group: "livekit", Default: apiURLDefault(),
			Label: "Twirp API 地址", Hint: "服务端调 LiveKit 的地址，也是信令代理的上游"},
		{Name: "livekit_api_key", Env: "LIVEKIT_API_KEY", Group: "livekit",
			Label: "API Key", Hint: "与 livekit.yaml 的 keys 一致"},
		{Name: "livekit_api_secret", Env: "LIVEKIT_API_SECRET", Group: "livekit", Secret: true,
			Label: "API Secret", Hint: "签发进房令牌用"},
		{Name: "livekit_url", Env: "LIVEKIT_URL", Group: "livekit",
			Label: "浏览器可见地址", Hint: "留空 = 同源信令代理（推荐）"},
		{Name: "ingress_upstream_url", Env: "INGRESS_UPSTREAM_URL", Group: "ingress",
			Label: "WHIP 上游地址", Hint: "留空 = 推流入口未启用（推流接口返回 503）"},
		{Name: "ingress_public_url", Env: "INGRESS_PUBLIC_URL", Group: "ingress",
			Label: "浏览器可见 WHIP 基地址", Hint: "留空 = 同源推流代理（推荐）"},
	}
}

// Provider 同时实现房间内核与推流入口（LiveKit 二者同一套 API 凭证）。
type Provider struct {
	cfg rtc.ConfigFunc

	mu               sync.Mutex
	url, key, secret string
	rooms            *lkroom.Client
	ing              *lkingress.Client
}

func New(cfg rtc.ConfigFunc) *Provider {
	return &Provider{cfg: cfg}
}

// clients 按当前生效配置取客户端；配置变了就重建。
func (p *Provider) clients(ctx context.Context) (*lkingress.Client, *lkroom.Client) {
	url := p.cfg(ctx, "livekit_api_url")
	key := p.cfg(ctx, "livekit_api_key")
	secret := p.cfg(ctx, "livekit_api_secret")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rooms == nil || url != p.url || key != p.key || secret != p.secret {
		p.url, p.key, p.secret = url, key, secret
		p.rooms = lkroom.NewClient(url, key, secret)
		p.ing = lkingress.NewClient(url, key, secret)
	}
	return p.ing, p.rooms
}

// ---- rtc.Provider ----

func (p *Provider) Name() string { return "livekit" }

func (p *Provider) JoinCredentials(ctx context.Context, room, username, deviceTag string) (rtc.Credentials, error) {
	tok, err := lktoken.Sign(p.cfg(ctx, "livekit_api_key"), p.cfg(ctx, "livekit_api_secret"), room, username, deviceTag)
	if err != nil {
		return rtc.Credentials{}, err
	}
	return rtc.Credentials{URL: p.cfg(ctx, "livekit_url"), Token: tok}, nil
}

func (p *Provider) RoomCounts(ctx context.Context) (map[string]int, error) {
	_, rooms := p.clients(ctx)
	return rooms.RoomCounts(ctx)
}

func (p *Provider) ListParticipants(ctx context.Context, room string) ([]rtc.Participant, error) {
	_, rooms := p.clients(ctx)
	ps, err := rooms.ListParticipants(ctx, room)
	if err != nil {
		return nil, err
	}
	out := make([]rtc.Participant, 0, len(ps))
	for _, x := range ps {
		out = append(out, rtc.Participant{Identity: x.Identity, Name: x.Name, JoinedAt: x.JoinedAt})
	}
	return out, nil
}

func (p *Provider) RemoveParticipantsOf(ctx context.Context, room, username string) (int, error) {
	_, rooms := p.clients(ctx)
	return rooms.RemoveParticipantsOf(ctx, room, username)
}

func (p *Provider) SignalProxyUpstream(ctx context.Context) string {
	return p.cfg(ctx, "livekit_api_url")
}

// ---- rtc.IngestProvider ----

func (p *Provider) Enabled(ctx context.Context) bool {
	return p.cfg(ctx, "ingress_upstream_url") != ""
}

func (p *Provider) CreateEndpoint(ctx context.Context, room, username string) (string, string, error) {
	ing, _ := p.clients(ctx)
	return ing.Create(ctx, room, username)
}

func (p *Provider) DeleteEndpoint(ctx context.Context, id string) error {
	ing, _ := p.clients(ctx)
	return ing.Delete(ctx, id)
}

func (p *Provider) PublicBase(ctx context.Context) string {
	return p.cfg(ctx, "ingress_public_url")
}

func (p *Provider) ProxyUpstream(ctx context.Context) string {
	return p.cfg(ctx, "ingress_upstream_url")
}
