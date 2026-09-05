package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"hearth/server/internal/config"
	"hearth/server/internal/store"
)

func testAPI(t *testing.T) *API {
	t.Helper()
	a, _ := testAPIWithDB(t)
	return a
}

// testAPIWithDB 同 testAPI，另返回数据库文件路径（需要原始 SQL 造数/断言的迁移测试用）。
func testAPIWithDB(t *testing.T) (*API, string) {
	t.Helper()
	path := t.TempDir() + "/test.db"
	s, err := store.Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, config.Load(), nil, "dev"), path
}

func TestBuiltinInstancesFirst(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	a.reloadProviders(ctx)
	list := a.listInstances(ctx)
	if len(list) != 1 || list[0].Alias != AliasLkembed {
		t.Fatalf("内建实例应只有 lkembed: %+v", list)
	}
	if !list[0].Builtin || list[0].Voice == nil || list[0].Stage == nil || list[0].Ingest == nil {
		t.Fatal("lkembed 应为内建实例且语音/舞台/推流三面齐全")
	}
}

func TestEnvLockedInstance(t *testing.T) {
	t.Setenv("LIVEKIT_API_KEY", "k")
	t.Setenv("LIVEKIT_API_SECRET", "s")
	t.Setenv("LIVEKIT_API_URL", "http://10.0.0.2:7880")
	a := testAPI(t)
	a.reloadProviders(context.Background())
	inst := a.instance("livekit")
	if inst == nil || !inst.Locked || inst.Type != TypeLivekit {
		t.Fatalf("env 应合成锁定的 livekit 实例: %+v", inst)
	}
	if inst.Params["livekit_api_url"] != "http://10.0.0.2:7880" {
		t.Fatalf("params 应取自 env: %+v", inst.Params)
	}
}

func TestSelectorResolutionAndFallback(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	// 未设置选择器：默认 lkembed / lkembed（内建优先，默认解析不算回落）；
	// 推流无独立选择器，ingestInstance 跟随舞台实例
	if alias, _ := a.voiceInstance(ctx); alias != AliasLkembed {
		t.Fatalf("voice 默认应为 lkembed，实际 %q", alias)
	}
	if alias, sp := a.stageInstance(ctx); alias != AliasLkembed || sp == nil {
		t.Fatalf("stage 默认应为 lkembed，实际 %q", alias)
	}
	if alias, ip := a.ingestInstance(ctx); alias != AliasLkembed || ip == nil {
		t.Fatalf("ingest 应跟随舞台实例 lkembed，实际 %q", alias)
	}
	// 注册一套 livekit 并选中
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk2", Type: TypeLivekit,
		Params: map[string]string{"livekit_api_url": "http://x", "livekit_api_key": "k", "livekit_api_secret": "s"}})
	a.reloadProviders(ctx)
	a.st.SetSetting(ctx, "cfg_voice_provider", "lk2")
	if alias, _ := a.voiceInstance(ctx); alias != "lk2" {
		t.Fatalf("voice 应解析到 lk2，实际 %q", alias)
	}
	// 舞台切到 lk2：推流入口跟着切（livekit 实例三面齐全，推流走它自带的 WHIP 入口）
	a.st.SetSetting(ctx, "cfg_stage_provider", "lk2")
	if alias, ip := a.ingestInstance(ctx); alias != "lk2" || ip == nil {
		t.Fatalf("ingest 应跟随舞台实例 lk2，实际 %q", alias)
	}
	// 舞台线关闭：推流入口随之不存在
	a.st.SetSetting(ctx, "cfg_stage_provider", "none")
	if alias, ip := a.ingestInstance(ctx); alias != "" || ip != nil {
		t.Fatalf("stage=none 时 ingest 应为空，实际 %q ip=%v", alias, ip)
	}
	// 选了不存在的 alias → 回落内建默认
	a.st.SetSetting(ctx, "cfg_voice_provider", "nope")
	if alias, _ := a.voiceInstance(ctx); alias != AliasLkembed {
		t.Fatalf("未知 alias 应回落 lkembed，实际 %q", alias)
	}
}

