// 迁移 v6（内核收敛）的测试：选择器改写、退场实例行删除、旧全局键删除、
// ingest_endpoints 清空、幂等重入。
package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"testing"

	"hearth/server/internal/store"
)

// rerunV6 把游标拨回 5 并重跑迁移（New 已在空库上跑完全部迁移，这里模拟旧库升级）。
func rerunV6(t *testing.T, a *API) {
	t.Helper()
	if err := a.st.SetMigrationVersion(context.Background(), 5); err != nil {
		t.Fatalf("拨游标失败: %v", err)
	}
	a.runMigrations(context.Background())
	if v, _ := a.st.MigrationVersion(context.Background()); v != 6 {
		t.Fatalf("迁移成功后游标应为 6，实际 %d", v)
	}
}

// 选择器改写：voice 为 ember/pion/bellows/不存在的 alias → lkembed；
// stage 显式 none 保持 none，指向已删类型实例或不存在的 alias → lkembed；
// 指向 livekit 类型实例的选择器保留。
func TestMigrateV6SelectorRewrite(t *testing.T) {
	maskProviderEnv(t)
	for _, tc := range []struct {
		name      string
		voice     string
		stage     string
		ingress   bool // 是否注册一条 livekit-ingress 实例并把 stage 指向它
		wantVoice string
		wantStage string
	}{
		{"voice 旧值 ember", "ember", "", false, AliasLkembed, ""},
		{"voice 改名前残留 pion", "pion", "", false, AliasLkembed, ""},
		{"voice 旧值 bellows", "bellows", "", false, AliasLkembed, ""},
		{"voice 不存在的 alias", "nope", "", false, AliasLkembed, ""},
		{"stage 显式 none 保持", "", "none", false, "", "none"},
		{"stage 指向已删类型实例", "", "", true, "", AliasLkembed},
		{"stage 不存在的 alias", "", "nope", false, "", AliasLkembed},
		{"都为空不落库（新默认 lkembed 生效）", "", "", false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			maskProviderEnv(t)
			a := testAPI(t)
			ctx := context.Background()
			if tc.voice != "" {
				a.st.SetSetting(ctx, "cfg_voice_provider", tc.voice)
			}
			if tc.stage != "" {
				a.st.SetSetting(ctx, "cfg_stage_provider", tc.stage)
			}
			if tc.ingress {
				// 类型名按历史字面量写：对应常量已随实现一并删除，v6 按字面量匹配删行
				if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "ing1", Type: "livekit-ingress",
					Params: map[string]string{"ingress_upstream_url": "http://x:58080"}}); err != nil {
					t.Fatalf("造 ingress 实例失败: %v", err)
				}
				a.st.SetSetting(ctx, "cfg_stage_provider", "ing1")
			}
			rerunV6(t, a)
			if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != tc.wantVoice {
				t.Fatalf("voice 选择器应为 %q，实际 %q", tc.wantVoice, v)
			}
			if v, _ := a.st.GetSetting(ctx, "cfg_stage_provider"); v != tc.wantStage {
				t.Fatalf("stage 选择器应为 %q，实际 %q", tc.wantStage, v)
			}
		})
	}
}

// 选择器指向 livekit 类型实例（DB 注册）时保留——旧部署行为不变。
func TestMigrateV6KeepsLivekitSelectors(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk1", Type: TypeLivekit, Params: lkParams}); err != nil {
		t.Fatalf("造 livekit 实例失败: %v", err)
	}
	a.st.SetSetting(ctx, "cfg_voice_provider", "lk1")
	a.st.SetSetting(ctx, "cfg_stage_provider", "lk1")
	rerunV6(t, a)
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != "lk1" {
		t.Fatalf("指向 livekit 实例的 voice 选择器应保留，实际 %q", v)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_stage_provider"); v != "lk1" {
		t.Fatalf("指向 livekit 实例的 stage 选择器应保留，实际 %q", v)
	}
}

