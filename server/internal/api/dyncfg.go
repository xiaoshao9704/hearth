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

// 内核选择器：换实现只改这两个值，各实现的配置键互不干扰、原样保留。
var selectorKeys = []rtc.ConfigKey{
	{Name: "rtc_provider", Env: "RTC_PROVIDER", Group: "core",
		Label: "房间内核", Hint: "当前可选：livekit"},
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
	if name == "rtc_provider" || name == "ingest_provider" {
		return "livekit" // 注册表里的默认实现
	}
	return k.Default
}

// rtcProvider 按选择器取房间内核；未知名字回退 livekit。
func (a *API) rtcProvider(ctx context.Context) rtc.Provider {
	if p, ok := a.rtcKernels[a.dynVal(ctx, "rtc_provider")]; ok {
		return p
	}
	return a.rtcKernels["livekit"]
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