func TestMigrateImportsLegacyCfg(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	// testAPI 的 New 已跑过全部迁移（空库游标 0→6）；重置游标并只重跑 v1，
	// 模拟从旧版本升级到注册制这一步（后续版本步会改写/删除这里断言的中间产物）
	a.st.SetMigrationVersion(ctx, 0)
	a.st.SetSetting(ctx, "cfg_livekit_api_url", "http://old:7880")
	a.st.SetSetting(ctx, "cfg_livekit_api_key", "k")
	a.st.SetSetting(ctx, "cfg_livekit_api_secret", "s")
	a.st.SetSetting(ctx, "cfg_ingress_upstream_url", "http://old:58080")
	a.runMigrationSteps(ctx, []migrationStep{{1, a.migrateProviders}})
	a.reloadProviders(ctx)
	if v, _ := a.st.MigrationVersion(ctx); v != 1 {
		t.Fatalf("v1 成功后游标应为 1，实际 %d", v)
	}
	if a.instance("livekit") == nil || a.instance("livekit").Locked {
		t.Fatal("旧 cfg_livekit_* 应导入为 DB 实例 livekit")
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_livekit_api_url"); v != "" {
		t.Fatal("导入后旧 cfg_ 键应删除")
	}
	// 退场类型的旧键 v1 不再导入为实例（livekit-ingress/bellows-remote 已删类型），
	// 留着由迁移 v6 清理
	if a.instance("livekit-ingress") != nil {
		t.Fatal("cfg_ingress_upstream_url 不应再导入为实例")
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_ingress_upstream_url"); v != "http://old:58080" {
		t.Fatalf("cfg_ingress_upstream_url 应原样留给 v6 清理，实际 %q", v)
	}
}

func TestReloadReusesUnchangedInstances(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	builtin := a.instance(AliasLkembed)
	if builtin == nil {
		t.Fatal("内建 lkembed 实例应存在")
	}
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk1", Type: TypeLivekit,
		Params: map[string]string{"livekit_api_url": "http://r1", "livekit_api_key": "k", "livekit_api_secret": "s"}})
	a.reloadProviders(ctx)
	if a.instance(AliasLkembed) != builtin {
		t.Fatal("未变化的内建实例应复用旧对象（活动会话不被 reload 打断）")
	}
	lk1 := a.instance("lk1")
	if lk1 == nil {
		t.Fatal("lk1 应已注册")
	}
	a.st.UpdateProviderParams(ctx, "lk1", map[string]string{
		"livekit_api_url": "http://r2", "livekit_api_key": "k", "livekit_api_secret": "s"})
	a.reloadProviders(ctx)
	if a.instance("lk1") == lk1 {
		t.Fatal("参数变化的实例应重建")
	}
	if a.instance(AliasLkembed) != builtin {
		t.Fatal("其余未变化实例仍应复用旧对象")
	}
	list := a.listInstances(ctx)
	if len(list) != 2 || list[0].Alias != AliasLkembed || list[1].Alias != "lk1" {
		t.Fatalf("实例顺序应保持 内建→DB: %+v", list)
	}
}

func TestMigratePinsLegacySelectors(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	a.st.SetMigrationVersion(ctx, 0)
	// 旧部署：只配了 livekit 系 cfg_ 键，从未显式设过选择器。
	// 只重跑 v1（后续版本步会删除/改写这里的部分产物，见迁移 v6）
	a.st.SetSetting(ctx, "cfg_livekit_api_key", "k")
	a.st.SetSetting(ctx, "cfg_livekit_api_secret", "s")
	a.runMigrationSteps(ctx, []migrationStep{{1, a.migrateProviders}})
	a.reloadProviders(ctx)
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != "livekit" {
		t.Fatalf("旧部署语音选择器应落库 livekit，实际 %q", v)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_stage_provider"); v != "livekit" {
		t.Fatalf("旧部署舞台选择器应落库 livekit，实际 %q", v)
	}
	// 选择器落库只随 v1 跑一次：管理员清空恢复默认后，重启（重跑迁移）不得再落库
	a.st.SetSetting(ctx, "cfg_voice_provider", "")
	a.runMigrations(ctx)
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != "" {
		t.Fatalf("v1 已执行过后不得重复落库，实际 %q", v)
	}
}

