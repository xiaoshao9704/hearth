// Package livekitembed 在当前进程内拉起/停止一个 LiveKit 服务端（补丁式 fork，见
// docs/plan-livekit-embed.md）。本包只管生命周期与配置拼装，不实现任何 rtc 接口——
// 舞台槽位、令牌签发、信令反代、推流出口一律由 livekitrtc 指向它的监听地址。
//
// 两种宿主：hearth 自己（内建实例 lkembed，只听回环，见 api/lkembed.go），
// 以及远端舞台机器上的 cmd/stage（听私网地址，hearth 经 LIVEKIT_API_URL 访问）。
package livekitembed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/logger"

	"hearth/server/internal/rtc"
)

// ConfigKeys 本内核的全局配置键（命名空间 lkembed_*）。端口不给环境变量：它们是进程内
// 基建，改动重启生效；密钥留空时由宿主首次启动生成并落库（见 api 的 ensureEmbedKeys）。
func ConfigKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "lkembed_port", Group: "stage", Default: "47730",
			Label: "信令端口", Hint: "只监听回环，浏览器经 /providers/lkembed/rtc 同源反代访问；改动重启生效"},
		{Name: "lkembed_udp_port", Group: "stage", Default: "47720",
			Label: "媒体 UDP 端口", Hint: "单端口 mux，需在防火墙/安全组放行；改动重启生效"},
		{Name: "lkembed_tcp_port", Group: "stage", Default: "0",
			Label: "ICE-TCP 端口",
			Hint: "0 = 关闭。实测家庭网络做策略路由/分流时 UDP 常被中间设备接管、回程不通，" +
				"建议与媒体 UDP 端口同号开启，云侧安全组该端口 udp/tcp 双放行；改动重启生效"},
		{Name: "lkembed_log_level", Group: "stage", Default: "warn",
			Options: []string{"debug", "info", "warn", "error"},
			Label:   "LiveKit 日志级别", Hint: "正常运行保持 warn；排障时临时改为 debug；改动重启生效"},
		{Name: "lkembed_api_key", Group: "stage",
			Label: "API Key", Hint: "留空 = 首次启动自动生成并落库（随数据库一起备份）"},
		{Name: "lkembed_api_secret", Group: "stage", Secret: true,
			Label: "API Secret", Hint: "留空 = 首次启动自动生成并落库"},
		{Name: "lkembed_public_ip", Env: "LKEMBED_PUBLIC_IP", Group: "stage",
			Label: "公网 IP", Hint: "留空 = 自动宣告全部网卡地址与 STUN 探测到的公网映射；显式设置则只通告该地址（覆盖）"},
		{Name: "lkembed_extra_ips", Env: "LKEMBED_EXTRA_IPS", Group: "stage",
			Label: "额外候选 IP", Hint: "逗号分隔；容器部署时可填宿主的 IPv6、局域网或覆盖网络地址，与自动探测结果并列宣告"},
		{Name: "lkembed_stun_servers", Env: "LKEMBED_STUN_SERVERS", Group: "stage",
			Label: "STUN 服务器", Hint: "逗号分隔；探测各网卡公网映射用，留空用内置默认（不可达时改填可用地址）"},
	}
}

// bindOr / logLevelOr 空值回落默认（见 Options 字段注释）。
func bindOr(v string) string {
	if v == "" {
		return "127.0.0.1"
	}
	return v
}

func logLevelOr(v string) string {
	if v == "" {
		return "warn"
	}
	return v
}

// setLoggerOnce 见 Start 里的用法：LiveKit 的全局 logger 只装一次。
var setLoggerOnce sync.Once

// startTimeout 是「监听起来并能应答 HTTP」的等待上限；超时按启动失败返回。
const startTimeout = 20 * time.Second

// stopTimeout 是 Stop 后等端口释放的上限；到点就不再等，避免卡住宿主退出。
const stopTimeout = 10 * time.Second

