package lite

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func fakeProbe(mapping map[string]string) func([]string, []string, time.Duration) map[string]string {
	return func(locals, _ []string, _ time.Duration) map[string]string {
		out := make(map[string]string, len(locals))
		for _, l := range locals {
			if ext, ok := mapping[l]; ok {
				out[l] = ext
			}
		}
		return out
	}
}

func TestAnnounceRulesExplicitOverride(t *testing.T) {
	probeCalled := false
	probe := func(locals, servers []string, _ time.Duration) map[string]string {
		probeCalled = true
		return nil
	}
	rules := announceRules("203.0.113.5", []string{"10.0.0.2"}, nil, probe)
	if probeCalled {
		t.Fatal("显式配置不应触发 STUN 探测")
	}
	if len(rules) != 1 {
		t.Fatalf("应只有 1 条规则，实际 %d", len(rules))
	}
	r := rules[0]
	if len(r.External) != 1 || r.External[0] != "203.0.113.5" || r.Local != "" {
		t.Fatalf("应为 catch-all 规则: %+v", r)
	}
	if r.Mode != webrtc.ICEAddressRewriteModeUnspecified {
		t.Fatalf("覆盖语义应为替换模式，实际 %v", r.Mode)
	}
}

func TestAnnounceRulesAppend(t *testing.T) {
	locals := []string{"192.168.1.2", "10.0.0.2", "172.17.0.1"}
	mapping := map[string]string{
		"192.168.1.2": "203.0.113.5",
		"10.0.0.2":    "203.0.113.5", // 多网卡同出口：与第一条重复，应去重
		// 172.17.0.1 未探到：不出规则
	}
	rules := announceRules("", locals, nil, fakeProbe(mapping))
	if len(rules) != 1 {
		t.Fatalf("重复外部地址应去重为 1 条，实际 %d: %+v", len(rules), rules)
	}
	r := rules[0]
	if r.Local != "192.168.1.2" || r.External[0] != "203.0.113.5" {
		t.Fatalf("规则内容不符: %+v", r)
	}
	if r.Mode != webrtc.ICEAddressRewriteAppend {
		t.Fatalf("公网映射应为 append 模式，实际 %v", r.Mode)
	}
	if r.AsCandidateType != webrtc.ICECandidateTypeHost {
		t.Fatalf("应作为 host candidate，实际 %v", r.AsCandidateType)
	}
}

func TestAnnounceRulesDirectPublicSkipped(t *testing.T) {
	locals := []string{"203.0.113.5"}
	mapping := map[string]string{"203.0.113.5": "203.0.113.5"} // 网卡直连公网
	rules := announceRules("", locals, nil, fakeProbe(mapping))
	if len(rules) != 0 {
		t.Fatalf("直连公网的网卡不需要改写规则，实际 %+v", rules)
	}
}

func TestAnnounceRulesAllProbeFailed(t *testing.T) {
	rules := announceRules("", []string{"192.168.1.2"}, nil, fakeProbe(nil))
	if rules != nil {
		t.Fatalf("探测全失败应返回 nil（原样宣告网卡地址），实际 %+v", rules)
	}
}

func TestAnnounceRulesNoLocals(t *testing.T) {
	probeCalled := false
	probe := func(_, _ []string, _ time.Duration) map[string]string {
		probeCalled = true
		return nil
	}
	if rules := announceRules("", nil, nil, probe); rules != nil {
		t.Fatalf("无网卡地址应返回 nil，实际 %+v", rules)
	}
	if probeCalled {
		t.Fatal("无网卡地址不应触发探测")
	}
}
