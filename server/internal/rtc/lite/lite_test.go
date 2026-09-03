package lite

import (
	"maps"
	"testing"
	"time"
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

func TestStunExternals(t *testing.T) {
	locals := []string{"192.168.1.2", "10.0.0.2", "172.17.0.1", "203.0.113.9"}
	got := stunExternals(locals, nil, fakeProbe(map[string]string{
		"192.168.1.2": "203.0.113.5",
		"10.0.0.2":    "203.0.113.5", // 多网卡同出口：两条都留，宣告时按外部地址去重
		"203.0.113.9": "203.0.113.9", // 网卡直连公网：host 候选已经是对的，不入表
		// 172.17.0.1 未探到
	}))
	want := map[string]string{"192.168.1.2": "203.0.113.5", "10.0.0.2": "203.0.113.5"}
	if !maps.Equal(got, want) {
		t.Fatalf("探测结果不符: %v", got)
	}
}

func TestStunExternalsAllFailed(t *testing.T) {
	if got := stunExternals([]string{"192.168.1.2"}, nil, fakeProbe(nil)); got != nil {
		t.Fatalf("探测全失败应返回 nil（只宣告 host 候选），实际 %v", got)
	}
}

func TestStunExternalsNoLocals(t *testing.T) {
	probeCalled := false
	probe := func(_, _ []string, _ time.Duration) map[string]string {
		probeCalled = true
		return nil
	}
	if got := stunExternals(nil, nil, probe); got != nil {
		t.Fatalf("无网卡地址应返回 nil，实际 %v", got)
	}
	if probeCalled {
		t.Fatal("无网卡地址不应触发探测")
	}
}
