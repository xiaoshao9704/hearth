package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"hearth/server/internal/store"
)

// 选择器不读环境变量：env 的职责只是把 provider 实例带进可选列表。旧部署的选择器 env
// 由迁移 v2 一次性落库（升级行为不变），此后取值以管理后台/DB 为准。
func TestSelectorEnvImportedOnce(t *testing.T) {
	maskProviderEnv(t)                    // 不带 livekit 凭证：隔离 v1 的默认落库，只验 v2 的 env 导入
	t.Setenv("VOICE_PROVIDER", "livekit") // 应被 v2 落库
	t.Setenv("INGEST_PROVIDER", "bellows") // 推流选择器已删除：不再导入，也不再读取
	a := testAPI(t)                       // New 内跑迁移
	ctx := context.Background()

	// v2 应把 env 值落库；但 livekit 实例不存在（env 已屏蔽），迁移 v5 随即将这个
	// 悬空选择器改写为 lkembed——落库值为 lkembed 即证明 v2 导入过（否则键为空）。
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != AliasLkembed {
		t.Fatalf("旧 env 值应由迁移 v2 落库并被 v5 改写为 lkembed，实际 %q", v)
	}
	// INGEST_PROVIDER 是已删除的选择器：不导入（cfg_ingest_provider 由 v5 删除）、不参与取值
	if v, err := a.st.GetSetting(ctx, "cfg_ingest_provider"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cfg_ingest_provider 不应存在，实际 %q err=%v", v, err)
	}
	// 落库值由管理员清空后，env 不再参与取值（重启重跑迁移也不应再落库）
	if err := a.st.SetSetting(ctx, "cfg_voice_provider", ""); err != nil {
		t.Fatalf("清选择器失败: %v", err)
	}
	a.runMigrations(ctx)
	if v := a.dynVal(ctx, "voice_provider"); v != AliasLkembed {
		t.Fatalf("v2 已执行过后不得重复落库，应回落默认 lkembed，实际 %q", v)
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

// 注册新 provider 实例后应立即出现在选择器的可选列表里；推流选择器已删除，
// 只剩 voice/stage 两个槽位。
func TestRegisteredProviderJoinsSelectorOptions(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk-remote", Type: TypeLivekit, Params: lkParams}); err != nil {
		t.Fatalf("注册实例失败: %v", err)
	}
	a.reloadProviders(ctx)
	for _, sel := range []string{"voice_provider", "stage_provider"} {
		if opts := a.selectorOptions(ctx, sel); !slices.Contains(opts, "lk-remote") {
			t.Fatalf("livekit 实例应进入 %s 可选列表，实际 %v", sel, opts)
		}
	}
}
