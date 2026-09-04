package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesExplicitDataDirectoryBeforeWorkingDirectoryDatabase(t *testing.T) {
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
	want := filepath.Join(dir, "data", "hearth.db")
	if cfg.DBPath != want {
		t.Fatalf("显式数据目录应优先，期望 %q，实际 %q", want, cfg.DBPath)
	}
	st, err := os.Stat(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("数据目录权限应为 0700，实际 %v", st.Mode().Perm())
	}
}

func TestLoadUsesExistingWorkingDirectoryDatabaseWithoutExplicitDataDir(t *testing.T) {
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
	t.Setenv("HEARTH_DATA", "")
	old := os.Args
	os.Args = []string{"hearth"}
	t.Cleanup(func() { os.Args = old })

	if cfg := Load(); cfg.DBPath != "hearth.db" {
		t.Fatalf("未显式指定数据目录时应沿用工作目录数据库，实际 %q", cfg.DBPath)
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
