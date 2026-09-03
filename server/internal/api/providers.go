// 内核实例注册表：实例即对象，每条注册构造独立的 rtc 接口对象，api 持 map[alias]实例。
// 实例来源三类：内建（ember/bellows，进程内零依赖兜底）、env 锁定（环境变量合成，只读）、
// DB 注册（providers 表，params 即旧命名空间键名，rtc 实现零改动）。
// 选择器（voice_provider/stage_provider/ingest_provider）的值是实例 alias。
package api

import (
	"context"
	"log"
	"maps"
	"os"
	"regexp"
	"strings"

	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/bellows"
	"hearth/server/internal/rtc/livekitrtc"
	"hearth/server/internal/store"
)

// 实例类型（内建两类 + 可注册三类）
const (
	TypeLivekit        = "livekit"
	TypeLivekitIngress = "livekit-ingress"
	TypeBellowsRemote  = "bellows-remote"
	TypeEmber          = "ember"   // 内建
	TypeBellows        = "bellows" // 内建（进程内 WHIP 直通）
	// TypeLivekitEmbedded 内建：补丁式 fork 的 LiveKit 跑在本进程内，只监听回环，
	// 浏览器经 /providers/lkembed/rtc 同源反代访问（见 lkembed.go）
	TypeLivekitEmbedded = "livekit-embedded"
)

// AliasLkembed 内建进程内 LiveKit 实例的 alias（不能叫 livekit，那个留给 env 锁定实例）。
const AliasLkembed = "lkembed"

// alias 规则：单段小写，出现在 URL 路径里；类型同名的 alias 保留给 env 锁定实例
var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var reservedAliases = map[string]bool{
	TypeEmber: true, TypeBellows: true, TypeLivekit: true,
	TypeLivekitIngress: true, TypeBellowsRemote: true, AliasLkembed: true,
}

// ProviderInstance 一个注册的内核实例：alias 唯一标识，能力按槽位接口非空判定。
type ProviderInstance struct {
	Alias   string
	Type    string
	Params  map[string]string
	Cfg     rtc.ConfigFunc // params + 字段模式 Default 的解析（跨实例委托读配置统一走它）
	Locked  bool           // env 锁定，只读
	Builtin bool
	Voice   rtc.Provider       // livekit/ember 非空
	Stage   rtc.StageProvider  // livekit 非空
	Ingest  rtc.IngestProvider // livekit-ingress/bellows/bellows-remote 非空
}

// Caps 实例具备的槽位能力，如 ["voice","stage"] / ["ingest"]。
func (i *ProviderInstance) Caps() []string {
	var caps []string
	if i.Voice != nil {
		caps = append(caps, "voice")
	}
	if i.Stage != nil {
		caps = append(caps, "stage")
	}
	if i.Ingest != nil {
		caps = append(caps, "ingest")
	}
	return caps
}

// 注册表单字段模式（值在实例 params 里复用旧键名，rtc 实现零改动）
func (a *API) providerTypeFields(typ string) []rtc.ConfigKey {
	switch typ {
	case TypeLivekit:
		return livekitrtc.ConfigKeys()
	case TypeLivekitIngress:
		return livekitrtc.IngressKeys()
	case TypeBellowsRemote:
		return bellows.RemoteKeys()
	}
	return nil
}

// paramsCfg 把实例 params 包成 rtc.ConfigFunc（实例即对象的关键）；
// params 空值回落字段模式声明的 Default（如 livekit_api_url 的 apiURLDefault()）
func paramsCfg(params map[string]string, fields []rtc.ConfigKey) rtc.ConfigFunc {
	return func(_ context.Context, name string) string {
		if v := params[name]; v != "" {
			return v
		}
		for _, f := range fields {
			if f.Name == name {
				return f.Default
			}
		}
		return ""
	}
}