// 迁移步骤失败：记日志停止，后续步骤不执行，游标不前进（下次启动重试）。
func TestMigrationFailureKeepsCursor(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	a.st.SetMigrationVersion(ctx, 0)
	calls := 0
	a.runMigrationSteps(ctx, []migrationStep{
		{1, func(context.Context) error { calls++; return errors.New("boom") }},
		{2, func(context.Context) error { calls++; return nil }},
	})
	if calls != 1 {
		t.Fatalf("失败步骤之后不应再执行，实际执行 %d 步", calls)
	}
	if v, _ := a.st.MigrationVersion(ctx); v != 0 {
		t.Fatalf("失败时游标不应前进，实际 %d", v)
	}
	// 全部成功：按序执行、游标步进；重跑不再执行
	calls = 0
	a.runMigrationSteps(ctx, []migrationStep{
		{1, func(context.Context) error { calls++; return nil }},
		{2, func(context.Context) error { calls++; return nil }},
	})
	if calls != 2 {
		t.Fatalf("两个新步骤都应执行，实际 %d", calls)
	}
	if v, _ := a.st.MigrationVersion(ctx); v != 2 {
		t.Fatalf("游标应为 2，实际 %d", v)
	}
	a.runMigrationSteps(ctx, []migrationStep{
		{1, func(context.Context) error { calls++; return nil }},
		{2, func(context.Context) error { calls++; return nil }},
	})
	if calls != 2 {
		t.Fatalf("游标已覆盖的步骤不应重复执行，实际 %d", calls)
	}
}

func TestMigrateFreshDeployKeepsBuiltinDefaults(t *testing.T) {
	// 屏蔽真实环境里可能存在的内核变量，保证「全新部署」前提
	maskProviderEnv(t)
	a := testAPI(t) // New 里已完成全新部署的首次迁移（游标 0→6）
	ctx := context.Background()
	a.runMigrations(ctx) // 重跑幂等
	for _, k := range []string{"cfg_voice_provider", "cfg_stage_provider", "cfg_ingest_provider"} {
		if v, _ := a.st.GetSetting(ctx, k); v != "" {
			t.Fatalf("全新部署不应落库选择器 %s，实际 %q", k, v)
		}
	}
	if alias, vp := a.voiceInstance(ctx); alias != AliasLkembed || vp == nil {
		t.Fatalf("全新部署语音应走内建 lkembed，实际 %q", alias)
	}
	if alias, sp := a.stageInstance(ctx); alias != AliasLkembed || sp == nil {
		t.Fatalf("全新部署舞台应走内建 lkembed，实际 %q", alias)
	}
	if alias, ip := a.ingestInstance(ctx); alias != AliasLkembed || ip == nil {
		t.Fatalf("全新部署推流应跟随舞台实例 lkembed，实际 %q", alias)
	}
}

// ember 信令入口已随内核一并退场：/providers/ember/voice 一律 404（实例本身不存在）。
func TestEmberVoiceEndpointGone(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	r := a.Router()
	a.RegisterProxies(r)
	for _, method := range []string{"GET", "POST"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/providers/ember/voice", nil))
		if rec.Code != 404 {
			t.Fatalf("%s /providers/ember/voice 应 404，实际 %d", method, rec.Code)
		}
	}
}

// ListProviders 失败时保留旧注册表：DB 实例不得消失。
func TestReloadKeepsRegistryOnListFailure(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk1", Type: TypeLivekit, Params: lkParams})
	a.reloadProviders(ctx)
	if a.instance("lk1") == nil {
		t.Fatal("lk1 应已加载")
	}
	a.st.Close() // 让后续 ListProviders 失败
	a.reloadProviders(ctx)
	if a.instance("lk1") == nil {
		t.Fatal("ListProviders 失败时应保留旧注册表")
	}
}

