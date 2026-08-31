// 依赖服务（LiveKit / Ingress）的动态配置。
// 规则：环境变量（含 .env，进程启动时已加载）设置了 → 以环境为准，管理后台只读展示；
// 未设置 → 管理后台可填，落库 settings（cfg_ 前缀），保存后即时生效（客户端与反代按当前值重建）。
package api

import (
	"context"
	"net/http"
	"os"
	"strings"

	"hearth/server/internal/lkingress"
	"hearth/server/internal/lkroom"
)

type dynKey struct {
	Name   string `json:"name"`
	Env    string `json:"env"`
	Label  string `json:"label"`
	Hint   string `json:"hint"`
	Secret bool   `json:"secret"`
	Group  string `json:"group"` // livekit | ingress
}

var dynKeys = []dynKey{
	{Name: "livekit_api_url", Env: "LIVEKIT_API_URL", Group: "livekit",
		Label: "Twirp API 地址", Hint: "服务端调 LiveKit 的地址，也是 /lk 信令代理的上游"},
	{Name: "livekit_api_key", Env: "LIVEKIT_API_KEY", Group: "livekit",
		Label: "API Key", Hint: "与 livekit.yaml 的 keys 一致"},
	{Name: "livekit_api_secret", Env: "LIVEKIT_API_SECRET", Group: "livekit", Secret: true,
		Label: "API Secret", Hint: "签发进房令牌用"},
	{Name: "livekit_url", Env: "LIVEKIT_URL", Group: "livekit",
		Label: "浏览器可见地址", Hint: "留空 = 同源 /lk 代理（推荐）"},
	{Name: "ingress_upstream_url", Env: "INGRESS_UPSTREAM_URL", Group: "ingress",
		Label: "WHIP 上游地址", Hint: "留空 = Ingress 未启用（推流接口返回 503）"},
	{Name: "ingress_public_url", Env: "INGRESS_PUBLIC_URL", Group: "ingress",
		Label: "浏览器可见 WHIP 基地址", Hint: "留空 = 同源 /w/ 代理（推荐）"},
}

func findDynKey(name string) *dynKey {
	for i := range dynKeys {
		if dynKeys[i].Name == name {
			return &dynKeys[i]
		}
	}
	return nil
}

// envFixed 该项是否被环境变量固定（.env 在启动时已合入环境）。
func envFixed(k *dynKey) bool { return os.Getenv(k.Env) != "" }

// dynDefault 未配置时的兜底值（沿用 config 包的推导逻辑，如 LIVEKIT_URL → API 地址）。
func (a *API) dynDefault(name string) string {
	switch name {
	case "livekit_api_url":
		return a.cfg.LiveKitAPIURL
	case "livekit_api_key":
		return a.cfg.LiveKitKey
	case "livekit_api_secret":
		return a.cfg.LiveKitSecret
	case "livekit_url":
		return a.cfg.LiveKitURL
	case "ingress_upstream_url":
		return a.cfg.IngressUpstreamURL
	case "ingress_public_url":
		return a.cfg.IngressPublicURL
	}
	return ""
}

// dynVal 取生效值：环境变量 > 数据库 > 兜底默认。
func (a *API) dynVal(ctx context.Context, name string) string {
	k := findDynKey(name)
	if k == nil {
		return ""
	}
	if v := os.Getenv(k.Env); v != "" {
		return v
	}
	if v, err := a.st.GetSetting(ctx, "cfg_"+name); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return a.dynDefault(name)
}

// lkClients 按当前生效配置取 LiveKit 客户端；配置变了就重建。
func (a *API) lkClients(ctx context.Context) (*lkingress.Client, *lkroom.Client) {
	apiURL := a.dynVal(ctx, "livekit_api_url")
	key := a.dynVal(ctx, "livekit_api_key")
	secret := a.dynVal(ctx, "livekit_api_secret")
	a.lkMu.Lock()
	defer a.lkMu.Unlock()
	if a.ing == nil || apiURL != a.lkCurURL || key != a.lkCurKey || secret != a.lkCurSecret {
		a.lkCurURL, a.lkCurKey, a.lkCurSecret = apiURL, key, secret
		a.ing = lkingress.NewClient(apiURL, key, secret)
		a.rooms = lkroom.NewClient(apiURL, key, secret)
	}
	return a.ing, a.rooms
}

// ---- 管理后台：读 / 写 ----

func (a *API) adminGetConfig(w http.ResponseWriter, r *http.Request) {
	type item struct {
		dynKey
		Value  string `json:"value"`
		Set    bool   `json:"set"`    // 是否有非空生效值
		Locked bool   `json:"locked"` // 环境变量固定
	}
	items := make([]item, 0, len(dynKeys))
	for i := range dynKeys {
		k := dynKeys[i]
		v := a.dynVal(r.Context(), k.Name)
		it := item{dynKey: k, Value: v, Set: v != "", Locked: envFixed(&k)}
		if k.Secret && v != "" {
			it.Value = "" // 密钥不回显，只报告已设置
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) adminSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Values map[string]string `json:"values"`
	}
	if !decode(w, r, &req) {
		return
	}
	for name := range req.Values {
		k := findDynKey(name)
		if k == nil {
			writeErr(w, http.StatusBadRequest, "未知配置项: "+name)
			return
		}
		if envFixed(k) {
			writeErr(w, http.StatusConflict, k.Label+" 已由环境变量固定，改部署侧配置后重启生效")
			return
		}
	}
	for name, value := range req.Values {
		if err := a.st.SetSetting(r.Context(), "cfg_"+name, strings.TrimSpace(value)); err != nil {
			writeErr(w, http.StatusInternalServerError, "内部错误")
			return
		}
	}
	// 让缓存的在线人数立即按新地址重取
	a.countsMu.Lock()
	a.counts = nil
	a.countsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