// builtinInstances 内建实例：ember 语音；bellows 进程内 WHIP 直通（发布出口 = 当前舞台线实例的 Publisher）。
func (a *API) builtinInstances() []*ProviderInstance {
	return []*ProviderInstance{
		{Alias: TypeEmber, Type: TypeEmber, Builtin: true, Cfg: a.dynVal, Voice: a.ember},
		{Alias: TypeBellows, Type: TypeBellows, Builtin: true, Cfg: a.dynVal,
			Ingest: bellows.New(a.dynVal, a.ingressResolver, a.stagePublisherSink, a.mapped)},
		{Alias: AliasLkembed, Type: TypeLivekitEmbedded, Builtin: true, Cfg: a.embedCfg, Stage: a.lkembed},
	}
}

// reloadProviders 重建实例注册表（reloadMu 串行化「写 DB → 重建」，见 mutateProviders）。
func (a *API) reloadProviders(ctx context.Context) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	a.reloadProvidersLocked(ctx)
}

// reloadProvidersLocked 须持 reloadMu 调用。顺序即 listInstances 顺序：内建 → env 锁定 →
// DB（按创建序）；DB 与 env 同名时 env 优先。未变化的实例（Type+Params 相等）复用旧对象：
// 内建 bellows 的活动会话不被 reload 打断。ListProviders 失败时保留旧注册表，
// 不用只含内建+env 的表覆盖（否则 DB 实例全部消失直到下次管理操作）。
func (a *API) reloadProvidersLocked(ctx context.Context) {
	a.providersMu.RLock()
	old := a.providers
	a.providersMu.RUnlock()

	m := map[string]*ProviderInstance{}
	order := []string{}
	add := func(inst *ProviderInstance) {
		if _, dup := m[inst.Alias]; dup {
			return
		}
		if o := old[inst.Alias]; o != nil && o.Type == inst.Type && o.Locked == inst.Locked &&
			o.Builtin == inst.Builtin && maps.Equal(o.Params, inst.Params) {
			inst = o
		}
		m[inst.Alias] = inst
		order = append(order, inst.Alias)
	}

	// 内建实例排最前（首次 reload 时复用 New 种下的对象，活动会话不被打断）
	for _, inst := range a.builtinInstances() {
		add(inst)
	}

	// env 锁定：按各类型字段的 Env 探测，任一关键 env 存在即合成只读实例，params 从 env 读全字段
	if params := envLockedParams(a.providerTypeFields(TypeLivekit), "livekit_api_url", "livekit_api_key", "livekit_api_secret"); params != nil {
		cfg := paramsCfg(params, a.providerTypeFields(TypeLivekit))
		lk := livekitrtc.New(cfg)
		add(&ProviderInstance{Alias: TypeLivekit, Type: TypeLivekit, Params: params, Cfg: cfg, Locked: true, Voice: lk, Stage: lk})
	}
	if params := envLockedParams(a.providerTypeFields(TypeLivekitIngress), "ingress_upstream_url"); params != nil {
		cfg := paramsCfg(params, a.providerTypeFields(TypeLivekitIngress))
		add(&ProviderInstance{Alias: TypeLivekitIngress, Type: TypeLivekitIngress, Params: params, Cfg: cfg, Locked: true,
			Ingest: livekitrtc.NewIngress(cfg)})
	}
	if params := envLockedParams(a.providerTypeFields(TypeBellowsRemote), "bellows_remote_url"); params != nil {
		cfg := paramsCfg(params, a.providerTypeFields(TypeBellowsRemote))
		add(&ProviderInstance{Alias: TypeBellowsRemote, Type: TypeBellowsRemote, Params: params, Cfg: cfg, Locked: true,
			Ingest: bellows.New(cfg, nil, nil, nil)})
	}

	// DB 注册
	recs, err := a.st.ListProviders(ctx)
	if err != nil {
		log.Printf("加载 providers 失败（保留旧注册表）: %v", err)
		return
	}
	for _, rec := range recs {
		if inst := a.instantiateProvider(rec); inst != nil {
			add(inst)
		}
	}

	a.providersMu.Lock()
	a.providers = m
	a.providerOrder = order
	a.providersMu.Unlock()
}

