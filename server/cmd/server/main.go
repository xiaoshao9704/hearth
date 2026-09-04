// Hearth 服务端入口：REST API + 聊天 WebSocket + LiveKit 令牌签发。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"hearth/server/internal/api"
	"hearth/server/internal/chat"
	"hearth/server/internal/config"
	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc/lite"
	"hearth/server/internal/selfcheck"
	"hearth/server/internal/store"
	"hearth/server/internal/tlsx"
	"hearth/server/internal/webui"

	"golang.org/x/crypto/bcrypt"
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,32}$`)

func main() {
	cfg := config.Load()

	// CLI 子命令: healthcheck —— 容器健康检查（镜像无 shell/curl）：探活本机 /healthz。不开数据库。
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := selfcheck.Run(selfcheck.URL(cfg.Addr)); err != nil {
			log.Printf("健康检查失败: %v", err)
			os.Exit(1)
		}
		return
	}

	st, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// CLI 子命令: adduser <用户名> <密码> —— 注册接口默认关闭时由管理员开通账号
	if len(os.Args) > 1 && os.Args[1] == "adduser" {
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "用法: hearth adduser <用户名> <密码>")
			os.Exit(2)
		}
		username, password := os.Args[2], os.Args[3]
		if !usernameRe.MatchString(username) || len(password) < 6 {
			log.Fatal("用户名需 2-32 位字母数字，密码至少 6 位")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Fatalf("密码哈希失败: %v", err)
		}
		u, err := st.CreateUser(context.Background(), username, string(hash))
		if err != nil {
			log.Fatalf("创建用户失败: %v", err)
		}
		fmt.Printf("用户 %s (id=%d, role=%s) 创建成功\n", u.Username, u.ID, u.Role)
		return
	}

	// CLI 子命令: promote <用户名> —— 转移超级管理员（旧 super 降为 admin，全站恰好一个 super）
	if len(os.Args) > 1 && os.Args[1] == "promote" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "用法: hearth promote <用户名>")
			os.Exit(2)
		}
		u, _, err := st.UserByName(context.Background(), os.Args[2])
		if err != nil {
			log.Fatalf("用户不存在: %v", err)
		}
		if u.Role == store.RoleGuest {
			log.Fatal("访客不能成为超级管理员")
		}
		if err := st.TransferSuper(context.Background(), u.ID); err != nil {
			log.Fatalf("转移失败: %v", err)
		}
		fmt.Printf("超级管理员已转移给 %s (id=%d)，原超级管理员降为 admin\n", u.Username, u.ID)
		return
	}

	hub := chat.NewHub(st, cfg.CORSOrigin)
	// 端口映射先建好：它是内核宣告外部地址的来源之一（lite.MappedFunc），要在内核构造前交出去。
	// Run 之前查不到任何映射，返回 false 即可，内核那边只是暂时少一条 srflx 候选。
	mapper := portmap.New()
	a := api.New(st, cfg, hub, lite.MappedFunc(mapper.UDPExternal))
	a.SetPortMapper(mapper)

	// 舞台线选中 lkembed 时拉起进程内 LiveKit（选 none 或外部实例时什么都不起）
	a.EnsureStageKernel(context.Background())

	// chi 路由：API + 聊天 WS + /providers/* 接入分发；具体路由优先于静态通配，无 ServeMux 模式冲突问题
	r := a.Router()
	a.RegisterProxies(r)
	r.Get("/api/chat", hub.ServeHTTP)

	// 静态托管前端：优先二进制内嵌产物（单文件分发），未内嵌时回落 STATIC_DIR 外置目录
	if h := webui.Handler(); h != nil {
		r.Get("/*", h.ServeHTTP)
		r.Head("/*", h.ServeHTTP)
	} else if cfg.StaticDir != "" {
		fs := http.FileServer(http.Dir(cfg.StaticDir))
		r.Get("/*", fs.ServeHTTP)
		r.Head("/*", fs.ServeHTTP)
	}

	// 优雅退出：Windows 没有 SIGTERM（常量仍在），os.Interrupt 是那边的兜底
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 过期访客周期清理（10 分钟一轮，见 api.GuestSweepLoop）
	go a.GuestSweepLoop(ctx)

	// 进程内 TLS：模式/证书由动态配置决定，Sync 幂等热切换（见 internal/tlsx）。
	// HTTP listener 不随模式重启——它的 handler 是按当前模式分流的壳
	// （TLS 开启时只做 ACME 挑战 + 301 到 HTTPS）。
	tlsm := tlsx.New(filepath.Join(cfg.DataDir, "certs"),
		func() http.Handler { return r },
		func() []string { ex, _ := a.AnnounceExternals(); return ex })
	a.SetTLS(tlsm)
	a.SyncTLS()

	// 映射建立/变化后立刻刷新宣告，让新会话拿到映射出的外部地址。
	// 回调不得阻塞 Mapper 的申请轮次（RefreshAnnounce 里是最长 2s 的 STUN 探测），另起协程。
	mapper.OnChange = func(portmap.Status) {
		go func() {
			a.RefreshAnnounce(context.Background())
			a.SyncTLS() // 公网地址集合变了，自签名叶子证书要重签
		}()
	}
	go mapper.Run(ctx, a.PortWants)

	// 宣告探测周期刷新：公网 IP 变化后新会话拿到新候选，不重启、不动在途会话
	go func() {
		t := time.NewTicker(lite.DefaultAnnounceTTL)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.RefreshAnnounce(ctx)
				a.SyncTLS()
			case <-ctx.Done():
				return
			}
		}
	}()

	// 首启（还没建任何账号）自动开浏览器进向导；--no-browser 关闭
	if !noBrowserFlag() {
		if n, _, err := st.Counts(context.Background()); err == nil && n == 0 {
			_, port, err := net.SplitHostPort(cfg.Addr)
			if err == nil {
				go func() {
					time.Sleep(300 * time.Millisecond) // 等 listener 起来
					openBrowser("http://localhost:" + port + "/#/setup")
				}()
			}
		}
	}

	srv := &http.Server{Addr: cfg.Addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if tlsm.TLSOn() {
			tlsm.HTTPHandler(nil).ServeHTTP(w, req)
			return
		}
		r.ServeHTTP(w, req)
	})}
	go func() {
		log.Printf("hearth server 监听于 %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	tlsm.Close()
	// Run 的 ctx 已经结束，撤销映射必须用新的 ctx，否则请求发不出去、映射留在网关上
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	mapper.Close(closeCtx)
	a.StopStageKernel()
}
