package api

import (
	"context"
	"encoding/json"
	"testing"

	"hearth/server/internal/store"
)

// TestPortWantsLkembedStage lkembed 的媒体端口 want 在语音/舞台选择器切换时相应增删，
// 且必须 StrictPort（补丁二的地址改写只换 IP 不换端口）。
// 语音线或舞台线任一选中 lkembed 都需要该端口；两线都切走（voice→外部实例、stage→none）才消失。
func TestPortWantsLkembedStage(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()

	findStage := func() (found bool, strict bool) {
		for _, w := range a.PortWants(ctx) {
			if w.Desc == "hearth stage" && w.Proto == "udp" {
				return true, w.StrictPort
			}
		}
		return false, false
	}

	// 默认 voice/stage 同选 lkembed：应出现 udp want 且 StrictPort=true。
	found, strict := findStage()
	if !found {
		t.Fatal("默认配置下 PortWants 应包含 lkembed 媒体端口的 udp want")
	}
	if !strict {
		t.Fatal("lkembed 的媒体端口 want 必须 StrictPort=true")
	}

	// ICE-TCP 默认开（与媒体 UDP 同号），应有一条 StrictPort 的 tcp want。
	var tcpFound, tcpStrict bool
	for _, w := range a.PortWants(ctx) {
		if w.Desc == "hearth stage" && w.Proto == "tcp" {
			tcpFound, tcpStrict = true, w.StrictPort
		}
	}
	if !tcpFound || !tcpStrict {
		t.Fatalf("默认应出现 StrictPort 的 ICE-TCP want: found=%v strict=%v", tcpFound, tcpStrict)
	}

	// 显式设成 0 即关闭，tcp want 随之消失。
	if err := a.st.SetSetting(ctx, "cfg_lkembed_tcp_port", "0"); err != nil {
		t.Fatalf("落库 lkembed_tcp_port 失败: %v", err)
	}
	for _, w := range a.PortWants(ctx) {
		if w.Desc == "hearth stage" && w.Proto == "tcp" {
			t.Fatal("lkembed_tcp_port=0 时不应出现 tcp want")
		}
	}

	// 只切走舞台（stage=none、语音仍 lkembed）：want 保留——语音线仍需要该端口。
	if err := a.st.SetSetting(ctx, "cfg_stage_provider", "none"); err != nil {
		t.Fatalf("落库 stage_provider 失败: %v", err)
	}
	if found, _ := findStage(); !found {
		t.Fatal("stage=none 但 voice=lkembed 时 PortWants 仍应包含 lkembed 媒体端口")
	}

	// 两线都切走（voice→外部 livekit 实例、stage→none）：want 随之消失
	//（下一轮 Mapper.Run 读取时即生效，无需重启）。
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk2", Type: TypeLivekit, Params: lkParams})
	a.reloadProviders(ctx)
	if err := a.st.SetSetting(ctx, "cfg_voice_provider", "lk2"); err != nil {
		t.Fatalf("落库 voice_provider 失败: %v", err)
	}
	if found, _ := findStage(); found {
		t.Fatal("voice/stage 都切走 lkembed 后 PortWants 不应再包含 hearth stage")
	}
}

// TestWatchDiagKey 观看诊断是站点级总开关：默认关、只收 off/on、生效值经 /api/site 下发。
func TestWatchDiagKey(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("WATCH_DIAG", "") // 部署侧设了 env 就锁成只读，测试从未设的前提出发
	a := testAPI(t)
	ctx := context.Background()
	token := adminToken(t, a)
	r := a.Router()

	siteWatchDiag := func() any {
		rec := doReq(t, r, "GET", "/api/site", "", nil)
		if rec.Code != 200 {
			t.Fatalf("/api/site 应 200，实际 %d", rec.Code)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		return got["watch_diag"]
	}

	if v := a.dynVal(ctx, "watch_diag"); v != "off" {
		t.Fatalf("默认应为 off，实际 %q", v)
	}
	if v := siteWatchDiag(); v != false {
		t.Fatalf("默认时 /api/site 的 watch_diag 应为 false，实际 %v", v)
	}

	// 枚举校验：非 off/on 一律拒收
	rec := doReq(t, r, "POST", "/api/admin/config", token, map[string]any{"values": map[string]string{"watch_diag": "yes"}})
	if rec.Code != 400 {
		t.Fatalf("非枚举值应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, r, "POST", "/api/admin/config", token, map[string]any{"values": map[string]string{"watch_diag": "on"}})
	if rec.Code != 204 {
		t.Fatalf("写入 on 应 204，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if v, err := a.st.GetSetting(ctx, "cfg_watch_diag"); err != nil || v != "on" {
		t.Fatalf("应落库到 cfg_watch_diag=on，实际 %q err=%v", v, err)
	}
	if v := siteWatchDiag(); v != true {
		t.Fatalf("开启后 /api/site 的 watch_diag 应为 true，实际 %v", v)
	}
}