// 清理动作：cfg_ingest_provider 键删除；providers 表退场类型的行删除（逐条日志）；
// cfg_ember_*/cfg_bellows_*/cfg_pion_* 与 cfg_ingress_upstream_url 删除；ingest_endpoints 清空；其余键不动。
func TestMigrateV6Cleanup(t *testing.T) {
	maskProviderEnv(t)
	a, dbPath := testAPIWithDB(t)
	ctx := context.Background()

	a.st.SetSetting(ctx, "cfg_ingest_provider", "bellows")
	for _, k := range []string{"cfg_ember_udp_port", "cfg_ember_public_ip",
		"cfg_bellows_udp_port", "cfg_pion_udp_port", "cfg_ingress_upstream_url"} {
		a.st.SetSetting(ctx, k, "1")
	}
	a.st.SetSetting(ctx, "cfg_portmap_mode", "off") // 非退场内核的键：应保留
	for _, rec := range []*store.ProviderRecord{
		// 退场类型名按历史字面量写（常量已随实现删除）
		{Alias: "ing1", Type: "livekit-ingress", Params: map[string]string{"ingress_upstream_url": "http://x:58080"}},
		{Alias: "br1", Type: "bellows-remote", Params: map[string]string{"bellows_remote_url": "http://r1"}},
		{Alias: "lk1", Type: TypeLivekit, Params: lkParams},
	} {
		if err := a.st.CreateProvider(ctx, rec); err != nil {
			t.Fatalf("造实例 %s 失败: %v", rec.Alias, err)
		}
	}
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	tok, err := a.st.CreateIngestToken(ctx, u.ID, "obs")
	if err != nil {
		t.Fatalf("建令牌失败: %v", err)
	}
	// 造一条存量端点（表读写方法已随类型删除，直接写表）
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开原始连接失败: %v", err)
	}
	if _, err := raw.Exec(
		"INSERT INTO ingest_endpoints (token_id, alias, ingress_id, upstream_key, bound_room) VALUES (?, ?, ?, ?, ?)",
		tok.ID, "ing1", "ing_x", "k", "chan1"); err != nil {
		t.Fatalf("造端点失败: %v", err)
	}
	raw.Close()

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	rerunV6(t, a)

	if _, err := a.st.GetSetting(ctx, "cfg_ingest_provider"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cfg_ingest_provider 键应被删除，err=%v", err)
	}
	for _, k := range []string{"cfg_ember_udp_port", "cfg_ember_public_ip",
		"cfg_bellows_udp_port", "cfg_pion_udp_port", "cfg_ingress_upstream_url"} {
		if _, err := a.st.GetSetting(ctx, k); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s 应被删除，err=%v", k, err)
		}
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_portmap_mode"); v != "off" {
		t.Fatalf("非退场内核的键不应被动，实际 %q", v)
	}
	if _, err := a.st.ProviderByAlias(ctx, "ing1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("livekit-ingress 实例行应被删除，err=%v", err)
	}
	if _, err := a.st.ProviderByAlias(ctx, "br1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bellows-remote 实例行应被删除，err=%v", err)
	}
	if _, err := a.st.ProviderByAlias(ctx, "lk1"); err != nil {
		t.Fatalf("livekit 实例行应保留，err=%v", err)
	}
	if out := logBuf.String(); !bytes.Contains(logBuf.Bytes(), []byte("迁移 v6")) ||
		!bytes.Contains([]byte(out), []byte("ing1")) || !bytes.Contains([]byte(out), []byte("br1")) {
		t.Fatalf("迁移日志应逐条记录删除的实例: %q", out)
	}
	raw, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开原始连接失败: %v", err)
	}
	defer raw.Close()
	var epCount int
	if err := raw.QueryRow("SELECT COUNT(*) FROM ingest_endpoints").Scan(&epCount); err != nil || epCount != 0 {
		t.Fatalf("ingest_endpoints 应清空: n=%d err=%v", epCount, err)
	}
	// 迁移后注册表重建：退场实例消失，livekit 实例仍在
	if a.instance("ing1") != nil || a.instance("br1") != nil {
		t.Fatal("退场实例不应再出现在注册表")
	}
	if a.instance("lk1") == nil {
		t.Fatal("livekit 实例应仍在注册表")
	}
}

// 幂等：重复执行 v6 不出错、结果不变（游标失败重入/手工重跑场景）。
func TestMigrateV6Idempotent(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	a.st.SetSetting(ctx, "cfg_voice_provider", "ember")
	a.st.SetSetting(ctx, "cfg_stage_provider", "none")
	a.st.SetSetting(ctx, "cfg_ingest_provider", "bellows")
	a.st.SetSetting(ctx, "cfg_ember_udp_port", "47700")
	if err := a.migrateKernelConsolidation(ctx); err != nil {
		t.Fatalf("首次执行失败: %v", err)
	}
	if err := a.migrateKernelConsolidation(ctx); err != nil {
		t.Fatalf("重复执行失败: %v", err)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_voice_provider"); v != AliasLkembed {
		t.Fatalf("voice 应改写为 lkembed，实际 %q", v)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_stage_provider"); v != "none" {
		t.Fatalf("stage 显式 none 应保持，实际 %q", v)
	}
}