// mutateProviders 串行化「写 DB → 重建注册表」：fn（DB 写）与重建在同一把 reloadMu 内完成，
// 避免并发 CRUD 交错时后写者把过期快照换上去（已删除的实例复活）。
func (a *API) mutateProviders(ctx context.Context, fn func() error) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if err := fn(); err != nil {
		return err
	}
	a.reloadProvidersLocked(ctx)
	return nil
}

// envLockedParams probes 里任一字段的 Env 已设置时，从环境读全字段合成 params；否则返回 nil。
func envLockedParams(fields []rtc.ConfigKey, probes ...string) map[string]string {
	set := map[string]bool{}
	for _, p := range probes {
		set[p] = true
	}
	any := false
	for _, f := range fields {
		if set[f.Name] && f.Env != "" && os.Getenv(f.Env) != "" {
			any = true
			break
		}
	}
	if !any {
		return nil
	}
	params := map[string]string{}
	for _, f := range fields {
		if f.Env != "" {
			params[f.Name] = os.Getenv(f.Env)
		}
	}
	return params
}

// backfillEnv params 里为空的字段回读对应环境变量补齐：旧混合部署（部分配置在 env、
// 部分在 DB 的 cfg_ 键）迁移成实例后仍可用。字段→env 映射取字段模式的 Env 声明。
func backfillEnv(params map[string]string, fields []rtc.ConfigKey) {
	for _, f := range fields {
		if params[f.Name] == "" && f.Env != "" {
			if v := os.Getenv(f.Env); v != "" {
				params[f.Name] = v
			}
		}
	}
}

// instantiateProvider 把 DB 记录构造成实例；未知类型返回 nil（跳过，不阻塞其余实例）。
func (a *API) instantiateProvider(rec *store.ProviderRecord) *ProviderInstance {
	cfg := paramsCfg(rec.Params, a.providerTypeFields(rec.Type))
	inst := &ProviderInstance{Alias: rec.Alias, Type: rec.Type, Params: rec.Params, Cfg: cfg}
	switch rec.Type {
	case TypeLivekit:
		lk := livekitrtc.New(cfg)
		inst.Voice, inst.Stage = lk, lk
	case TypeLivekitIngress:
		inst.Ingest = livekitrtc.NewIngress(cfg)
	case TypeBellowsRemote:
		inst.Ingest = bellows.New(cfg, nil, nil, nil)
	default:
		log.Printf("providers 表实例 %s 类型未知（%s），跳过", rec.Alias, rec.Type)
		return nil
	}
	return inst
}

// stagePublisherSink 进程内 bellows 的发布出口：当前舞台线实例的 rtc.Publisher
// （每次发布时取，注册表/选择器切换即生效）；舞台线为 none 或实例不实现 Publisher 时返回 nil
// （bellows 据此 Enabled=false）。
func (a *API) stagePublisherSink(ctx context.Context) rtc.Publisher {
	_, sp := a.stageInstance(ctx)
	if pub, ok := sp.(rtc.Publisher); ok {
		return pub
	}
	return nil
}

// instance 按 alias 取实例，不存在返回 nil。
func (a *API) instance(alias string) *ProviderInstance {
	a.providersMu.RLock()
	defer a.providersMu.RUnlock()
	return a.providers[alias]
}

// listInstances 全部实例：内建 → env 锁定 → DB（按创建序）。
func (a *API) listInstances(ctx context.Context) []*ProviderInstance {
	a.providersMu.RLock()
	defer a.providersMu.RUnlock()
	out := make([]*ProviderInstance, 0, len(a.providerOrder))
	for _, alias := range a.providerOrder {
		out = append(out, a.providers[alias])
	}
	return out
}

