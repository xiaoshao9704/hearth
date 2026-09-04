// 首启向导自动打开浏览器（--no-browser 关闭）。控制台程序形态下的唯一 GUI 动作；
// 服务化（阶段三）后不触发。容器里没有 open/xdg-open，失败静默忽略。
package main

import (
	"os/exec"
	"runtime"
)

// noBrowserFlag 手扫 --no-browser：与 --data 同理，flag 包解析之前就要知道。
func noBrowserFlag() bool { return hasFlag("--no-browser") }

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start() // 没图形环境的机器上失败无害，不打日志吓人
}
