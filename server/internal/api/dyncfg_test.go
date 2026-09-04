package api

import (
	"context"
	"testing"
)

// TestPortWantsLkembedStage lkembed 的媒体端口 want 在语音/舞台选择器切换时相应增删，
// 且必须 StrictPort（补丁二的地址改写只换 IP 不换端口）。
// 语音线或舞台线任一选中 lkembed 都需要该端口；两线都切走（voice→ember、stage→none）才消失。
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

	// TCP 端口默认 0（关闭），不应出现 tcp want。
	for _, w := range a.PortWants(ctx) {
		if w.Desc == "hearth stage" && w.Proto == "tcp" {
			t.Fatal("lkembed_tcp_port 默认关闭时不应出现 tcp want")
		}
	}

	// 打开 TCP 端口后应追加一条 StrictPort 的 tcp want。
	if err := a.st.SetSetting(ctx, "cfg_lkembed_tcp_port", "47721"); err != nil {
		t.Fatalf("落库 lkembed_tcp_port 失败: %v", err)
	}
	var tcpFound, tcpStrict bool
	for _, w := range a.PortWants(ctx) {
		if w.Desc == "hearth stage" && w.Proto == "tcp" {
			tcpFound, tcpStrict = true, w.StrictPort
		}
	}
	if !tcpFound || !tcpStrict {
		t.Fatalf("打开 lkembed_tcp_port 后应出现 StrictPort 的 tcp want: found=%v strict=%v", tcpFound, tcpStrict)
	}

	// 只切走舞台（stage=none、语音仍 lkembed）：want 保留——语音线仍需要该端口。
	if err := a.st.SetSetting(ctx, "cfg_stage_provider", "none"); err != nil {
		t.Fatalf("落库 stage_provider 失败: %v", err)
	}
	if found, _ := findStage(); !found {
		t.Fatal("stage=none 但 voice=lkembed 时 PortWants 仍应包含 lkembed 媒体端口")
	}

	// 两线都切走（voice→ember、stage→none）：want 随之消失
	//（下一轮 Mapper.Run 读取时即生效，无需重启）。
	if err := a.st.SetSetting(ctx, "cfg_voice_provider", TypeEmber); err != nil {
		t.Fatalf("落库 voice_provider 失败: %v", err)
	}
	if found, _ := findStage(); found {
		t.Fatal("voice/stage 都切走 lkembed 后 PortWants 不应再包含 hearth stage")
	}
}
