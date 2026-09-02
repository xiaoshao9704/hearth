// Package selfcheck 二进制自检入口：部署镜像无 shell/curl，容器健康检查用程序自己的
// `healthcheck` 子命令完成（exec 形式）。健康状态只表示进程活着：端点内的探测
// 失败、映射为空都不构成非 200，否则 autoheal 类工具会把「探测服务暂时不可达」
// 当故障反复重启，比不刷新更糟。
package selfcheck

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// URL 由监听地址推导本机健康检查地址（容器内发起，天然回环）；refresh=1 顺带触发
// 宣告探测刷新（服务端只接受回环来源，见 lite.LoopbackRemote）。
func URL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz?refresh=1"
}

// Run GET 健康检查地址：请求失败或非 200 返回错误（调用方转成退出码 1）。
func Run(url string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("健康检查返回 %d", resp.StatusCode)
	}
	return nil
}
