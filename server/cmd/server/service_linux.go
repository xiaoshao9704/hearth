//go:build linux

// Linux 服务化：默认 systemd --user 单元（~/.config/systemd/user/hearth.service）；
// --system 写 /etc/systemd/system/（需要权限，install 时报错提示 sudo 或用户级）。
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"hearth/server/internal/config"
)

const serviceSupported = true

const unitName = "hearth.service"

func unitPath(system bool) (string, error) {
	if system {
		return filepath.Join("/etc/systemd/system", unitName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

// systemctl 按形态拼参数（用户级加 --user）。
func systemctl(system bool, args ...string) *exec.Cmd {
	if !system {
		args = append([]string{"--user"}, args...)
	}
	return exec.Command("systemctl", args...)
}

func svcInstall(cfg config.Config, system bool) error {
	exe, err := exePath()
	if err != nil {
		return err
	}
	path, err := unitPath(system)
	if err != nil {
		return err
	}
	wantedBy := "default.target"
	after := "After=network-online.target"
	if system {
		wantedBy = "multi-user.target"
	}
	unit := `[Unit]
Description=Hearth Server
` + after + `

[Service]
ExecStart=` + systemdQuote(exe) + ` --data ` + systemdQuote(cfg.DataDir) + ` --service
WorkingDirectory=` + systemdWorkingDirectory(cfg.DataDir) + `
Restart=on-failure
RestartSec=2

[Install]
WantedBy=` + wantedBy + `
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return errors.New("没有写 " + path + " 的权限：用 sudo 运行 install --system，或去掉 --system 装用户级服务")
		}
		return err
	}
	if out, err := systemctl(system, "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := systemctl(system, "enable", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("enable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("已安装并设为开机自启（" + map[bool]string{true: "系统级", false: "用户级"}[system] + " systemd 单元）：")
	fmt.Println("  配置: " + path)
	fmt.Println("  启动: hearth service start" + map[bool]string{true: " --system", false: ""}[system])
	fmt.Println("  日志: " + filepath.Join(cfg.DataDir, "hearth.log"))
	if !system {
		fmt.Println("  提示: 若需未登录也自动启动，请执行 loginctl enable-linger $USER")
	}
	return nil
}

func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// WorkingDirectory 的值允许空格，但其中的 % 会被 systemd 当作 specifier 展开。
func systemdWorkingDirectory(path string) string {
	return strings.ReplaceAll(path, "%", "%%")
}

func svcUninstall(cfg config.Config, system bool) error {
	_ = systemctl(system, "stop", unitName).Run()
	_ = systemctl(system, "disable", unitName).Run()
	path, err := unitPath(system)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = systemctl(system, "daemon-reload").Run()
	fmt.Println("已卸载（数据目录 " + cfg.DataDir + " 保留）")
	return nil
}

func svcStart(system bool) error {
	if out, err := systemctl(system, "start", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("start: %v（服务未安装则先 install）(%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("已启动")
	return nil
}

func svcStop(system bool) error {
	if out, err := systemctl(system, "stop", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("stop: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("已停止")
	return nil
}

func svcState(system bool) (serviceState, error) {
	path, err := unitPath(system)
	if err != nil {
		return serviceState{}, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return serviceState{}, nil
	}
	// is-active 在非 active 时退出码非 0，输出仍是状态词（inactive/failed/…），只看输出
	active, _ := systemctl(system, "is-active", unitName).Output()
	detail := strings.TrimSpace(string(active))
	return serviceState{Installed: true, Running: detail == "active", Detail: detail}, nil
}

func svcStatus(system bool) error {
	st, err := svcState(system)
	if err != nil {
		return err
	}
	if !st.Installed {
		fmt.Println("未安装（hearth service install 安装" + map[bool]string{true: "系统级", false: "用户级"}[system] + " systemd 单元）")
		return nil
	}
	path, err := unitPath(system)
	if err != nil {
		return err
	}
	fmt.Println("已安装: " + path)
	enabled, _ := systemctl(system, "is-enabled", unitName).Output()
	fmt.Printf("状态: %s（开机自启: %s）\n", st.Detail, strings.TrimSpace(string(enabled)))
	return nil
}
