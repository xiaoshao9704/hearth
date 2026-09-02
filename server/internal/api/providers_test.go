package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"hearth/server/internal/chat"
	"hearth/server/internal/config"
	"hearth/server/internal/store"
)

func testAPI(t *testing.T) *API {
	t.Helper()
	s, err := store.Open("sqlite://" + t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, config.Load(), chat.NewHub(s, ""))
}

func TestBuiltinInstancesFirst(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	a.reloadProviders(ctx)
	list := a.listInstances(ctx)
	if len(list) < 2 || list[0].Alias != "ember" || list[1].Alias != "bellows" {
		t.Fatalf("内建实例应排最前: %+v", list)
	}
	if !list[0].Builtin || list[0].Voice == nil || list[1].Ingest == nil {
		t.Fatal("ember 应有语音能力，bellows 应有推流能力")
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
	// 未设置选择器：默认 ember / none / bellows（内建优先，默认解析不算回落）
	if alias, _ := a.voiceInstance(ctx); alias != "ember" {
		t.Fatalf("voice 默认应为 ember，实际 %q", alias)
	}
	if _, sp := a.stageInstance(ctx); sp != nil {
		t.Fatal("stage 默认应为 none")
	}
	if alias, _, fellBack := a.ingestInstance(ctx); alias != "bellows" || fellBack {
		t.Fatalf("ingest 默认应为 bellows 且非回落，实际 %q fellBack=%v", alias, fellBack)
	}
	// 注册一套 livekit 并选中
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk2", Type: TypeLivekit,
		Params: map[string]string{"livekit_api_url": "http://x", "livekit_api_key": "k", "livekit_api_secret": "s"}})
	a.reloadProviders(ctx)
	a.st.SetSetting(ctx, "cfg_voice_provider", "lk2")
	if alias, _ := a.voiceInstance(ctx); alias != "lk2" {
		t.Fatalf("voice 应解析到 lk2，实际 %q", alias)
	}
	// 选了无对应槽位能力的实例 → 回落
	a.st.SetSetting(ctx, "cfg_voice_provider", "lk2")
	a.st.SetSetting(ctx, "cfg_ingest_provider", "lk2") // livekit 无推流能力
	if alias, _, fellBack := a.ingestInstance(ctx); alias != "bellows" || !fellBack {
		t.Fatalf("无推流能力的实例应回落 bellows 且 fellBack=true，实际 %q fellBack=%v", alias, fellBack)
	}
}

func TestMigrateImportsLegacyCfg(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	// testAPI 的 New 已跑过 v1（空库游标 0→1）；重置游标模拟从旧版本升级
	a.st.SetMigrationVersion(ctx, 0)
	a.st.SetSetting(ctx, "cfg_livekit_api_url", "http://old:7880")
	a.st.SetSetting(ctx, "cfg_livekit_api_key", "k")
	a.st.SetSetting(ctx, "cfg_livekit_api_secret", "s")
	a.st.SetSetting(ctx, "cfg_ingest_provider", "livekit") // 旧值：livekit 的 ingress 面
	a.st.SetSetting(ctx, "cfg_ingress_upstream_url", "http://old:58080")
	// 旧库里归属为实现名 livekit 的 ingress 记录
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	c, err := a.st.CreateChannel(ctx, "chan1", u.ID)
	if err != nil {
		t.Fatalf("造频道失败: %v", err)
	}
	if _, err := a.st.CreateIngress(ctx, u.ID, c.ID, "ing1", "key1", "livekit"); err != nil {
		t.Fatalf("造 ingress 失败: %v", err)
	}
	a.runMigrations(ctx)
	if v, _ := a.st.MigrationVersion(ctx); v != 1 {
		t.Fatalf("迁移成功后游标应为 1，实际 %d", v)
	}
	if a.instance("livekit") == nil || a.instance("livekit").Locked {
		t.Fatal("旧 cfg_livekit_* 应导入为 DB 实例 livekit")
	}
	ing := a.instance("livekit-ingress")
	if ing == nil || ing.Params["ingress_upstream_url"] != "http://old:58080" {
		t.Fatalf("ingress 旧键应导入 livekit-ingress 实例: %+v", ing)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_livekit_api_url"); v != "" {
		t.Fatal("导入后旧 cfg_ 键应删除")
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_ingest_provider"); v != "livekit-ingress" {
		t.Fatalf("旧选择器值应改写为 livekit-ingress，实际 %q", v)
	}
	rec, err := a.st.IngressByUserChannel(ctx, u.ID, c.ID)
	if err != nil || rec.Provider != "livekit-ingress" {
		t.Fatalf("ingress 记录归属应改写为 livekit-ingress: %+v err=%v", rec, err)
	}
}