// voiceInstance 按选择器取语音实例；未知 alias 或无语音能力回落内建 ember。
func (a *API) voiceInstance(ctx context.Context) (string, rtc.Provider) {
	if inst := a.instance(a.dynVal(ctx, "voice_provider")); inst != nil && inst.Voice != nil {
		return inst.Alias, inst.Voice
	}
	return TypeEmber, a.instance(TypeEmber).Voice
}

// stageInstance 按选择器取舞台实例；"none"、未知 alias 或无舞台能力 → ("", nil)（纯语音部署）。
func (a *API) stageInstance(ctx context.Context) (string, rtc.StageProvider) {
	if inst := a.instance(a.dynVal(ctx, "stage_provider")); inst != nil && inst.Stage != nil {
		return inst.Alias, inst.Stage
	}
	return "", nil
}

// ingestInstance 按选择器取推流实例；未知 alias 或无推流能力回落内建 bellows（fellBack=true）。
// 回落意味着选择器配置无效：调用方不得据此做端点删除/重建等破坏性操作。
func (a *API) ingestInstance(ctx context.Context) (alias string, ip rtc.IngestProvider, fellBack bool) {
	if inst := a.instance(a.dynVal(ctx, "ingest_provider")); inst != nil && inst.Ingest != nil {
		return inst.Alias, inst.Ingest, false
	}
	return TypeBellows, a.instance(TypeBellows).Ingest, true
}

// migrationStep 一个版本步：v 单调递增；run 必须幂等（失败会在下次启动重入）。
type migrationStep struct {
	v   int
	run func(context.Context) error
}

// runMigrations 版本游标迁移入口：末尾照常重建注册表。
// 以后所有跨版本兼容处理都作为新版本步挂在这里。
func (a *API) runMigrations(ctx context.Context) {
	a.runMigrationSteps(ctx, []migrationStep{{1, a.migrateProviders}, {2, a.importSelectorEnv},
		{3, a.migrateIngestTokens}, {4, a.migrateEndpointIdentity}})
	a.reloadProviders(ctx)
}

// runMigrationSteps 按序执行版本号大于游标的步骤，每步成功后立刻写游标；
// 某步失败记日志并停止（不写游标，下次启动重试）。
func (a *API) runMigrationSteps(ctx context.Context, steps []migrationStep) {
	cur, err := a.st.MigrationVersion(ctx)
	if err != nil {
		log.Printf("读取迁移游标失败，本次跳过迁移: %v", err)
		return
	}
	for _, s := range steps {
		if s.v <= cur {
			continue
		}
		if err := s.run(ctx); err != nil {
			log.Printf("数据迁移 v%d 未完成（下次启动重试）: %v", s.v, err)
			break
		}
		if err := a.st.SetMigrationVersion(ctx, s.v); err != nil {
			log.Printf("写入迁移游标 v%d 失败（下次启动重试）: %v", s.v, err)
			break
		}
	}
}

