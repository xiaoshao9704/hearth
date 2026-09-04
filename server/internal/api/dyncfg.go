// 动态配置与内核注册表。
// 配置键 = 内核选择器（voice_provider / stage_provider / ingest_provider）+ 内建实例的全局命名空间键。
// 规则：环境变量（含 .env，进程启动时已加载）设置了 → 以环境为准，管理后台只读展示；
// 未设置 → 管理后台可填，落库 settings（cfg_ 前缀），保存后即时生效。
// 例外：三个选择器不读环境变量（env 的职责只是把 provider 实例带进可选列表）；
// 部署侧旧的选择器 env 由迁移 v2 一次性落库，此后以管理后台为准。
package api

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc"
)

// 内核选择器：值是注册表里的实例 alias（见 providers.go），合法性由 adminSetConfig 的
// 选择器钩子按实例能力校验（Options 在 adminGetConfig 里按当前实例动态填充，注册新实例
// 即进可选列表）。语音线（voice）与舞台线（stage：投屏/摄像头/OBS 推流及其伴音）各占一个槽位。
var selectorKeys = []rtc.ConfigKey{
	{Name: "voice_provider", Group: "core",
		Label: "语音内核", Hint: "实例 alias；内建 ember = 进程内纯音频 SFU，零外部依赖；语音舞台同选一套 livekit 即单线形态"},
	{Name: "stage_provider", Group: "core",
		Label: "舞台内核", Hint: "实例 alias；none = 纯语音部署，禁用投屏与摄像头"},
	{Name: "ingest_provider", Group: "core",
		Label: "推流入口", Hint: "OBS/WHIP 推流的接入实例 alias；内建 bellows = 进程内直通网关（支持 HEVC/AV1，发进舞台内核）；" +
			"舞台线选 lkembed 时也可把这里一并选 lkembed，推流直接进进程内 LiveKit 自带的 WHIP 入口，少一跳回环 PeerConnection"},
}

// portmapKeys 自动端口映射：进程内网络基建，与 bellows_udp_port 同类的全局键（不进实例 params）。
var portmapKeys = []rtc.ConfigKey{
	{Name: "portmap_mode", Env: "PORTMAP_MODE", Group: "network", Default: "auto",
		Options: []string{"auto", "off"},
		Label:   "自动端口映射",
		Hint: "auto = 向默认网关申请 UPnP/PCP/NAT-PMP 映射（HTTP 端口与当前选中内核的媒体端口），" +
			"仅 host 网络或裸机可用（容器 bridge 网络发现不到网关）；off = 关闭并撤销已建映射"},
}

// siteKeys 站点身份：域名影响邀请链接与信令地址的推导（见 publicBase）与 TLS 证书签发。
var siteKeys = []rtc.ConfigKey{
	{Name: "site_domain", Env: "SITE_DOMAIN", Group: "site",
		Label: "公开域名", Hint: "如 voice.example.com；填了邀请链接就用 https://<域名>，TLS 的 acme 模式也按它签发。PUBLIC_URL 环境变量优先于它"},
}

// tlsKeys 进程内 TLS（见 internal/tlsx）：切换立即生效（HTTPS listener 热起停，
// HTTP 端口转做 ACME 挑战 + 301 重定向），不用重启。
var tlsKeys = []rtc.ConfigKey{
	{Name: "tls_mode", Env: "TLS_MODE", Group: "tls", Default: "off",
		Options: []string{"off", "acme", "selfsigned"},
		Label:   "证书模式",
		Hint: "off = 纯 HTTP（反代在前的部署）；acme = 按公开域名自动签发（需外部 80/443 可达）；" +
			"selfsigned = 本地 CA 自签，局域网/无域名场景用，设备装上 CA 证书后不再提示警告"},
	{Name: "https_addr", Env: "HTTPS_ADDR", Group: "tls", Default: ":8443",
		Label: "HTTPS 监听地址", Hint: "证书模式不为 off 时生效；保存即热切换。端口映射会把外部 443 指到它"},
	{Name: "acme_directory", Env: "ACME_DIRECTORY", Group: "tls", Default: "https://acme-v02.api.letsencrypt.org/directory",
		Label: "ACME 目录", Hint: "默认 Let's Encrypt；可换 ZeroSSL 或内网 step-ca"},
	{Name: "acme_email", Env: "ACME_EMAIL", Group: "tls",
		Label: "ACME 账户邮箱", Hint: "可空；填了 CA 能在证书快过期时联系到管理员"},
}

// selectorEnv 选择器对应的旧环境变量名：只供迁移 v2 一次性导入与启动告警，不参与取值。
var selectorEnv = map[string]string{
	"voice_provider":  "VOICE_PROVIDER",
	"stage_provider":  "STAGE_PROVIDER",
	"ingest_provider": "INGEST_PROVIDER",
}

