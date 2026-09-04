//go:build windows

// Windows 服务化：x/sys/windows/svc + svc/mgr 注册服务（自动启动）；install 时同时用
// netsh 放行 HTTP/HTTPS 监听端口与 ember 媒体 UDP 端口（卸载时删规则）——非服务模式
// 首次监听时系统弹一次防火墙询问即可，服务账号弹不出对话框，所以规则在安装时写好。
// 进程被 SCM 拉起时（IsWindowsService）走 svc.Run 包住主循环并响应停止事件。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"hearth/server/internal/config"
	"hearth/server/internal/store"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceSupported = true

const serviceName = "hearth"

// runAsWindowsService 被 SCM 拉起时接管 main：svc.Run 包主循环，响应 SCM 停止事件。
// 返回 true 表示本进程是服务形态、main 不再往下走。
func runAsWindowsService() bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false
	}
	cfg := config.Load()
	redirectServiceLog(cfg.DataDir)
	if err := svc.Run(serviceName, &hearthService{cfg: cfg}); err != nil {
		log.Fatalf("Windows 服务运行失败: %v", err)
	}
	return true
}

type hearthService struct {
	cfg config.Config
}

func (s *hearthService) Execute(_ []string, reqs <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	st, err := store.Open(s.cfg.DatabaseDSN())
	if err != nil {
		log.Printf("打开数据库失败: %v", err)
		return true, 1
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runServer(ctx, s.cfg, st)
		close(done)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-reqs:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		case <-done:
			cancel()
			return false, 0
		}
	}
}

func svcInstall(cfg config.Config, system bool) error {
	exe, err := exePath()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return errors.New("连不上服务管理器（需要管理员权限运行 install）: " + err.Error())
	}
	defer m.Disconnect()
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "Hearth Server",
		Description: "Hearth 语音/投屏服务器",
		StartType:   mgr.StartAutomatic,
	}, "--data", cfg.DataDir, "--service")
	if err != nil {
		return err
	}
	s.Close()
	if err := addFirewallRules(cfg); err != nil {
		log.Printf("防火墙规则写入失败（首次监听时系统会弹询问，手动允许即可）: %v", err)
	}
	fmt.Println("已安装并设为自动启动（Windows 服务）：")
	fmt.Println("  启动: hearth service start")
	fmt.Println("  日志: " + cfg.DataDir + `\hearth.log`)
	return nil
}

func svcUninstall(cfg config.Config, system bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return errors.New("连不上服务管理器（需要管理员权限运行 uninstall）: " + err.Error())
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err == nil {
		_, _ = s.Control(svc.Stop) // 没在跑时失败无害
		time.Sleep(500 * time.Millisecond)
		if err := s.Delete(); err != nil {
			s.Close()
			return err
		}
		s.Close()
	}
	removeFirewallRules()
	fmt.Println("已卸载（数据目录 " + cfg.DataDir + " 保留）")
	return nil
}

func svcStart(system bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("服务未安装，先 hearth service install")
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return err
	}
	fmt.Println("已启动")
	return nil
}

func svcStop(system bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("服务未安装")
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil {
		return err
	}
	fmt.Println("已停止")
	return nil
}

func svcStatus(system bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Println("未安装（以管理员身份运行 hearth service install）")
		return nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	state := map[svc.State]string{
		svc.Running: "运行中", svc.Stopped: "已停止", svc.StartPending: "启动中", svc.StopPending: "停止中",
	}[st.State]
	if state == "" {
		state = fmt.Sprintf("状态码 %d", uint32(st.State))
	}
	fmt.Println("已安装，状态: " + state)
	return nil
}

// 防火墙规则名（卸载时按名删）。
var fwRules = []struct{ name, proto string }{
	{"Hearth HTTP", "TCP"},
	{"Hearth HTTPS", "TCP"},
	{"Hearth Voice UDP", "UDP"},
}

// firewallPorts 要放行的监听端口：HTTP（ADDR）、HTTPS（https_addr）、ember 媒体 UDP。
// env 优先，其次 DB settings，最后默认——与运行时生效口径一致（dyncfg 的简化版，
// 服务命令形态不构造完整 API 对象）。
func firewallPorts(cfg config.Config) (httpP, httpsP, udpP int) {
	_, ps, err := net.SplitHostPort(cfg.Addr)
	if err == nil {
		httpP, _ = strconv.Atoi(ps)
	}
	st, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		log.Printf("读取配置失败，防火墙只按 env/默认端口放行: %v", err)
	}
	defer func() {
		if st != nil {
			st.Close()
		}
	}()
	pick := func(env, key, def string) int {
		if v := os.Getenv(env); v != "" {
			if _, p, err := net.SplitHostPort(v); err == nil {
				if n, err := strconv.Atoi(p); err == nil {
					return n
				}
			}
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		if st != nil {
			if v, err := st.GetSetting(context.Background(), "cfg_"+key); err == nil && v != "" {
				if _, p, err := net.SplitHostPort(v); err == nil {
					if n, err := strconv.Atoi(p); err == nil {
						return n
					}
				}
				if n, err := strconv.Atoi(v); err == nil {
					return n
				}
			}
		}
		n, _ := strconv.Atoi(def)
		return n
	}
	httpsP = pick("HTTPS_ADDR", "https_addr", ":8443")
	udpP = pick("EMBER_UDP_PORT", "ember_udp_port", "47700")
	return httpP, httpsP, udpP
}

func addFirewallRules(cfg config.Config) error {
	httpP, httpsP, udpP := firewallPorts(cfg)
	ports := []int{httpP, httpsP, udpP}
	for i, rule := range fwRules {
		if ports[i] <= 0 {
			continue
		}
		out, err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
			"name="+rule.name, "dir=in", "action=allow", "protocol="+rule.proto,
			"localport="+strconv.Itoa(ports[i])).CombinedOutput()
		if err != nil {
			return fmt.Errorf("netsh %s: %v (%s)", rule.name, err, string(out))
		}
	}
	return nil
}

func removeFirewallRules() {
	for _, rule := range fwRules {
		_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+rule.name).Run()
	}
}