// migrateProviders v1：Provider 注册制迁移——旧全局 cfg_ 键导入为 DB 实例、旧选择器值改写、
// 老部署的选择器默认落库（升级后行为不变）。
// 各子步骤幂等；任一部分失败返回错误，游标不前进，下次启动整体重试。
func (a *API) migrateProviders(ctx context.Context) error {
	// 旧 cfg_ 键导入（params 复用旧键名）：env 已锁定同名实例时不建行（DB 行会被 env 实例
	// 遮蔽、UI 不可见且不可删），只清旧键；导入失败保留旧键并返回错误，由游标重试。
	// 返回 true 表示该类型的实例来源已就位（新建/已存在/env 锁定）。
	importLegacy := func(typ string, params map[string]string, probes []string, clearKeys ...string) (bool, error) {
		if envLockedParams(a.providerTypeFields(typ), probes...) == nil {
			if _, err := a.st.ProviderByAlias(ctx, typ); err != nil {
				if cerr := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: typ, Type: typ, Params: params}); cerr != nil {
					log.Printf("迁移旧 %s 配置失败（旧 cfg_ 键保留，下次启动重试）: %v", typ, cerr)
					return false, cerr
				}
			}
		}
		for _, k := range clearKeys {
			a.st.SetSetting(ctx, "cfg_"+k, "")
		}
		return true, nil
	}
	lkParams := map[string]string{}
	for _, name := range []string{"livekit_api_url", "livekit_api_key", "livekit_api_secret", "livekit_url"} {
		if v, _ := a.st.GetSetting(ctx, "cfg_"+name); strings.TrimSpace(v) != "" {
			lkParams[name] = strings.TrimSpace(v)
		}
	}
	if len(lkParams) > 0 {
		backfillEnv(lkParams, a.providerTypeFields(TypeLivekit))
		if _, err := importLegacy(TypeLivekit, lkParams,
			[]string{"livekit_api_url", "livekit_api_key", "livekit_api_secret"},
			"livekit_api_url", "livekit_api_key", "livekit_api_secret", "livekit_url"); err != nil {
			return err
		}
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_ingress_upstream_url"); strings.TrimSpace(v) != "" {
		params := map[string]string{}
		for k, val := range lkParams {
			params[k] = val
		}
		params["ingress_upstream_url"] = strings.TrimSpace(v)
		backfillEnv(params, a.providerTypeFields(TypeLivekitIngress))
		if _, err := importLegacy(TypeLivekitIngress, params,
			[]string{"ingress_upstream_url"}, "ingress_upstream_url"); err != nil {
			return err
		}
	}
	remoteReady := os.Getenv("BELLOWS_REMOTE_URL") != ""
	if v, _ := a.st.GetSetting(ctx, "cfg_bellows_remote_url"); strings.TrimSpace(v) != "" {
		params := map[string]string{"bellows_remote_url": strings.TrimSpace(v)}
		if s, _ := a.st.GetSetting(ctx, "cfg_bellows_shared_secret"); strings.TrimSpace(s) != "" {
			params["bellows_shared_secret"] = strings.TrimSpace(s)
		}
		backfillEnv(params, a.providerTypeFields(TypeBellowsRemote))
		ok, err := importLegacy(TypeBellowsRemote, params,
			[]string{"bellows_remote_url"}, "bellows_remote_url", "bellows_shared_secret")
		if err != nil {
			return err
		}
		remoteReady = remoteReady || ok
	}

	// 选择器改写：livekit 的 ingress 面已拆成独立类型
	if v, _ := a.st.GetSetting(ctx, "cfg_ingest_provider"); v == "livekit" {
		a.st.SetSetting(ctx, "cfg_ingest_provider", TypeLivekitIngress)
	}
	// 老远端形态（ingest_provider=bellows + bellows_remote_url）：内建 bellows 不再读远端键，
	// 选择器改写指向 bellows-remote 实例，存量 ingress 记录归属一并改写
	if remoteReady {
		if v, _ := a.st.GetSetting(ctx, "cfg_ingest_provider"); v == TypeBellows {
			a.st.SetSetting(ctx, "cfg_ingest_provider", TypeBellowsRemote)
		}
	}

	// 选择器默认落库：老部署（后台未选过）原来默认跑 livekit，注册表默认是内建
	// ember/bellows，这里把旧默认写死，保证升级后行为不变。
	// 只随 v1 跑一次：管理员之后清空选择器恢复默认不会被重启撤销。
	hasProvider := func(alias, envProbe string) bool {
		if _, err := a.st.ProviderByAlias(ctx, alias); err == nil {
			return true
		}
		return envProbe != "" && os.Getenv(envProbe) != ""
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); strings.TrimSpace(v) == "" && hasProvider(TypeLivekit, "LIVEKIT_API_KEY") {
		a.st.SetSetting(ctx, "cfg_voice_provider", TypeLivekit)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_stage_provider"); strings.TrimSpace(v) == "" && hasProvider(TypeLivekit, "LIVEKIT_API_KEY") {
		a.st.SetSetting(ctx, "cfg_stage_provider", TypeLivekit)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_ingest_provider"); strings.TrimSpace(v) == "" && hasProvider(TypeLivekitIngress, "INGRESS_UPSTREAM_URL") {
		a.st.SetSetting(ctx, "cfg_ingest_provider", TypeLivekitIngress)
	}
	return nil
}

