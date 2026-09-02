package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"hearth/server/internal/store"
)

// 选择器不读环境变量：env 的职责只是把 provider 实例带进可选列表。旧部署的选择器 env
// 由迁移 v2 一次性落库（升级行为不变），此后取值以管理后台/DB 为准。
func TestSelectorEnvImportedOnce(t *testing.T) {
	maskProviderEnv(t)                     // 不带 livekit 凭证：隔离 v1 的默认落库，只验 v2 的 env 导入
	t.Setenv("VOICE_PROVIDER", "livekit")  // 应被 v2 落库
	t.Setenv("INGEST_PROVIDER", "livekit") // 无推流能力的旧残留：跳过
	a := testAPI(t)                        // New 内跑迁移
	ctx := context.Background()

	if v := a.dynVal(ctx, "voice_provider"); v != "livekit" {
		t.Fatalf("旧 env 值应由迁移 v2 落库保持生效，实际 %q", v)
	}
	if v := a.dynVal(ctx, "ingest_provider"); v != TypeBellows {
		t.Fatalf("INGEST_PROVIDER=livekit 应跳过导入并回落内建 bellows，实际 %q", v)
	}
	// 落库值由管理员清空后，env 不再参与取值（重启重跑迁移也不应再落库）
	if err := a.st.SetSetting(ctx, "cfg_voice_provider", ""); err != nil {
		t.Fatalf("清选择器失败: %v", err)
	}
	a.runMigrations(ctx)
	if v := a.dynVal(ctx, "voice_provider"); v != TypeEmber {
		t.Fatalf("v2 已执行过后不得重复落库，应回落默认 ember，实际 %q", v)
	}
}

// 选择器不被 env 固定：后台可改、保存即生效。
func TestSelectorWritableWithEnvSet(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("LIVEKIT_API_KEY", "k")
	t.Setenv("LIVEKIT_API_SECRET", "s")
	t.Setenv("STAGE_PROVIDER", "livekit")
	a := testAPI(t)
	token := adminToken(t, a)
	r := a.Router()

	rec := doReq(t, r, "GET", "/api/admin/config", token, nil)
	if rec.Code != 200 {
		t.Fatalf("读配置失败: %d", rec.Code)
	}
	var resp struct {
		Items []struct {
			Name   string `json:"name"`
			Locked bool   `json:"locked"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	for _, it := range resp.Items {
		if it.Name == "stage_provider" && it.Locked {
			t.Fatal("选择器不应再被环境变量固定")
		}
	}
	rec = doReq(t, r, "POST", "/api/admin/config", token, map[string]any{"values": map[string]string{"stage_provider": "none"}})
	if rec.Code != 204 {
		t.Fatalf("选择器应可写，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if v := a.dynVal(context.Background(), "stage_provider"); v != "none" {
		t.Fatalf("写入未生效，实际 %q", v)
	}
}

// 注册新 provider 实例后应立即出现在选择器的可选列表里。
func TestRegisteredProviderJoinsSelectorOptions(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "bk-remote", Type: TypeBellowsRemote,
		Params: map[string]string{"bellows_remote_url": "http://10.0.0.5:8090", "bellows_shared_secret": "sec"}}); err != nil {
		t.Fatalf("注册实例失败: %v", err)
	}
	a.reloadProviders(ctx)
	opts := a.selectorOptions(ctx, "ingest_provider")
	if !slices.Contains(opts, "bk-remote") {
		t.Fatalf("新注册的实例应进入推流入口可选列表，实际 %v", opts)
	}
	// 无舞台能力的不应混入 stage 列表
	if slices.Contains(a.selectorOptions(ctx, "stage_provider"), "bk-remote") {
		t.Fatal("bellows-remote 无舞台能力，不应进入 stage 可选列表")
	}
}