type Options struct {
	HTTPPort int // HTTP/信令端口
	// Bind HTTP/信令的监听地址，空 = 127.0.0.1。进程内形态（lkembed）只听回环、
	// 浏览器经 hearth 同源反代；远端形态（cmd/stage）要让 hearth 经私网访问 API，
	// 由部署侧给 0.0.0.0 或具体网卡地址。
	Bind      string
	UDPPort   int // 媒体 UDP 单端口
	TCPPort   int // ICE-TCP 端口，0 = 关
	APIKey    string
	APISecret string
	// LogLevel LiveKit 自己的日志级别，空 = warn（错误看得见，正常运行不刷屏）。
	LogLevel string
	// ExternalIPs 是补丁二的回调：每建一个 PeerConnection 取一次当前外部地址，
	// 追加为候选（本机 host 候选保留）。nil = 只宣告本机地址。
	ExternalIPs func() []string
	// LogSink 收本包自己的生命周期日志。LiveKit 内部日志进不来：protocol/logger 的
	// zap console 输出写死 os.Stderr，只能额外挂 tap 不能改向，所以内部日志仍走 stderr，
	// 级别压到 warn（错误还看得见，正常运行不刷屏）。
	LogSink func(string, ...any)
}

type Server struct {
	lk       *service.LivekitServer
	opts     Options
	done     chan struct{} // Start() 返回后关闭
	runErr   error         // Start() 的返回值，读之前必须 <-done
	stopOnce sync.Once
}

// Start 拉起进程内 LiveKit。失败原样返回错误（端口占用等），由调用方决定是否致命——
// 舞台线起不来不该拖垮语音线。
func Start(ctx context.Context, o Options) (*Server, error) {
	if o.APIKey == "" || o.APISecret == "" {
		return nil, errors.New("livekitembed: 缺少 api key/secret")
	}
	if o.HTTPPort <= 0 || o.UDPPort <= 0 {
		return nil, fmt.Errorf("livekitembed: 端口非法 http=%d udp=%d", o.HTTPPort, o.UDPPort)
	}

	conf, err := config.NewConfig(buildYAML(o), true, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("livekitembed: 配置: %w", err)
	}
	// 补丁二的回调必须在 NewLocalNode / InitializeServer 之前挂上：
	// 那两步会把 RTCConfig 拷进 rtc.WebRTCConfig。
	conf.RTC.ExternalIPs = o.ExternalIPs

	// 不用 config.InitLoggerFromConfig：它内部还会 slog.SetDefault，而那会连带把标准库
	// log 的默认输出接到 LiveKit 的 zap 上——级别一压到 warn，hearth 自己的 log.Printf
	// 就全被吞掉。这里只装 LiveKit 自己的 logger，不碰进程级默认。
	// 只装一次：logger.SetLogger 写的是包级变量且无锁，重启舞台线时上一实例尚未散尽的
	// goroutine 还在读它。日志配置在几次启动之间不变，装一次就够。
	setLoggerOnce.Do(func() {
		if l, lerr := logger.NewZapLogger(&conf.Logging.Config); lerr == nil {
			logger.SetLogger(l, "livekit")
		}
	})
	if err := conf.ValidateKeys(); err != nil {
		return nil, fmt.Errorf("livekitembed: 密钥: %w", err)
	}

	node, err := routing.NewLocalNode(conf)
	if err != nil {
		return nil, fmt.Errorf("livekitembed: 节点: %w", err)
	}
	// 幂等（内部 initialized.Swap），但必须在 InitializeServer 之前调，否则运行时用到的
	// 指标是 nil。
	if err := prometheus.Init(string(node.NodeID()), node.NodeType()); err != nil {
		return nil, fmt.Errorf("livekitembed: 指标: %w", err)
	}

	lk, err := service.InitializeServer(conf, node)
	if err != nil {
		return nil, fmt.Errorf("livekitembed: 初始化: %w", err)
	}

	s := &Server{lk: lk, opts: o, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.runErr = lk.Start() // 阻塞到 Stop
	}()

	if err := s.waitReady(ctx); err != nil {
		s.Stop()
		return nil, err
	}
	s.logf("进程内 LiveKit 就绪: http=%s udp=%d tcp=%d",
		net.JoinHostPort(bindOr(o.Bind), strconv.Itoa(o.HTTPPort)), o.UDPPort, o.TCPPort)
	return s, nil
}

