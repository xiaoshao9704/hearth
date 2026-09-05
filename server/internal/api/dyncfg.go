// 动态配置与内核注册表。
// 配置键 = 内核选择器（voice_provider / stage_provider）+ 内建实例的全局命名空间键。
// 规则：环境变量（含 .env，进程启动时已加载）设置了 → 以环境为准，管理后台只读展示；
// 未设置 → 管理后台可填，落库 settings（cfg_ 前缀），保存后即时生效。
// 例外：选择器不读环境变量（env 的职责只是把 provider 实例带进可选列表）；
// 部署侧旧的选择器 env 由迁移 v2 一次性落库，此后以管理后台为准。
package api

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc"
)

// 内核选择器：值是注册表里的实例 alias（见 providers.go），合法性由 adminSetConfig 的
// 选择器钩子按实例能力校验（Options 在 adminGetConfig 里按当前实例动态填充，注册新实例
// 即进可选列表）。语音线（voice）与舞台线（stage：投屏/摄像头/OBS 推流及其伴音）各占一个槽位。
// 推流不再是独立选择器：OBS 的 WHIP 一律进当前舞台实例自带的入口（见 admitIngest）。
var selectorKeys = []rtc.ConfigKey{
	{Name: "voice_provider", Group: "core",
		Label: "语音内核", Hint: "实例 alias；语音舞台同选一套 livekit 即单线（combined）形态"},
	{Name: "stage_provider", Group: "core",
		Label: "舞台内核", Hint: "实例 alias；none = 纯语音部署，禁用投屏与摄像头；OBS 推流也进这个实例自带的 WHIP 入口"},
}

// portmapKeys 自动端口映射：进程内网络基建的全局键（不进实例 params）。
var portmapKeys = []rtc.ConfigKey{
	{Name: "portmap_mode", Env: "PORTMAP_MODE", Group: "network", Default: "auto",
		Options: []string{"auto", "off"},
		Label:   "自动端口映射",
		Hint: "auto = 向默认网关申请 UPnP/PCP/NAT-PMP 映射（HTTP 端口与当前选中内核的媒体端口），" +
			"仅 host 网络或裸机可用（容器 bridge 网络发现不到网关）；off = 关闭并撤销已建映射"},
}

// clientICEKeys 面向浏览器的 ICE 策略：是 hearth 的全局策略而非某个实例的参数，
// 由信令反代逐连接读取后改写下发（见 signalbridge.go / iceservers.go）。
var clientICEKeys = []rtc.ConfigKey{
	{Name: "client_stun_servers", Env: "CLIENT_STUN_SERVERS", Group: "network", Default: defaultClientSTUN,
		Label: "浏览器 STUN 服务器",
		Hint: "逗号分隔的 host:port，由信令反代改写后下发给浏览器，对所有内核实例生效、保存即生效。" +
			"浏览器并行探测、谁先回用谁，所以按地域并列几个即可；none = 不下发任何 STUN" +
			"（连通性不依赖它：客户端永远是主动方，服务端从收到的包学到对端地址）。" +
			"与实例参数 lkembed_stun_servers 语义不同——那个是服务端自己探测公网映射用的"},
}

// selectorEnv 选择器对应的旧环境变量名：只供迁移 v2 一次性导入，不参与取值。
var selectorEnv = map[string]string{
	"voice_provider": "VOICE_PROVIDER",
	"stage_provider": "STAGE_PROVIDER",
}

// warnLegacyConfig 启动时检查已废弃/不再读取的旧环境变量，各打一行日志提示管理员从
// 部署侧删除。本版本（内核收敛）的告警集：Ember/Bellows/livekit-ingress 退场，
// 语音/推流已并入进程内 LiveKit（内建实例 lkembed），下列 env 一律不再读取
// （LIVEKIT_API_URL 仍是合法的 env 锁定实例来源，不在其列）。
// 比照 pion_* 先例只保留一个版本，下个版本删除本函数。
func (a *API) warnLegacyConfig() {
	var names []string
	for _, env := range []string{
		"EMBER_UDP_PORT", "EMBER_PUBLIC_IP", "EMBER_STUN_SERVERS", "INGRESS_UPSTREAM_URL",
	} {
		if os.Getenv(env) != "" {
			names = append(names, env)
		}
	}
	for _, e := range os.Environ() {
		if name, val, ok := strings.Cut(e, "="); ok && val != "" && strings.HasPrefix(name, "BELLOWS_") {
			names = append(names, name)
		}
	}
	for _, name := range names {
		log.Printf("配置告警: %s 已不再读取（语音/推流已并入进程内 LiveKit（lkembed）），请从部署侧删除该环境变量", name)
	}
}

