// LiveKit Ingress 推流入口：端点管理走 LiveKit Twirp API（lkingress），
// WHIP 信令反代到 ingress_upstream_url。与 livekitrtc.Provider 分实例后，
// 凭证字段在实例 params 里重复声明（实例即对象，互不引用）。
package livekitrtc

import (
	"context"
	"sync"

	"hearth/server/internal/lkingress"
	"hearth/server/internal/rtc"
)

// IngressKeys 注册表单的参数字段（兼作 env 锁定实例的探测键）。
func IngressKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "livekit_api_url", Env: "LIVEKIT_API_URL", Label: "Twirp API 地址", Hint: "端点管理用的 LiveKit API 地址",
			Default: apiURLDefault()},
		{Name: "livekit_api_key", Env: "LIVEKIT_API_KEY", Label: "API Key"},
		{Name: "livekit_api_secret", Env: "LIVEKIT_API_SECRET", Secret: true, Label: "API Secret"},
		{Name: "ingress_upstream_url", Env: "INGRESS_UPSTREAM_URL", Label: "WHIP 上游地址", Hint: "ingress 进程的 WHIP 监听地址"},
	}
}

// Ingress 实现 rtc.IngestProvider（LiveKit Ingress）。
type Ingress struct {
	cfg rtc.ConfigFunc

	mu               sync.Mutex
	url, key, secret string
	ing              *lkingress.Client
}

func NewIngress(cfg rtc.ConfigFunc) *Ingress { return &Ingress{cfg: cfg} }

func (i *Ingress) Name() string { return "livekit-ingress" }

func (i *Ingress) client(ctx context.Context) *lkingress.Client {
	url, key, secret := i.cfg(ctx, "livekit_api_url"), i.cfg(ctx, "livekit_api_key"), i.cfg(ctx, "livekit_api_secret")
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ing == nil || url != i.url || key != i.key || secret != i.secret {
		i.url, i.key, i.secret = url, key, secret
		i.ing = lkingress.NewClient(url, key, secret)
	}
	return i.ing
}

func (i *Ingress) Enabled(ctx context.Context) bool {
	return i.cfg(ctx, "livekit_api_key") != "" && i.cfg(ctx, "livekit_api_secret") != "" &&
		i.cfg(ctx, "ingress_upstream_url") != ""
}

func (i *Ingress) CreateEndpoint(ctx context.Context, room, username string) (string, string, error) {
	return i.client(ctx).Create(ctx, room, username)
}

func (i *Ingress) DeleteEndpoint(ctx context.Context, id string) error {
	return i.client(ctx).Delete(ctx, id)
}

func (i *Ingress) ProxyUpstream(ctx context.Context) string {
	return i.cfg(ctx, "ingress_upstream_url")
}
