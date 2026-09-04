//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

func TestPositionalsSkipsSingleDashDataValue(t *testing.T) {
	old := os.Args
	os.Args = []string{"hearth", "-data", "<占位>", "adduser", "user", "change-me"}
	t.Cleanup(func() { os.Args = old })
	got := positionals()
	if strings.Join(got, ",") != "adduser,user,change-me" {
		t.Fatalf("位置参数解析错误: %v", got)
	}
}

func TestSystemdQuote(t *testing.T) {
	got := systemdQuote(`path with space/100%/"quoted"`)
	if got != `"path with space/100%%/\"quoted\""` {
		t.Fatalf("systemd 参数转义错误: %s", got)
	}
}

func TestSystemdWorkingDirectoryEscapesSpecifier(t *testing.T) {
	got := systemdWorkingDirectory(`/srv/hearth data/100%h`)
	if got != `/srv/hearth data/100%%h` {
		t.Fatalf("WorkingDirectory 的 systemd specifier 转义错误: %s", got)
	}
}
