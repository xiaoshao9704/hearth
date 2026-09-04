//go:build darwin

// macOS 服务化：用户级 LaunchAgent（~/Library/LaunchAgents/com.hearth.server.plist），
// 不需要 sudo。launchctl bootstrap/bootout/print 管理；RunAtLoad + KeepAlive 常驻。
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"hearth/server/internal/config"
)

const serviceSupported = true

const (
	launchdLabel = "com.hearth.server"
)

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// launchdTarget launchctl 的服务目标：gui/<uid>/<label>。
func launchdTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchdLabel
}

func svcInstall(cfg config.Config, system bool) error {
	if system {
		return errors.New("macOS 只支持用户级服务（LaunchAgent），去掉 --system 即可")
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	plist, err := plistPath()
	if err != nil {
		return err
	}
	// launchd 自身的 stdout/stderr 抓到单独文件（hearth.log 由进程内 logrot 轮转，
	// 两个 writer 追加同一文件会在轮转rename后错位，所以分开）
	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEsc(exe) + `</string>
		<string>--data</string>
		<string>` + xmlEsc(cfg.DataDir) + `</string>
		<string>--service</string>
	</array>
	<key>WorkingDirectory</key>
	<string>` + xmlEsc(cfg.DataDir) + `</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + xmlEsc(filepath.Join(cfg.DataDir, "hearth-launchd.log")) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEsc(filepath.Join(cfg.DataDir, "hearth-launchd.log")) + `</string>
</dict>
</plist>
`
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}
	// 已装载过先卸再装，让新 plist 生效（bootout 未装载时失败无害）
	_ = exec.Command("launchctl", "bootout", launchdTarget()).Run()
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("已安装并启动（用户级 LaunchAgent）：")
	fmt.Println("  配置: " + plist)
	fmt.Println("  日志: " + filepath.Join(cfg.DataDir, "hearth.log"))
	fmt.Println("  状态: hearth service status")
	return nil
}

func svcUninstall(cfg config.Config, system bool) error {
	if system {
		return errors.New("macOS 只支持用户级服务，去掉 --system 即可")
	}
	_ = exec.Command("launchctl", "bootout", launchdTarget()).Run() // 未装载时失败无害
	plist, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("已卸载（数据目录 " + cfg.DataDir + " 保留）")
	return nil
}

func svcStart(system bool) error {
	if system {
		return errors.New("macOS 只支持用户级服务，去掉 --system 即可")
	}
	plist, err := plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); err != nil {
		return errors.New("服务未安装，先 hearth service install")
	}
	// 已装载则 kickstart 重启；未装载（stop 过）先 bootstrap 再拉起——bootout 后立刻
	// bootstrap 可能赶上旧实例还没退完而失败，所以按 print 结果分路而不是无脑都试
	if err := exec.Command("launchctl", "print", launchdTarget()).Run(); err != nil {
		if out, err := exec.Command("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), plist).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		fmt.Println("已启动")
		return nil
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", launchdTarget()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("已启动")
	return nil
}

func svcStop(system bool) error {
	if system {
		return errors.New("macOS 只支持用户级服务，去掉 --system 即可")
	}
	if out, err := exec.Command("launchctl", "bootout", launchdTarget()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootout: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("已停止（plist 保留，hearth service start 可再拉起）")
	return nil
}

func svcStatus(system bool) error {
	if system {
		return errors.New("macOS 只支持用户级服务，去掉 --system 即可")
	}
	plist, err := plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		fmt.Println("未安装（hearth service install 安装用户级 LaunchAgent）")
		return nil
	}
	fmt.Println("已安装: " + plist)
	out, err := exec.Command("launchctl", "print", launchdTarget()).CombinedOutput()
	if err != nil {
		fmt.Println("状态: 未装载（未在运行；hearth service start 启动）")
		return nil
	}
	// print 输出里抠 state 与 pid 两行即可
	state, pid := "未知", ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state = "); ok {
			state = v
		}
		if v, ok := strings.CutPrefix(line, "pid = "); ok {
			pid = v
		}
	}
	if pid != "" {
		fmt.Printf("状态: %s（pid %s）\n", state, pid)
	} else {
		fmt.Println("状态: " + state)
	}
	return nil
}

func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
