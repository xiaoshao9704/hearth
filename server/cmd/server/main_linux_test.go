//go:build linux

package main

import (
	"testing"
)

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
