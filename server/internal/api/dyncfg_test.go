package api

import (
	"context"
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
