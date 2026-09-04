// service 子命令（hearth service install|uninstall|start|stop|status）的共享部分：
// 参数解析、服务模式判定、日志重定向。平台实现见 service_{darwin,linux,windows,other}.go。
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"hearth/server/internal/config"
	"hearth/server/internal/logrot"
)

// serviceModeActive 当前进程是否由服务管理器拉起（安装单元里带的 --service 参数；
// 与 --data/--no-browser 同理手扫，flag 包解析之前就要知道）。
func serviceModeActive() bool {
	for _, a := range os.Args[1:] {
		if a == "--service" {
			return true
		}
	}
	return false
}

// redirectServiceLog 服务模式日志落 <data>/hearth.log（10MB 轮转、保留 5 个备份）；
// 控制台模式不动，仍打 stdout/stderr。
func redirectServiceLog(dataDir string) {
	w, err := logrot.New(filepath.Join(dataDir, "hearth.log"), 10<<20, 5)
	if err != nil {
		log.Printf("日志重定向到 %s 失败（仍打控制台）: %v", filepath.Join(dataDir, "hearth.log"), err)
		return
	}
	log.SetOutput(w)
}

// runServiceCmd 分发 service 子命令；system = 带了 --system（仅 Linux 有意义）。返回退出码。
func runServiceCmd(args []string, system bool, cfg config.Config) int {
	action := ""
	for _, a := range args {
		if action == "" {
			action = a
			continue
		}
		fmt.Fprintln(os.Stderr, "无法识别的参数: "+a)
		return 2
	}
	if !serviceSupported {
		fmt.Fprintln(os.Stderr, "当前平台暂不支持服务化，请用 nohup/tmux 等方式常驻")
		return 1
	}
	var err error
	switch action {
	case "install":
		err = svcInstall(cfg, system)
	case "uninstall":
		err = svcUninstall(cfg, system)
	case "start":
		err = svcStart(system)
	case "stop":
		err = svcStop(system)
	case "status":
		err = svcStatus(system)
	default:
		fmt.Fprintln(os.Stderr, "用法: hearth service install [--system] | uninstall [--system] | start | stop | status")
		fmt.Fprintln(os.Stderr, "（--system 仅 Linux：写系统级 systemd 单元；默认是用户级）")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "service "+action+" 失败: "+err.Error())
		return 1
	}
	return 0
}

// exePath 当前二进制的绝对路径（服务单元的 ExecStart / ProgramArguments 用它）。
func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return filepath.Abs(exe)
}
