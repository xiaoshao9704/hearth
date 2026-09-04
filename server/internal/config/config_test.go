package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesExistingWorkingDirectoryDatabase(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("hearth.db", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_PATH", "")
	t.Setenv("HEARTH_DATA", filepath.Join(dir, "data"))

	cfg := Load()
	if cfg.DBPath != "hearth.db" {
		t.Fatalf("应沿用工作目录现有数据库，实际 %q", cfg.DBPath)
	}
	st, err := os.Stat(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("数据目录权限应为 0700，实际 %v", st.Mode().Perm())
	}
}

func TestDataFlagAcceptsSingleDash(t *testing.T) {
	old := os.Args
	want := filepath.Join(t.TempDir(), "data")
	os.Args = []string{"hearth", "-data", want, "version"}
	t.Cleanup(func() { os.Args = old })
	if got := dataFlag(); got != want {
		t.Fatalf("-data 未被识别: %q", got)
	}
}