func TestReloadReusesUnchangedInstances(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	builtin := a.instance(TypeBellows)
	if builtin == nil {
		t.Fatal("内建 bellows 实例应存在")
	}
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "br1", Type: TypeBellowsRemote,
		Params: map[string]string{"bellows_remote_url": "http://r1"}})
	a.reloadProviders(ctx)
	if a.instance(TypeBellows) != builtin {
		t.Fatal("未变化的内建实例应复用旧对象（活动会话不被 reload 打断）")
	}
	br1 := a.instance("br1")
	if br1 == nil {
		t.Fatal("br1 应已注册")
	}
	a.st.UpdateProviderParams(ctx, "br1", map[string]string{"bellows_remote_url": "http://r2"})
	a.reloadProviders(ctx)
	if a.instance("br1") == br1 {
		t.Fatal("参数变化的实例应重建")
	}
	if a.instance(TypeBellows) != builtin {
		t.Fatal("其余未变化实例仍应复用旧对象")
	}
	list := a.listInstances(ctx)
	if len(list) < 3 || list[0].Alias != "ember" || list[1].Alias != "bellows" || list[2].Alias != "br1" {
		t.Fatalf("实例顺序应保持 内建→DB: %+v", list)
	}
}

func TestMigratePinsLegacySelectors(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	a.st.SetMigrationVersion(ctx, 0)
	// 旧部署：只配了 livekit 系 cfg_ 键，从未显式设过选择器
	a.st.SetSetting(ctx, "cfg_livekit_api_key", "k")
	a.st.SetSetting(ctx, "cfg_livekit_api_secret", "s")
	a.st.SetSetting(ctx, "cfg_ingress_upstream_url", "http://old:58080")
	a.runMigrations(ctx)
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != "livekit" {
		t.Fatalf("旧部署语音选择器应落库 livekit，实际 %q", v)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_stage_provider"); v != "livekit" {
		t.Fatalf("旧部署舞台选择器应落库 livekit，实际 %q", v)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_ingest_provider"); v != "livekit-ingress" {
		t.Fatalf("旧部署推流选择器应落库 livekit-ingress，实际 %q", v)
	}
	// 选择器落库只随 v1 跑一次：管理员清空恢复默认后，重启（重跑迁移）不得再落库
	a.st.SetSetting(ctx, "cfg_voice_provider", "")
	a.runMigrations(ctx)
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != "" {
		t.Fatalf("v1 已执行过后不得重复落库，实际 %q", v)
	}
}

