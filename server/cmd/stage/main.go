// Stage 独立进程：舞台内核（补丁式 fork 的 LiveKit）跑在另一台机器上。自打洞（portmap）、
// 自宣告（lite.Announcer 的探测周期刷新，公网 IP 变化只影响新会话），
// 无外部依赖：不连 hearth、不要 redis。
//
// hearth 侧接线与外部 LiveKit 完全一样：LIVEKIT_API_URL 指向本进程的 API 地址（私网通道）
// 合成 env 锁定的 livekit 实例，后台把 stage_provider 选成它（推流无独立选择器，
// OBS 的 WHIP 一律进当前舞台实例）——浏览器信令与 OBS 的 WHIP 都经 hearth 反代到这里，
// 媒体走同一个打洞出来的 UDP 端口。入场判定仍在 hearth 做完（推流经 admitIngest 后由 hearth
// 现签短时效 LiveKit JWT 换票），本进程只认票。
//
// 环境变量：
//
//	STAGE_API_KEY / STAGE_API_SECRET  必填，与 hearth 侧 LIVEKIT_API_KEY/SECRET 同值
//	STAGE_HTTP_PORT   API/信令监听端口，默认 7880
//	STAGE_BIND        监听地址，默认 0.0.0.0（hearth 要经私网访问 API，不能只回环）
//	STAGE_UDP_PORT    媒体 UDP 单端口，默认 47720
//	STAGE_TCP_PORT    ICE-TCP 端口，默认 0（关；UDP 全被封的网络里才需要）
//	STAGE_PUBLIC_IP   显式公网 IP；留空 = 自动（端口映射结果 + STUN 探测）
//	STAGE_STUN_SERVERS 逗号分隔的 STUN 服务器，留空用内置默认
//	STAGE_LOG_LEVEL   LiveKit 自己的日志级别，默认 warn
//	PORTMAP_MODE      auto（默认）向默认网关申请 UPnP/PCP/NAT-PMP 映射（媒体端口），
//	                  仅 host 网络或裸机可用；off 关闭
//
// 子命令：`stage healthcheck` 探活本机 API 端口（容器健康检查用，镜像无 shell/curl）。
// 健康只表示进程活着：宣告探测的刷新走进程内周期任务，不挂在健康检查上。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc/lite"
	"hearth/server/internal/rtc/livekitembed"
	"hearth/server/internal/selfcheck"
)

func main() {
	httpPort := port("STAGE_HTTP_PORT", 7880)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// LiveKit 没有 /healthz，用它的根路径（200 即服务端已就绪）
		if err := selfcheck.Run("http://127.0.0.1:" + strconv.Itoa(httpPort) + "/"); err != nil {
			log.Printf("健康检查失败: %v", err)
			os.Exit(1)
		}
		return
	}

	udpPort, tcpPort := port("STAGE_UDP_PORT", 47720), port("STAGE_TCP_PORT", 0)
	// 宣告：本进程自己的 Announcer，显式登记媒体端口——LiveKit 的
	// PeerConnection 由它自己建、不经 Announcer 的 Announce()，不登记的话快照查不到
	// 映射结果（见 lite.Announcer 的 registered 字段注释）。
	mapper := portmap.New()
	ann := lite.NewAnnouncer(
		envFunc(os.Getenv("STAGE_PUBLIC_IP")),
		envFunc(os.Getenv("STAGE_STUN_SERVERS")),
		mapper.UDPExternal)
	ann.RegisterMediaPort("stage", udpPort)

	srv, err := livekitembed.Start(context.Background(), livekitembed.Options{
		HTTPPort:  httpPort,
		Bind:      envOr("STAGE_BIND", "0.0.0.0"),
		UDPPort:   udpPort,
		TCPPort:   tcpPort,
		APIKey:    need("STAGE_API_KEY"),
		APISecret: need("STAGE_API_SECRET"),
		LogLevel:  envOr("STAGE_LOG_LEVEL", "warn"),
		ExternalIPs: func() []string {
			externals, _ := ann.Snapshot()
			return lite.ExternalIPv4s(externals)
		},
		LogSink: log.Printf,
	})
	if err != nil {
		log.Fatalf("舞台内核启动失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// 映射变化后立刻刷新宣告；回调不得阻塞 Mapper 的申请轮次（探测最长 2s），另起协程
	mapper.OnChange = func(portmap.Status) { go ann.Refresh(context.Background()) }
	go mapper.Run(ctx, func(context.Context) []portmap.Want {
		if envOr("PORTMAP_MODE", "auto") == "off" {
			return nil
		}
		// StrictPort：LiveKit 的地址改写（补丁二）只换 IP 不换端口，网关改派了外部端口
		// 宣告出去的候选就是错的，宁可判定失败给 port_conflict 诊断。
		// API/信令端口不申请映射：浏览器信令经 hearth 反代过来，Twirp 管理接口不该暴露公网。
		ws := []portmap.Want{{Proto: "udp", Port: udpPort, Desc: "hearth stage", StrictPort: true}}
		if tcpPort > 0 {
			ws = append(ws, portmap.Want{Proto: "tcp", Port: tcpPort, Desc: "hearth stage", StrictPort: true})
		}
		return ws
	})
	// 宣告探测周期刷新：公网 IP 变化后新会话拿到新候选，不重启、不动在途会话。
	// 先立刻探一次，别让开机后的头几条会话只带 host 候选。
	go func() {
		ann.Refresh(ctx)
		t := time.NewTicker(lite.DefaultAnnounceTTL)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ann.Refresh(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
	<-ctx.Done()
	srv.Stop() // 内部有 10s 上限，不会拖住退出
	// Run 的 ctx 已经结束，撤销映射必须用新的 ctx
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	mapper.Close(closeCtx)
}

// envFunc 把一个固定的环境变量值包成配置 getter（本进程的配置只来自环境，不会变）。
func envFunc(v string) func(context.Context) string {
	return func(context.Context) string { return v }
}

// port 端口环境变量转数字；填错了当没填，用默认值。
func port(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	p, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("环境变量 %s=%q 不是端口号，按默认值 %d 处理", k, v, def)
		return def
	}
	return p
}

func need(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("缺少环境变量 %s", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
