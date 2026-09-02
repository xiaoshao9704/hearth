// Bellows 独立进程：跑在 LiveKit 同一局域网的机器上收 OBS 的 WHIP 推流并直通发进
// LiveKit，视频不经过 hearth 所在服务器。无状态、无出站依赖（只连 LiveKit）：
// 推流密钥的归属与入场判定由 hearth 在反代前做完并签成短时效通行证（grant）随请求头带来，
// 本进程只本地验签（BELLOWS_SHARED_SECRET 与 hearth 的 bellows_shared_secret 同值）。
// hearth 侧把 bellows_remote_url 指到这里即可。
//
// 环境变量：
//
//	BELLOWS_SHARED_SECRET   必填，与 hearth 的 bellows_shared_secret 相同
//	LIVEKIT_API_URL / LIVEKIT_API_KEY / LIVEKIT_API_SECRET  必填，与 hearth 同名
//	BELLOWS_ADDR            WHIP HTTP 监听地址，默认 :8090
//	BELLOWS_UDP_PORT        媒体 UDP 端口，默认 47710
//	BELLOWS_PUBLIC_IP       向推流端通告的 IP；默认本机出口网卡 IP（局域网推流），外网推流时填公网 IP
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hearth/server/internal/rtc/bellows"
)

func main() {
	lk := map[string]string{
		"bellows_shared_secret": need("BELLOWS_SHARED_SECRET"),
		"livekit_api_url":       need("LIVEKIT_API_URL"),
		"livekit_api_key":       need("LIVEKIT_API_KEY"),
		"livekit_api_secret":    need("LIVEKIT_API_SECRET"),
		"bellows_udp_port":      envOr("BELLOWS_UDP_PORT", "47710"),
		"bellows_public_ip":     envOr("BELLOWS_PUBLIC_IP", localIP()),
	}
	gw := bellows.NewRemote(func(_ context.Context, name string) string { return lk[name] })

	mux := http.NewServeMux()
	mux.Handle("/w", gw.Handler())
	mux.Handle("/w/", gw.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: envOr("BELLOWS_ADDR", ":8090"), Handler: mux}

	go func() {
		log.Printf("bellows 监听于 %s（通告IP=%s）", srv.Addr, lk["bellows_public_ip"])
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// localIP 本机出口网卡 IP：UDP Dial 不发包，只借路由表选网卡。
func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
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