// importSelectorEnv 迁移 v2：选择器不再读环境变量（env 只负责把 provider 实例带进
// 可选列表）。为保证旧部署升级后行为不变，部署侧还设着的选择器 env 在后台从未选过时
// 把 env 值一次性落库；改名前残留（pion）与无对应能力的值（ingest 的 livekit）跳过，
// 由 warnLegacyConfig 打告警。只随 v2 跑一次：之后管理员清空恢复默认不被撤销。
func (a *API) importSelectorEnv(ctx context.Context) error {
	for name, env := range selectorEnv {
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" || v == "pion" || (name == "ingest_provider" && v == "livekit") {
			continue
		}
		if cur, _ := a.st.GetSetting(ctx, "cfg_"+name); strings.TrimSpace(cur) != "" {
			continue
		}
		if err := a.st.SetSetting(ctx, "cfg_"+name, v); err != nil {
			return err
		}
		log.Printf("配置迁移: %s=%q 已落库（选择器不再读环境变量，请从部署侧删除）", env, v)
	}
	return nil
}

// migrateIngestTokens v3：推流令牌改为每用户一把——旧 ingresses 表每用户取最近创建的一把
// stream_key 原值保留为其推流令牌（标签 obs；升级后 OBS 只需给服务器地址加频道段），
// 其余丢弃，然后 DROP ingresses。ingest_tokens 表由 Bun 迁移 00002 建好；
// 空库（无旧密钥）不建令牌。幂等：已有令牌的用户跳过，旧表已删视为空（半途重入安全）。
func (a *API) migrateIngestTokens(ctx context.Context) error {
	legacy, err := a.st.LegacyIngressTokens(ctx)
	if err != nil {
		return err
	}
	for userID, key := range legacy {
		if _, err := a.st.IngestTokenByUser(ctx, userID); err == nil {
			continue // 已有令牌（上次迁移半途重入或用户已新建），不覆盖
		}
		if err := a.st.ImportIngestToken(ctx, userID, key); err != nil {
			return err
		}
	}
	return a.st.DropIngresses(ctx)
}

// migrateEndpointIdentity v4：identity 主体由用户名改为 user_id（rtc.Identity），
// 存量上游端点里固化的 identity/name/metadata 全部过期——逐实例尽力 DeleteEndpoint
// 后清空 ingest_endpoints，下次推流按新 identity 惰性重建。
// 幂等：表已空时两步都是空操作。
func (a *API) migrateEndpointIdentity(ctx context.Context) error {
	// 必须先重建注册表：runMigrationSteps 跑在 New() 的 reloadProviders 之前，
	// 此刻注册表里只有内建实例，而持有上游端点的 livekit-ingress 是 env/DB 注册的——
	// 不重建就每条都取不到实例，一次 DeleteEndpoint 也不发，端点连同有效 stream key
	// 全部残留在上游且此后再也删不到。v1 建的 provider 行也要靠这次重建才可见。
	a.reloadProviders(ctx)
	eps, err := a.st.AllIngestEndpoints(ctx)
	if err != nil {
		return err
	}
	for _, ep := range eps {
		inst := a.instance(ep.Alias)
		if inst == nil || inst.Ingest == nil {
			continue // 实例已注销，内核侧删除无从下手
		}
		if derr := inst.Ingest.DeleteEndpoint(ctx, ep.IngressID); derr != nil {
			// 删不掉不阻塞迁移：记录清掉后下次推流会重建，残留端点由管理员在上游自行清理
			log.Printf("迁移 v4 删除旧 ingress 端点 %s（实例 %s）失败: %v", ep.IngressID, ep.Alias, derr)
		}
	}
	return a.st.DeleteAllIngestEndpoints(ctx)
}
