// Bellows 独立进程：跑在舞台内核同一局域网的机器上收 OBS 的 WHIP 推流并直通发进
// 舞台房间，视频不经过 hearth 所在服务器。无状态、无出站依赖（只连舞台内核）：
// 推流令牌的归属与入场判定由 hearth 在反代前做完并签成短时效通行证（grant）随请求头带来，
// 本进程只本地验签（BELLOWS_SHARED_SECRET 与 hearth 的 bellows_shared_secret 同值）。
// hearth 侧把 bellows_remote_url 指到这里即可。
//
// 环境变量：
//
//	BELLOWS_SHARED_SECRET   必填，与 hearth 的 bellows_shared_secret 相同
//	BELLOWS_SINK            发布出口（rtc.Publisher 实现），默认 livekit
//	LIVEKIT_API_URL / LIVEKIT_API_KEY / LIVEKIT_API_SECRET  sink=livekit 时必填，与 hearth 同名
//	BELLOWS_ADDR            WHIP HTTP 监听地址，默认 :8090
//	BELLOWS_UDP_PORT        媒体 UDP 端口，默认 47710
//	BELLOWS_PUBLIC_IP       向推流端通告的 IP；留空 = 自动宣告全部网卡地址 + STUN 探测的公网映射，显式设置则只通告该地址
//	BELLOWS_STUN_SERVERS    逗号分隔的 STUN 服务器；探测各网卡公网映射用，留空用内置默认
//	PORTMAP_MODE            auto（默认）向默认网关申请 UPnP/PCP/NAT-PMP 映射（媒体 UDP 口与 HTTP 口），
//	                        仅 host 网络或裸机可用；off 关闭
//
// 子命令：`bellows healthcheck` 探活本机 /healthz（容器健康检查用，镜像无 shell/curl，
// 健康检查命令只能是二进制自己）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hearth/server/internal/portmap"
	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/bellows"
	"hearth/server/internal/rtc/lite"
	"hearth/server/internal/rtc/livekitrtc"
	"hearth/server/internal/selfcheck"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := selfcheck.Run(selfcheck.URL(envOr("BELLOWS_ADDR", ":8090"))); err != nil {
			log.Printf("健康检查失败: %v", err)
			os.Exit(1)
		}
		return
	}

	cfg := map[string]string{
		"bellows_shared_secret": need("BELLOWS_SHARED_SECRET"),
		"bellows_udp_port":      envOr("BELLOWS_UDP_PORT", "47710"),
		"bellows_public_ip":     os.Getenv("BELLOWS_PUBLIC_IP"),
		"bellows_stun_servers":  os.Getenv("BELLOWS_STUN_SERVERS"),
		"livekit_api_url":       "",
		"livekit_api_key":       "",
		"livekit_api_secret":    "",
	}
	// 发布出口编译进全部 Publisher 实现，BELLOWS_SINK 选用
	var sink func(context.Context) rtc.Publisher
	switch name := envOr("BELLOWS_SINK", "livekit"); name {
	case "livekit":
		cfg["livekit_api_url"] = need("LIVEKIT_API_URL")
		cfg["livekit_api_key"] = need("LIVEKIT_API_KEY")
		cfg["livekit_api_secret"] = need("LIVEKIT_API_SECRET")
		pub := livekitrtc.New(func(_ context.Context, key string) string { return cfg[key] })
		sink = func(context.Context) rtc.Publisher { return pub }
	default:
		log.Fatalf("未知 BELLOWS_SINK %q（可选: livekit）", name)
	}
	// 端口映射：远端形态跑在别人的局域网里，最需要自动打洞。与 hearth 同形：PORTMAP_MODE=off
	// 时 wants 为空、Mapper 空转，宣告只剩 host 候选 + STUN 探测的公网 IP。
	addr := envOr("BELLOWS_ADDR", ":8090")
	mapper := portmap.New()
	gw := bellows.NewRemote(func(_ context.Context, name string) string { return cfg[name] }, sink, mapper.UDPExternal)

	mux := http.NewServeMux()
	mux.Handle("/w", gw.Handler())
	mux.Handle("/w/", gw.Handler())
	// 纯探活：健康只表示进程活着。宣告探测的刷新走下面的周期任务，不挂在这个匿名端点上。
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		announce := cfg["bellows_public_ip"]
		if announce == "" {
			announce = "自动"
		}
		log.Printf("bellows 监听于 %s（通告IP=%s）", srv.Addr, announce)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// 映射变化后立刻刷新宣告；回调不得阻塞 Mapper 的申请轮次（探测最长 2s），另起协程
	mapper.OnChange = func(portmap.Status) { go gw.RefreshAnnounce(context.Background()) }
	go mapper.Run(ctx, func(context.Context) []portmap.Want {
		if envOr("PORTMAP_MODE", "auto") == "off" {
			return nil
		}
		ws := []portmap.Want{{Proto: "udp", Port: udpPort(cfg["bellows_udp_port"]), Desc: "bellows whip"}}
		if _, port, err := net.SplitHostPort(addr); err == nil {
			if p, err := strconv.Atoi(port); err == nil {
				ws = append(ws, portmap.Want{Proto: "tcp", Port: p, Desc: "bellows http"})
			}
		}
		return ws
	})
	// 宣告探测周期刷新：公网 IP 变化后新会话拿到新候选，不重启、不动在途会话
	go func() {
		t := time.NewTicker(lite.DefaultAnnounceTTL)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				gw.RefreshAnnounce(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	// Run 的 ctx 已经结束，撤销映射必须用新的 ctx
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	mapper.Close(closeCtx)
}

// udpPort 端口环境变量转数字；填错了返回 0，由 Mapper 丢掉这条 want。
func udpPort(v string) int {
	p, _ := strconv.Atoi(strings.TrimSpace(v))
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
