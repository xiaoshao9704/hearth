// Hearth 服务端入口：REST API + 聊天 WebSocket + LiveKit 令牌签发。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"hearth/server/internal/api"
	"hearth/server/internal/config"
	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc/lite"
	"hearth/server/internal/selfcheck"
	"hearth/server/internal/store"
	"hearth/server/internal/webui"

	"github.com/soheilhy/cmux"
	"golang.org/x/crypto/bcrypt"
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,32}$`)

// version 由 CI 发版时以 -ldflags "-X main.version=<tag>" 注入；本地构建保持 dev，
// 前端据此探测服务端升级（见 api.New 的 version 参数与 X-Hearth-Version 响应头）。
var version = "dev"

// positionals 返回子命令及其位置参数，只跳过程序自己识别的全局 flag。
// 子命令在 config.Load 前分派，不能依赖 flag 包事后解析。
func positionals() []string {
	var out []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i+1:]...)
			break
		}
		if a == "--data" || a == "-data" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--data=") || strings.HasPrefix(a, "-data=") || a == "--system" || a == "--service" {
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasFlag(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}

func main() {
	pos := positionals()
	cmd := ""
	if len(pos) > 0 {
		cmd = pos[0]
	}

	// Windows 服务由 SCM 接管生命周期；其余平台恒为 false。
	if runAsWindowsService() {
		return
	}

	cfg := config.Load()

	// CLI 子命令: healthcheck —— 容器健康检查（镜像无 shell/curl）：探活本机 /healthz。不开数据库。
	if cmd == "healthcheck" {
		if err := selfcheck.Run(selfcheck.URL(cfg.Addr)); err != nil {
			log.Printf("健康检查失败: %v", err)
			os.Exit(1)
		}
		return
	}
	if cmd == "service" {
		os.Exit(runServiceCmd(pos[1:], hasFlag("--system"), cfg))
	}

	st, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// CLI 子命令: adduser <用户名> <密码> —— 注册接口默认关闭时由管理员开通账号
	if cmd == "adduser" {
		if len(pos) != 3 {
			fmt.Fprintln(os.Stderr, "用法: hearth adduser <用户名> <密码>")
			os.Exit(2)
		}
		username, password := pos[1], pos[2]
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
	if cmd == "promote" {
		if len(pos) != 2 {
			fmt.Fprintln(os.Stderr, "用法: hearth promote <用户名>")
			os.Exit(2)
		}
		u, _, err := st.UserByName(context.Background(), pos[1])
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

	if serviceModeActive() {
		redirectServiceLog(cfg.DataDir)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runServer(ctx, cfg, st)
}

// httpListeners 两种监听模式的共同外壳：
//   - 合并模式（HTTPS_ADDR 空或与 ADDR 相同）：一个端口，cmux 按连接首字节是不是 TLS
//     握手记录分流，明文与 TLS 各挂一个子 listener，同一个 http.Server 服务两边
//     （srv.TLSConfig 保持 nil，Serve 才会自动挂上 HTTP/2 处理器，h2 由 ALPN 协商出来）。
//   - 分开模式：两个端口两个 http.Server，同一个 handler。
type httpListeners struct {
	srv    *http.Server // 明文；合并模式下同时服务 TLS 子 listener
	srvTLS *http.Server // 仅分开模式
	root   net.Listener // 仅合并模式：cmux 的根 listener
	errCh  chan error
	// Addr/TLSAddr 实际监听地址（端口填 0 时由内核分配，测试据此拨号）
	Addr, TLSAddr net.Addr
}

func startHTTP(cfg config.Config, h http.Handler, tlsCfg *tls.Config) (*httpListeners, error) {
	// 连上不发字节的连接不能无限占用：cmux 的 SetReadTimeout 只管嗅探那一读，
	// 而 cmux.Any() 不读字节就匹配，所以真正兜底的是 ReadHeaderTimeout。
	// IdleTimeout 单独给，否则 keep-alive 的空闲连接会被 ReadHeaderTimeout 按同一档掐掉。
	// 都不设 ReadTimeout：大文件上传与长响应不能有整体时限；WebSocket 在 hijack 时
	// 由 net/http 自己清掉 deadline，不受影响。
	newSrv := func() *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}
	}
	l := &httpListeners{srv: newSrv(), errCh: make(chan error, 2)}
	if cfg.TLSSplit() {
		ln, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return nil, err
		}
		lnTLS, err := net.Listen("tcp", cfg.HTTPSAddr)
		if err != nil {
			ln.Close()
			return nil, err
		}
		l.Addr, l.TLSAddr = ln.Addr(), lnTLS.Addr()
		l.srvTLS = newSrv()
		l.srvTLS.TLSConfig = tlsCfg
		go func() { l.errCh <- l.srv.Serve(ln) }()
		go func() { l.errCh <- l.srvTLS.ServeTLS(lnTLS, "", "") }()
		return l, nil
	}
	root, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	l.root, l.Addr, l.TLSAddr = root, root.Addr(), root.Addr()
	m := cmux.New(root)
	// 嗅探期间一个字节都不发的连接不能无限占着：超时后 cmux 关掉它
	m.SetReadTimeout(5 * time.Second)
	tlsL := tls.NewListener(m.Match(cmux.TLS()), tlsCfg)
	plainL := m.Match(cmux.Any())
	go l.srv.Serve(tlsL)
	go l.srv.Serve(plainL)
	go func() { l.errCh <- m.Serve() }()
	return l, nil
}

// wait 阻塞到监听真正出错；正常关闭返回 nil。
func (l *httpListeners) wait() error {
	err := <-l.errCh
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, cmux.ErrServerClosed) || errors.Is(err, cmux.ErrListenerClosed) {
		return nil
	}
	return err
}

// shutdown 优雅关闭：先让 http.Server 收尾在途请求（顺带关掉它们的 listener），
// 再关合并模式的根 listener，让 cmux 的 Serve 返回。
func (l *httpListeners) shutdown(ctx context.Context) {
	l.srv.Shutdown(ctx)
	if l.srvTLS != nil {
		l.srvTLS.Shutdown(ctx)
	}
	if l.root != nil {
		l.root.Close()
	}
}

func runServer(ctx context.Context, cfg config.Config, st *store.Store) {
	// 端口映射先建好：它是内核宣告外部地址的来源之一（lite.MappedFunc），要在内核构造前交出去。
	// Run 之前查不到任何映射，返回 false 即可，内核那边只是暂时少一条 srflx 候选。
	mapper := portmap.New()
	a := api.New(st, cfg, lite.MappedFunc(mapper.UDPExternal), version)

	// 语音线或舞台线选中 lkembed（进程内的 LiveKit 补丁 fork）时拉起它（默认两线同选，即默认常驻；
	// 两线都切走——语音选外部实例、舞台选 none——时什么都不起）
	a.EnsureStageKernel(context.Background())

	// chi 路由：API + /providers/* 接入分发；具体路由优先于静态通配，无 ServeMux 模式冲突问题
	r := a.Router()
	a.RegisterProxies(r)

	// 静态托管前端：优先二进制内嵌产物（单文件分发），未内嵌时回落 STATIC_DIR 外置目录
	if h := webui.Handler(); h != nil {
		r.Get("/*", h.ServeHTTP)
		r.Head("/*", h.ServeHTTP)
	} else if cfg.StaticDir != "" {
		fs := http.FileServer(http.Dir(cfg.StaticDir))
		r.Get("/*", fs.ServeHTTP)
		r.Head("/*", fs.ServeHTTP)
	}

	// 审计日志的保留策略清理：启动清一次，之后每小时一次
	go a.RunAuditRetention(ctx)

	// 映射建立/变化后立刻刷新宣告，让新会话拿到映射出的外部地址。
	// 回调不得阻塞 Mapper 的申请轮次（RefreshAnnounce 里是最长 2s 的 STUN 探测），另起协程。
	mapper.OnChange = func(portmap.Status) { go a.RefreshAnnounce(context.Background()) }
	go mapper.Run(ctx, a.PortWants)

	// 聊天保留策略：启动清一次，之后每小时一次（天数改了下一轮生效）
	go a.RunChatRetention(ctx)

	// 过期访客清理：启动清一次，之后每小时一次
	go a.RunGuestPurge(ctx)

	// 宣告探测周期刷新：公网 IP 变化后新会话拿到新候选，不重启、不动在途会话
	go func() {
		t := time.NewTicker(lite.DefaultAnnounceTTL)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.RefreshAnnounce(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	// TLS 证书：先同步跑一轮（首个握手就有证书），之后 60 秒一轮热换
	a.SetPortmapStatus(mapper.Snapshot)
	a.CheckTLS(ctx)
	go a.RunTLSCheck(ctx)

	lis, err := startHTTP(cfg, r, a.TLSServerConfig())
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	if cfg.TLSSplit() {
		log.Printf("hearth server http 监听于 %s，https 监听于 %s", cfg.Addr, cfg.HTTPSAddr)
	} else {
		log.Printf("hearth server 监听于 %s（http 与 https 同端口）", cfg.Addr)
	}
	go func() {
		if err := lis.wait(); err != nil {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lis.shutdown(shutdownCtx)
	// Run 的 ctx 已经结束，撤销映射必须用新的 ctx，否则请求发不出去、映射留在网关上
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	mapper.Close(closeCtx)
	a.StopStageKernel()
}