// Stop 停掉进程内 LiveKit 并等端口释放，可重复调用。
// 整个停止过程有硬上限：LiveKit 的 Stop 与 Start 都要等它自己的关闭序列走完
// （roomManager/signalServer/ioService 逐个收尾），客户端没断干净时那一步可能迟迟不返回——
// 宿主的 SIGTERM 路径不能被它拖住，超时就放手，剩下的交给进程退出。
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.lk.Stop(true)
			<-s.done
		}()
		select {
		case <-done:
			s.waitPortsReleased()
			s.logf("进程内 LiveKit 已停止")
		case <-time.After(stopTimeout):
			s.logf("进程内 LiveKit 停止超时（%s），端口 %d/%d 可能仍被占用", stopTimeout, s.opts.HTTPPort, s.opts.UDPPort)
		}
	})
}

func (s *Server) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(s.probeHost(), strconv.Itoa(s.opts.HTTPPort)))
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case <-s.done:
			if s.runErr != nil {
				return fmt.Errorf("livekitembed: 启动: %w", s.runErr)
			}
			return errors.New("livekitembed: 启动后立即退出")
		case <-ctx.Done():
			return fmt.Errorf("livekitembed: 启动超时: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
			if !s.lk.IsRunning() {
				continue
			}
			resp, err := client.Get(url)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

// waitPortsReleased 轮询到端口能重新 bind 为止：Stop 返回时监听器刚 Close，内核可能还没
// 放掉，紧接着的重启（切换选择器）会撞上「address already in use」。
func (s *Server) waitPortsReleased() {
	deadline := time.Now().Add(stopTimeout)
	for {
		if s.portsFree() {
			return
		}
		if time.Now().After(deadline) {
			s.logf("进程内 LiveKit 端口 %d/%d 超时仍未释放", s.opts.HTTPPort, s.opts.UDPPort)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *Server) portsFree() bool {
	if !tcpFree(s.probeHost(), s.opts.HTTPPort) {
		return false
	}
	if s.opts.TCPPort > 0 && !tcpFree("", s.opts.TCPPort) {
		return false
	}
	pc, err := net.ListenPacket("udp", ":"+strconv.Itoa(s.opts.UDPPort))
	if err != nil {
		return false
	}
	pc.Close()
	return true
}

// probeHost 探活/端口回收检查连的地址：通配监听（0.0.0.0 / ::）从回环连自己。
func (s *Server) probeHost() string {
	switch h := s.opts.Bind; h {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return h
	}
}

func tcpFree(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func (s *Server) logf(format string, args ...any) {
	if s.opts.LogSink != nil {
		s.opts.LogSink(format, args...)
	}
}

// buildYAML 只写我们关心的键，其余全部落在 config.DefaultConfig 上。不配 redis 即本地
// router；显式写死 use_external_ip / stun_servers 是防上游改默认值——探测与打洞由 hearth
// 的 portmap/lite 负责，LiveKit 自己去 gather STUN 只会拖慢每个 PeerConnection。
//
// 浏览器信令必须用 full ICE。ICE-Lite 只被动响应客户端的 connectivity check，在策略路由、
// 多出口或存在可达内网候选的网络里，客户端发来的探测可能走了不可回程的路径，而服务端不会主动
// 尝试其他候选。full ICE 会双向检查并选出真正可达的 candidate pair。
//
// 但 stun_servers: [] 单独写不能禁用服务端 STUN：mediatransportutil 在 full ICE、node_ip
// 未显式指定且 use_external_ip=false 时，会把空列表回落成内置的 google/twilio 服务器。
// WHIP 的一次性信令要等待 gathering 完成，不可达的默认 STUN 会让一次 POST 稳定拖到 10 秒以上。
// 显式 node_ip: 127.0.0.1 只用于绕过该回落分支；advertise_internal_ip 保证 Announcer
// 的初次刷新尚未完成时仍保留真实 host 候选。补丁注入的 ExternalIPs 回调拿到结果后，会在每个
// transport 建立前用 includeInternal=true 整体替换启动时的 rewrite rules，因此稳态候选仍是
// 本机地址加 Announcer 的动态外部地址。
func buildYAML(o Options) string {
	return fmt.Sprintf(`port: %d
bind_addresses: [%q]
keys:
  %q: %q
rtc:
  node_ip: 127.0.0.1
  advertise_internal_ip: true
  udp_port: %d
  tcp_port: %d
  use_external_ip: false
  use_ice_lite: false
  stun_servers: []
room:
  auto_create: true
logging:
  level: %s
`, o.HTTPPort, bindOr(o.Bind), o.APIKey, o.APISecret, o.UDPPort, o.TCPPort, logLevelOr(o.LogLevel))
}