func (a *API) allConfigKeys() []rtc.ConfigKey {
	keys := append(append([]rtc.ConfigKey{}, selectorKeys...), a.kernelKeys...)
	keys = append(keys, portmapKeys...)
	return append(keys, clientICEKeys...)
}

// PortWants 当前要向网关申请的映射：HTTP 端口 + 当前选中内核里跑在本进程的媒体端口
// （选的是外部实例时那些端口不在本机，映射了也没意义）。cmd/server 把它交给 portmap.Mapper
// 每轮读——端口与内核选择都是动态配置，后台改了下一轮（renewInterval，见 portmap.go）就
// 撤旧加新，选择器切换不需要重启；这层"热更新"就是 Mapper.Run 本身的续租循环，未额外接线。
// 大多数端口不置 StrictPort：SDP 出口能宣告与监听端口不同的外部端口，让网关自由改派可用性更高
// （同端口优先仍由 Mapper 保证，上游 DMZ 的端口不变透传因此照样能对上）。
// 例外是 lkembed 的媒体端口，见下方注释。
func (a *API) PortWants(ctx context.Context) []portmap.Want {
	if a.dynVal(ctx, "portmap_mode") == "off" {
		return nil
	}
	var ws []portmap.Want
	if _, port, err := net.SplitHostPort(a.cfg.Addr); err == nil {
		if p, err := strconv.Atoi(port); err == nil {
			ws = append(ws, portmap.Want{Proto: "tcp", Port: p, Desc: "hearth http"})
		}
	}
	// lkembed（进程内 LiveKit）的媒体端口必须 StrictPort：LiveKit 的候选地址改写（补丁二）只换
	// IP 不换端口，与 pion 的 SDP 宣告同源限制一致；网关若把外部端口改派成别的号，宣告出去的
	// 候选端口就是错的，宁可让 Mapper 判定失败、走 port_conflict 诊断，也不能假装映射成功。
	// 语音线与舞台线任一选中 lkembed 都需要该端口（语音默认即 lkembed）。
	vAlias, _ := a.voiceInstance(ctx)
	sAlias, _ := a.stageInstance(ctx)
	if vAlias == AliasLkembed || sAlias == AliasLkembed {
		ws = append(ws, portmap.Want{Proto: "udp", Port: dynPort(a.dynVal(ctx, "lkembed_udp_port")), Desc: "hearth stage", StrictPort: true})
		if tcp := dynPort(a.dynVal(ctx, "lkembed_tcp_port")); tcp > 0 {
			ws = append(ws, portmap.Want{Proto: "tcp", Port: tcp, Desc: "hearth stage", StrictPort: true})
		}
	}
	return ws
}

// dynPort 端口配置项转数字；填错了返回 0，由 Mapper 的 normalizeWants 丢掉。
func dynPort(v string) int {
	p, _ := strconv.Atoi(strings.TrimSpace(v))
	return p
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

// dynVal 取生效值：环境变量（选择器除外） > 数据库 > 实现声明的兜底默认。
// 选择器默认：voice→lkembed、stage→lkembed（进程内 LiveKit，语音舞台同选即 combined
// 单连接）；选择器取到未注册或无对应能力的 alias 时由各 *Instance 取值函数回落（见 providers.go）。
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
		return AliasLkembed
	case "stage_provider":
		return AliasLkembed
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
	// 语音/舞台选择器切到/切走 lkembed：立即启停进程内 LiveKit（另起协程，启动要 1 秒级）
	if _, ok := req.Values["stage_provider"]; ok {
		go a.EnsureStageKernel(context.Background())
	} else if _, ok := req.Values["voice_provider"]; ok {
		go a.EnsureStageKernel(context.Background())
	}
	// 让缓存的在线人数立即按新配置重取
	a.countsMu.Lock()
	a.counts = nil
	a.countsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