// warnLegacyConfig 启动时检查已废弃/不再读取的旧配置，打一次日志提示管理员，不做静默迁移：
//   - 选择器 env（VOICE/STAGE/INGEST_PROVIDER）：不再读取（迁移 v2 已把旧值一次性落库），
//     提醒从部署侧删除；值是改名前残留（pion）时说明回落口径；
//   - 选择器 DB 值是 "pion"：按未知值回落默认实例（voice→ember、ingest→bellows）；
//   - pion_* 键：被忽略回落 ember_* 默认值；
//   - EMBED_LIVEKIT/EMBED_INGRESS/回环 LIVEKIT_API_URL：aio 自包含镜像的内嵌子进程（aioinit 拉起
//     livekit-server/redis/ingress）已退役，内嵌 LiveKit 并入本进程（内建实例 lkembed）；这三个
//     env 是旧编排的残留，hearth 本体从未读取，只提醒改走管理后台。
func (a *API) warnLegacyConfig(ctx context.Context) {
	for _, env := range selectorEnv {
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" {
			continue
		}
		switch {
		case v == "pion":
			log.Printf("配置告警: %s=pion 是改名前的残留（语音/推流内核现名 ember/bellows）；选择器已不再读环境变量，当前按管理后台所选实例运行", env)
		default:
			log.Printf("配置告警: %s 已不再读取（选择器以管理后台为准；旧值已在首次启动时落库导入），请从部署侧删除该环境变量", env)
		}
	}
	for _, sel := range []string{"voice_provider", "ingest_provider"} {
		if v, _ := a.st.GetSetting(ctx, "cfg_"+sel); strings.TrimSpace(v) == "pion" {
			log.Printf("配置告警: %s=pion 已不再支持（改名为 ember/bellows），当前按默认实例运行，请在管理后台重新选择", sel)
		}
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
	for _, env := range []string{"EMBED_LIVEKIT", "EMBED_INGRESS"} {
		if os.Getenv(env) != "" {
			log.Printf("配置告警: %s 已不再生效（自包含镜像的内嵌子进程已退役），舞台线请在管理后台把「舞台内核」改选 lkembed", env)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("LIVEKIT_API_URL")); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			if host := u.Hostname(); host == "127.0.0.1" || host == "localhost" || host == "::1" {
				log.Printf("配置告警: LIVEKIT_API_URL=%s 指向本机回环，疑似旧自包含镜像内嵌 LiveKit 的残留配置（该子进程已退役）；需要舞台内核请在管理后台改选 lkembed（进程内自带，无需此环境变量），指向真正的外部 LiveKit 才需要保留它", raw)
			}
		}
	}
}

func (a *API) allConfigKeys() []rtc.ConfigKey {
	keys := append(append([]rtc.ConfigKey{}, selectorKeys...), a.kernelKeys...)
	keys = append(keys, portmapKeys...)
	keys = append(keys, siteKeys...)
	return append(keys, tlsKeys...)
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
	// TLS 开启时公开链接按 80/443 拼（不带端口，ACME 两种挑战也都要这两个口）：
	// HTTP 与 HTTPS 的映射都指定首选外部端口并要求内外一致，网关改派宁可判失败走
	// port_conflict 诊断，也不能让邀请链接静默对不上。
	if a.dynVal(ctx, "tls_mode") != "off" {
		if _, port, err := net.SplitHostPort(a.dynVal(ctx, "https_addr")); err == nil {
			if p, err := strconv.Atoi(port); err == nil {
				ws = append(ws, portmap.Want{Proto: "tcp", Port: p, External: 443, StrictPort: true, Desc: "hearth https"})
			}
		}
		for i := range ws {
			if ws[i].Desc == "hearth http" {
				ws[i].External, ws[i].StrictPort = 80, true
			}
		}
	}
	if alias, _ := a.voiceInstance(ctx); alias == TypeEmber {
		ws = append(ws, portmap.Want{Proto: "udp", Port: dynPort(a.dynVal(ctx, "ember_udp_port")), Desc: "hearth voice"})
	}
	if alias, _, _ := a.ingestInstance(ctx); alias == TypeBellows {
		ws = append(ws, portmap.Want{Proto: "udp", Port: dynPort(a.dynVal(ctx, "bellows_udp_port")), Desc: "hearth whip"})
	}
	// lkembed（进程内 LiveKit）的媒体端口必须 StrictPort：LiveKit 的候选地址改写（补丁二）只换
	// IP 不换端口，与 pion 的 SDP 宣告同源限制一致；网关若把外部端口改派成别的号，宣告出去的
	// 候选端口就是错的，宁可让 Mapper 判定失败、走 port_conflict 诊断，也不能假装映射成功。
	if alias, _ := a.stageInstance(ctx); alias == AliasLkembed {
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
	// 舞台选择器切到/切走 lkembed：立即启停进程内 LiveKit（另起协程，启动要 1 秒级）
	if _, ok := req.Values["stage_provider"]; ok {
		go a.EnsureStageKernel(context.Background())
	}
	// TLS/域名相关键保存即热生效：HTTPS listener 热起停、证书现签（取舍见 internal/tlsx 包注释）
	for _, name := range []string{"tls_mode", "https_addr", "site_domain", "acme_directory", "acme_email"} {
		if _, ok := req.Values[name]; ok {
			go a.SyncTLS()
			break
		}
	}
	// 让缓存的在线人数立即按新配置重取
	a.countsMu.Lock()
	a.counts = nil
	a.countsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
