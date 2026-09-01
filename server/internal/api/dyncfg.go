// 动态配置与内核注册表。
// 配置键 = 内核选择器（rtc_provider / ingest_provider）+ 各实现自带的命名空间键。
// 规则：环境变量（含 .env，进程启动时已加载）设置了 → 以环境为准，管理后台只读展示；
// 未设置 → 管理后台可填，落库 settings（cfg_ 前缀），保存后即时生效。
package api

import (
	"context"
	"net/http"
	"os"
	"strings"

	"hearth/server/internal/rtc"
)

// 内核选择器：换实现只改这里的值，各实现的配置键互不干扰、原样保留。
// 语音线（voice）与舞台线（stage：投屏/摄像头/OBS 推流及其伴音）各占一个槽位。
var selectorKeys = []rtc.ConfigKey{
	{Name: "voice_provider", Env: "VOICE_PROVIDER", Group: "core",
		Label: "语音内核", Hint: "可选：livekit / pion（进程内纯音频 SFU）"},
	{Name: "stage_provider", Env: "STAGE_PROVIDER", Group: "core",
		Label: "舞台内核", Hint: "可选：livekit / none（纯语音部署，禁用投屏与摄像头）"},
	{Name: "ingest_provider", Env: "INGEST_PROVIDER", Group: "core",
		Label: "推流入口", Hint: "当前可选：livekit"},
}

func (a *API) allConfigKeys() []rtc.ConfigKey {
	return append(append([]rtc.ConfigKey{}, selectorKeys...), a.kernelKeys...)
}

func (a *API) findDynKey(name string) *rtc.ConfigKey {
	keys := a.allConfigKeys()
	for i := range keys {
		if keys[i].Name == name {
			return &keys[i]
		}
	}
	return nil
}

// envFixed 该项是否被环境变量固定（.env 在启动时已合入环境）。
func envFixed(k *rtc.ConfigKey) bool { return k.Env != "" && os.Getenv(k.Env) != "" }

// dynVal 取生效值：环境变量 > 数据库 > 实现声明的兜底默认（选择器默认 livekit）。
func (a *API) dynVal(ctx context.Context, name string) string {
	k := a.findDynKey(name)
	if k == nil {
		return ""
	}
	if k.Env != "" {
		if v := os.Getenv(k.Env); v != "" {
			return v
		}
	}
	if v, err := a.st.GetSetting(ctx, "cfg_"+name); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if name == "voice_provider" || name == "stage_provider" || name == "ingest_provider" {
		return "livekit" // 默认实现（两线同一 LiveKit 即今天的单线形态）
	}
	return k.Default
}

// voiceProvider 按选择器取语音内核；未知名字回退 livekit。
func (a *API) voiceProvider(ctx context.Context) rtc.Provider {
	if p, ok := a.voiceKernels[a.dynVal(ctx, "voice_provider")]; ok {
		return p
	}
	return a.voiceKernels["livekit"]
}

// stageProvider 按选择器取舞台内核；"none" 返回 nil（纯语音部署）。
func (a *API) stageProvider(ctx context.Context) rtc.Provider {
	name := a.dynVal(ctx, "stage_provider")
	if name == "none" {
		return nil
	}
	if p, ok := a.stageKernels[name]; ok {
		return p
	}
	return a.stageKernels["livekit"]
}

// ingestProvider 按选择器取推流入口；未知名字回退 livekit。
func (a *API) ingestProvider(ctx context.Context) rtc.IngestProvider {
	if p, ok := a.ingestKernels[a.dynVal(ctx, "ingest_provider")]; ok {
		return p
	}
	return a.ingestKernels["livekit"]
}

// ---- 管理后台：读 / 写 ----

func (a *API) adminGetConfig(w http.ResponseWriter, r *http.Request) {
	type item struct {
		rtc.ConfigKey
		Value  string `json:"value"`
		Set    bool   `json:"set"`    // 是否有非空生效值
		Locked bool   `json:"locked"` // 环境变量固定
	}
	keys := a.allConfigKeys()
	items := make([]item, 0, len(keys))
	for i := range keys {
		k := keys[i]
		v := a.dynVal(r.Context(), k.Name)
		it := item{ConfigKey: k, Value: v, Set: v != "", Locked: envFixed(&k)}
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
		k := a.findDynKey(name)
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
	// 让缓存的在线人数立即按新配置重取
	a.countsMu.Lock()
	a.counts = nil
	a.countsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
