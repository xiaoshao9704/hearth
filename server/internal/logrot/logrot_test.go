package logrot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hearth.log")
	// 阈值 100 字节、保留 3 个备份，写 5 轮各 60 字节
	w, err := New(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for round := 0; round < 5; round++ {
		if _, err := w.Write([]byte(strings.Repeat("a", 60) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	// 当前文件 + .1/.2/.3 存在，.4 不存在（最多保留 3 个备份）
	for _, suffix := range []string{"", ".1", ".2", ".3"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("%s 应存在: %v", path+suffix, err)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatal("备份数超上限应丢弃最老的")
	}
	// 每个文件都不超阈值太多（单条日志 61B 允许整条约过去：轮转在写之前判定）
	for _, suffix := range []string{"", ".1", ".2", ".3"} {
		st, _ := os.Stat(path + suffix)
		if st.Size() > 100+61 {
			t.Fatalf("%s 超限: %d", path+suffix, st.Size())
		}
	}
	// 最老的备份内容应是第 2 轮写的（第 1 轮已被挤出）
	b, _ := os.ReadFile(path + ".3")
	if len(b) != 61 {
		t.Fatalf(".3 应是一整轮日志: %d", len(b))
	}
}

func TestAppendExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hearth.log")
	// 已有超限文件时 reopen 先轮转
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b, _ := os.ReadFile(path + ".1")
	if len(b) != 200 {
		t.Fatal("重启后超限的旧日志应先轮转成 .1")
	}
	if _, err := w.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	b, _ = os.ReadFile(path)
	if string(b) != "new\n" {
		t.Fatalf("新日志应写进新文件: %q", b)
	}
}