// 老远端形态（ingest_provider=bellows + bellows_remote_url）：v1 改写选择器与
// 存量 ingress 记录归属到 bellows-remote。env 组合（INGRESS_PROVIDER=bellows 且
// BELLOWS_REMOTE_URL 已设）无法改写 env，由部署侧手动改配置解决，不加告警。
func TestMigrateRemoteBellowsSelector(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	a.st.SetMigrationVersion(ctx, 0)
	a.st.SetSetting(ctx, "cfg_ingest_provider", "bellows")
	a.st.SetSetting(ctx, "cfg_bellows_remote_url", "http://10.0.0.5:8090")
	a.st.SetSetting(ctx, "cfg_bellows_shared_secret", "sec")
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	c, err := a.st.CreateChannel(ctx, "chan1", u.ID)
	if err != nil {
		t.Fatalf("造频道失败: %v", err)
	}
	if _, err := a.st.CreateIngress(ctx, u.ID, c.ID, "ing1", "key1", "bellows"); err != nil {
		t.Fatalf("造 ingress 失败: %v", err)
	}
	a.runMigrations(ctx)
	if v, _ := a.st.GetSetting(ctx, "cfg_ingest_provider"); v != "bellows-remote" {
		t.Fatalf("老远端形态选择器应改写为 bellows-remote，实际 %q", v)
	}
	rec, err := a.st.IngressByUserChannel(ctx, u.ID, c.ID)
	if err != nil || rec.Provider != "bellows-remote" {
		t.Fatalf("ingress 记录归属应改写为 bellows-remote: %+v err=%v", rec, err)
	}
	inst := a.instance("bellows-remote")
	if inst == nil || inst.Params["bellows_remote_url"] != "http://10.0.0.5:8090" {
		t.Fatalf("远端配置应导入 bellows-remote 实例: %+v", inst)
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
	a := testAPI(t) // New 里已完成全新部署的首次迁移（游标 0→1）
	ctx := context.Background()
	a.runMigrations(ctx) // 重跑幂等
	for _, k := range []string{"cfg_voice_provider", "cfg_stage_provider", "cfg_ingest_provider"} {
		if v, _ := a.st.GetSetting(ctx, k); v != "" {
			t.Fatalf("全新部署不应落库选择器 %s，实际 %q", k, v)
		}
	}
	if alias, _ := a.voiceInstance(ctx); alias != "ember" {
		t.Fatalf("全新部署语音应走内建 ember，实际 %q", alias)
	}
	if alias, _, _ := a.ingestInstance(ctx); alias != "bellows" {
		t.Fatalf("全新部署推流应走内建 bellows，实际 %q", alias)
	}
}

// 选择器取未知值时语音回落 ember：joinToken 签的是 ember 票，/providers/ember/voice
// 不得按原始选择器串 409（无 token → 401 即证明过了守卫）。
func TestVoiceWSFallbackSelector(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	a.st.SetSetting(context.Background(), "cfg_voice_provider", "nope")
	r := a.Router()
	a.RegisterProxies(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/ember/voice", nil))
	if rec.Code == 409 {
		t.Fatal("选择器未知值回落 ember 时不应 409")
	}
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401，实际 %d", rec.Code)
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

// env 只设 LIVEKIT_URL+KEY+SECRET（不设 LIVEKIT_API_URL）时，livekit-ingress 实例与
// 内建 bellows 读到的 livekit_api_url 都应回落字段模式声明的 Default（apiURLDefault 推导值）。
func TestLivekitAPIURLDefaultOnInstancePaths(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("LIVEKIT_URL", "wss://lk.example.com")
	t.Setenv("LIVEKIT_API_KEY", "k")
	t.Setenv("LIVEKIT_API_SECRET", "s")
	t.Setenv("INGRESS_UPSTREAM_URL", "http://10.0.0.9:58080")
	a := testAPI(t)
	ctx := context.Background()
	const want = "https://lk.example.com"
	ing := a.instance("livekit-ingress")
	if ing == nil {
		t.Fatal("INGRESS_UPSTREAM_URL 应合成 env 锁定 livekit-ingress 实例")
	}
	if got := ing.Cfg(ctx, "livekit_api_url"); got != want {
		t.Fatalf("livekit-ingress 的 api_url 应取 Default 推导值 %q，实际 %q", want, got)
	}
	// v1 已把 stage 选择器落库 livekit（env 探测到凭证），内建 bellows 路由到该实例
	if got := a.builtinBellowsCfg(ctx, "livekit_api_url"); got != want {
		t.Fatalf("内建 bellows 的 api_url 应取 Default 推导值 %q，实际 %q", want, got)
	}
}

// env 残留 INGEST_PROVIDER=livekit（无推流能力 → 回落内建 bellows）时：
// getIngress 不做归属自愈——不删端点、不换 key，按记录原归属实例返回地址。
func TestGetIngressFallbackKeepsEndpoint(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("LIVEKIT_API_KEY", "k")
	t.Setenv("LIVEKIT_API_SECRET", "s")
	t.Setenv("INGEST_PROVIDER", "livekit") // 旧 env 残留：livekit 已无推流能力
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	c, err := a.st.CreateChannel(ctx, "chan1", u.ID)
	if err != nil {
		t.Fatalf("造频道失败: %v", err)
	}
	if _, err := a.st.CreateIngress(ctx, u.ID, c.ID, "ing1", "keepkey", "livekit-ingress"); err != nil {
		t.Fatalf("造 ingress 失败: %v", err)
	}
	// 记录归属的实例在册（否则按「配置无效」503）
	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "livekit-ingress", Type: TypeLivekitIngress,
		Params: map[string]string{"livekit_api_url": "http://x", "livekit_api_key": "k",
			"livekit_api_secret": "s", "ingress_upstream_url": "http://up"}}); err != nil {
		t.Fatalf("造实例失败: %v", err)
	}
	token, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("签发会话失败: %v", err)
	}
	a.reloadProviders(ctx)
	r := a.Router()

	rec := doReq(t, r, "POST", "/api/ingress", token, map[string]any{"channel": "chan1"})
	if rec.Code != 200 {
		t.Fatalf("回落保护下应返回既有地址 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var resp ingressResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.StreamKey != "keepkey" {
		t.Fatalf("回落状态下不得重建密钥，实际 %q", resp.StreamKey)
	}
	if !strings.Contains(resp.URL, "/providers/livekit-ingress/w/keepkey") {
		t.Fatalf("地址应按记录原归属实例推导，实际 %q", resp.URL)
	}
	dbRec, err := a.st.IngressByUserChannel(ctx, u.ID, c.ID)
	if err != nil || dbRec.Provider != "livekit-ingress" || dbRec.IngressID != "ing1" {
		t.Fatalf("回落状态下不得删除重建端点: %+v err=%v", dbRec, err)
	}

	// 回落且无存量记录：不创建端点，503「推流入口配置无效」（与 resetIngress 前置拦截同口径）
	u2, err := a.st.CreateUser(ctx, "bob", "x")
	if err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	tok2, err := a.st.CreateSession(ctx, u2.ID)
	if err != nil {
		t.Fatalf("签发会话失败: %v", err)
	}
	rec = doReq(t, r, "POST", "/api/ingress", tok2, map[string]any{"channel": "chan1"})
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "推流入口配置无效") {
		t.Fatalf("回落状态下创建端点应 503 配置无效，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 首次启动 ListProviders 失败（DB 不可用）：注册表保留 New 种下的内建实例，
// voiceInstance/ingestInstance 的回落路径不 panic、返回可用对象。
func TestFallbacksSafeWhenInitialReloadFails(t *testing.T) {
	maskProviderEnv(t)
	s, err := store.Open("sqlite://" + t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.Close() // 模拟启动期 DB 不可用（迁移与首次 ListProviders 都会失败）
	a := New(s, config.Load(), chat.NewHub(s, ""))
	ctx := context.Background()
	if alias, vp := a.voiceInstance(ctx); alias != "ember" || vp == nil {
		t.Fatalf("启动期加载失败时语音应回落 ember 且非 nil: %q", alias)
	}
	alias, ip, _ := a.ingestInstance(ctx)
	if alias != "bellows" || ip == nil {
		t.Fatalf("启动期加载失败时推流应回落 bellows 且非 nil: %q", alias)
	}
	_ = ip.Enabled(ctx) // 回落对象的调用路径不得 panic
}
