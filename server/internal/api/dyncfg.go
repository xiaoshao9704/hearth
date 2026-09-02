// 动态配置与内核注册表。
// 配置键 = 内核选择器（voice_provider / stage_provider / ingest_provider）+ 内建实例的全局命名空间键。
// 规则：环境变量（含 .env，进程启动时已加载）设置了 → 以环境为准，管理后台只读展示；
// 未设置 → 管理后台可填，落库 settings（cfg_ 前缀），保存后即时生效。
package api

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"

	"hearth/server/internal/rtc"
)

// 内核选择器：值是注册表里的实例 alias（见 providers.go），合法性由 adminSetConfig 的
// 选择器钩子按实例能力校验（Options 在 adminGetConfig 里按当前实例动态填充）。
// 语音线（voice）与舞台线（stage：投屏/摄像头/OBS 推流及其伴音）各占一个槽位。
var selectorKeys = []rtc.ConfigKey{
	{Name: "voice_provider", Env: "VOICE_PROVIDER", Group: "core",
		Label: "语音内核", Hint: "实例 alias；内建 ember = 进程内纯音频 SFU，零外部依赖；语音舞台同选一套 livekit 即单线形态"},
	{Name: "stage_provider", Env: "STAGE_PROVIDER", Group: "core",
		Label: "舞台内核", Hint: "实例 alias；none = 纯语音部署，禁用投屏与摄像头"},
	{Name: "ingest_provider", Env: "INGEST_PROVIDER", Group: "core",
		Label: "推流入口", Hint: "OBS/WHIP 推流的接入实例 alias；内建 bellows = 进程内直通网关（支持 HEVC/AV1，发进 LiveKit 房间）"},
}

// warnLegacyConfig 启动时检查改名前的旧配置（0.3.0 曾做兼容映射，0.3.1 起不再识别）：
// 选择器里的 "pion" 按未知值回落默认实例（voice→ember、ingest→bellows），pion_* 键被忽略
// 回落 ember_* 默认值，这里只打一次日志提示管理员改配置，不做静默迁移。
func (a *API) warnLegacyConfig(ctx context.Context) {
	for _, sel := range []string{"voice_provider", "ingest_provider"} {
		if a.dynVal(ctx, sel) == "pion" {
			log.Printf("配置告警: %s=pion 已不再支持（改名为 ember/bellows），当前按默认实例运行，请在管理后台重新选择", sel)
		}
	}
	// env 里的旧值无法改写（DB 值已由 migrateProviders 改写为 livekit-ingress），只告警
	if os.Getenv("INGEST_PROVIDER") == "livekit" {
		log.Printf("配置告警: INGEST_PROVIDER=livekit 已不再有效（推流入口已拆为独立类型），请改为 livekit-ingress，当前回落到默认实例 bellows")
	}
	for _, old := range []struct{ Env, Name, New string }{
		{"PION_UDP_PORT", "pion_udp_port", "ember_udp_port"},
		{"PION_PUBLIC_IP", "pion_public_ip", "ember_public_ip"},
	} {
		v := os.Getenv(old.Env)
		if v == "" {
			v, _ = a.st.GetSetting(ctx, "cfg_"+old.Name)
		}
		if strings.TrimSpace(v) != "" {
			log.Printf("配置告警: %s/%s 已不再读取，请改用 %s（当前按默认值运行）", old.Env, old.Name, old.New)
		}
	}
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

// dynVal 取生效值：环境变量 > 数据库 > 实现声明的兜底默认。
// 选择器默认：voice→ember、stage→none、ingest→bellows（内建实例兜底，零外部依赖）；
// 选择器取到未注册或无对应能力的 alias 时由各 *Instance 取值函数回落（见 providers.go）。
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
	switch name {
	case "voice_provider":
		return TypeEmber
	case "stage_provider":
		return "none"
	case "ingest_provider":
		return TypeBellows
	}
	return k.Default
}

// selectorCap 选择器槽位要求的能力；非选择器键返回 ""。
func selectorCap(name string) string {
	switch name {
	case "voice_provider":
		return "voice"
	case "stage_provider":
		return "stage"
	case "ingest_provider":
		return "ingest"
	}
	return ""
}

// selectorOptions 选择器当前可填的取值：具备对应能力的实例 alias（stage 另含 none）。
func (a *API) selectorOptions(ctx context.Context, name string) []string {
	need := selectorCap(name)
	var opts []string
	for _, inst := range a.listInstances(ctx) {
		for _, c := range inst.Caps() {
			if c == need {
				opts = append(opts, inst.Alias)
				break
			}
		}
	}
	if need == "stage" {
		opts = append(opts, "none")
	}
	return opts
}

// checkSelector 选择器取值校验钩子：空（恢复默认）、none（仅 stage）或已注册且具备
// 对应槽位能力的实例 alias；不合法返回面向管理员的错误文案。
func (a *API) checkSelector(ctx context.Context, name, value string) string {
	if value == "" {
		return ""
	}
	if name == "stage_provider" && value == "none" {
		return ""
	}
	need := selectorCap(name)
	if inst := a.instance(value); inst != nil {
		for _, c := range inst.Caps() {
			if c == need {
				return ""
			}
		}
	}
	k := a.findDynKey(name)
	label := name
	if k != nil {
		label = k.Label
	}
	return label + " 必须是具备对应能力的已注册实例 alias（当前可选: " + strings.Join(a.selectorOptions(ctx, name), " / ") + "）"
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
		if selectorCap(k.Name) != "" {
			k.Options = a.selectorOptions(r.Context(), k.Name) // 选择器可选项 = 当前注册实例
		}
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
		if selectorCap(name) != "" {
			if msg := a.checkSelector(r.Context(), name, strings.TrimSpace(req.Values[name])); msg != "" {
				writeErr(w, http.StatusBadRequest, msg)
				return
			}
		} else if len(k.Options) > 0 {
			ok := false
			for _, opt := range k.Options {
				if req.Values[name] == opt {
					ok = true
					break
				}
			}
			if !ok {
				writeErr(w, http.StatusBadRequest, k.Label+" 的取值必须是: "+strings.Join(k.Options, " / "))
				return
			}
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
