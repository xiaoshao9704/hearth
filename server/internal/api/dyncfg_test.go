package api

import (
	"context"
	"testing"
)

// TestPortWantsLkembedStage stage_provider 在 none/lkembed 之间切换时，PortWants 应
// 相应增删舞台媒体端口的 want，且该 want 必须 StrictPort（补丁二的地址改写只换 IP 不换端口）。
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

	// 默认 stage_provider=none：不应出现舞台媒体端口的 want。
	if found, _ := findStage(); found {
		t.Fatal("stage_provider=none 时 PortWants 不应包含 hearth stage")
	}

	// 切到 lkembed：应出现 udp want 且 StrictPort=true。
	if err := a.st.SetSetting(ctx, "cfg_stage_provider", AliasLkembed); err != nil {
		t.Fatalf("落库 stage_provider 失败: %v", err)
	}
	found, strict := findStage()
	if !found {
		t.Fatal("stage_provider=lkembed 时 PortWants 应包含 hearth stage 的 udp want")
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

	// 切回 none：舞台 want 应随之消失（下一轮 Mapper.Run 读取时即生效，无需重启）。
	if err := a.st.SetSetting(ctx, "cfg_stage_provider", "none"); err != nil {
		t.Fatalf("落库 stage_provider 失败: %v", err)
	}
	if found, _ := findStage(); found {
		t.Fatal("切回 stage_provider=none 后 PortWants 不应再包含 hearth stage")
	}
}