// env 只设 LIVEKIT_URL+KEY+SECRET（不设 LIVEKIT_API_URL）时，env 锁定 livekit 实例读到的
// livekit_api_url 应回落字段模式声明的 Default（apiURLDefault 推导值）。
func TestLivekitAPIURLDefaultOnInstancePaths(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("LIVEKIT_URL", "wss://lk.example.com")
	t.Setenv("LIVEKIT_API_KEY", "k")
	t.Setenv("LIVEKIT_API_SECRET", "s")
	a := testAPI(t)
	ctx := context.Background()
	const want = "https://lk.example.com"
	inst := a.instance("livekit")
	if inst == nil {
		t.Fatal("LIVEKIT_API_KEY/SECRET 应合成 env 锁定 livekit 实例")
	}
	if got := inst.Cfg(ctx, "livekit_api_url"); got != want {
		t.Fatalf("livekit 实例的 api_url 应取 Default 推导值 %q，实际 %q", want, got)
	}
}

// 首次启动 ListProviders 失败（DB 不可用）：注册表保留 New 种下的内建实例，
// voiceInstance/ingestInstance 的取值路径不 panic、返回可用对象。
func TestFallbacksSafeWhenInitialReloadFails(t *testing.T) {
	maskProviderEnv(t)
	s, err := store.Open("sqlite://" + t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.Close() // 模拟启动期 DB 不可用（迁移与首次 ListProviders 都会失败）
	a := New(s, config.Load(), nil, "dev")
	ctx := context.Background()
	// 配置读不到 → 落到默认选择器：语音 lkembed（内建对象在 New 已种下，非 nil）
	if alias, vp := a.voiceInstance(ctx); alias != AliasLkembed || vp == nil {
		t.Fatalf("启动期加载失败时语音应落默认 lkembed 且非 nil: %q", alias)
	}
	// 推流跟随舞台实例：舞台默认 lkembed，其推流面同样非 nil
	alias, ip := a.ingestInstance(ctx)
	if alias != AliasLkembed || ip == nil {
		t.Fatalf("启动期加载失败时推流应跟随 lkembed 且非 nil: %q", alias)
	}
	_ = ip.Enabled(ctx) // 调用路径不得 panic
}

// 内建 lkembed：可被舞台选择器选中，配置映射到回环，密钥首次生成后落库不变。
func TestLkembedInstanceWiring(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()

	if msg := a.checkSelector(ctx, "stage_provider", AliasLkembed); msg != "" {
		t.Fatalf("lkembed 应可选为舞台内核: %s", msg)
	}
	if err := a.st.SetSetting(ctx, "cfg_stage_provider", AliasLkembed); err != nil {
		t.Fatal(err)
	}
	if alias, sp := a.stageInstance(ctx); alias != AliasLkembed || sp == nil {
		t.Fatalf("舞台实例应为 lkembed，实际 %q", alias)
	}
	if got := a.embedCfg(ctx, "livekit_api_url"); got != "http://127.0.0.1:47730" {
		t.Fatalf("API 地址应指向回环默认端口，实际 %q", got)
	}

	key, secret, err := a.ensureEmbedKeys(ctx)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	if key == "" || len(secret) < 32 {
		t.Fatalf("密钥不合法: key=%q secretLen=%d", key, len(secret))
	}
	key2, secret2, err := a.ensureEmbedKeys(ctx)
	if err != nil || key2 != key || secret2 != secret {
		t.Fatalf("密钥应落库后不变: %q/%q err=%v", key2, secret2, err)
	}
	if got := a.embedCfg(ctx, "livekit_api_secret"); got != secret {
		t.Fatalf("配置映射应读到落库的 secret，实际 %q", got)
	}
}
