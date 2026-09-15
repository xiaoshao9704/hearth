package main

import (
	"encoding/json"
	"testing"

	"hearth/server/internal/config"
)

// service status --json 的字段名是与桌面壳之间的契约（壳据此判断该装、该启还是已在跑），
// 改名等于悄悄断掉「在本机运行服务器」。
func TestServiceStateJSONKeys(t *testing.T) {
	b, err := json.Marshal(serviceState{Installed: true, Running: true, Detail: "running", PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"installed":true,"running":true,"detail":"running","pid":42}`; got != want {
		t.Fatalf("序列化 = %s, want %s", got, want)
	}
	// 零值要能明确表达「没装、没跑」，detail/pid 省略
	b, err = json.Marshal(serviceState{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"installed":false,"running":false}`; got != want {
		t.Fatalf("零值序列化 = %s, want %s", got, want)
	}
}

func TestServiceCmdRejectsJSONOnNonStatus(t *testing.T) {
	if code := runServiceCmd([]string{"install", "--json"}, false, config.Config{}); code != 2 {
		t.Fatalf("install --json 退出码 = %d, want 2", code)
	}
}
