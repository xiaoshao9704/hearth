// 进程内 LiveKit（内建实例 lkembed）的接线：配置映射、密钥生成、按舞台选择器启停。
// 实例对象本身复用 livekitrtc.New——舞台槽位、令牌签发、/providers/lkembed/rtc 反代、
// Bellows 的 Publisher 全部零改动，只是把 livekit_api_url 指向回环。
package api

import (
	"context"
	"log"
	"net/netip"
	"strings"

	"hearth/server/internal/rtc/livekitembed"
)

// embedCfg 把 livekitrtc 要的 livekit_* 键映射到 lkembed_* 全局键：
// API 地址固定为回环，浏览器地址留空（走同源信令代理）。
func (a *API) embedCfg(ctx context.Context, name string) string {
	switch name {
	case "livekit_api_url":
		return "http://127.0.0.1:" + strings.TrimSpace(a.dynVal(ctx, "lkembed_port"))
	case "livekit_api_key":
		return a.dynVal(ctx, "lkembed_api_key")
	case "livekit_api_secret":
		return a.dynVal(ctx, "lkembed_api_secret")
	}
	return ""
}

// ensureEmbedKeys 密钥留空时生成一对并落库（之后不变，随数据库备份）。
// secret 取 32 字节 hex：LiveKit 要求 >= 32 字符。
func (a *API) ensureEmbedKeys(ctx context.Context) (key, secret string, err error) {
	key = a.dynVal(ctx, "lkembed_api_key")
	if key == "" {
		key = "hearth" + randHex(6)
		if err = a.st.SetSetting(ctx, "cfg_lkembed_api_key", key); err != nil {
			return "", "", err
		}
	}
	secret = a.dynVal(ctx, "lkembed_api_secret")
	if secret == "" {
		secret = randHex(32)
		if err = a.st.SetSetting(ctx, "cfg_lkembed_api_secret", secret); err != nil {
			return "", "", err
		}
	}
	return key, secret, nil
}

// stageExternalIPs 补丁二的回调：进程内 LiveKit 每建一个 PeerConnection 取一次当前外部
// IPv4，追加为候选。数据源是 Ember 那一个 Announcer 的快照（映射结果排最前，其次 STUN
// 探测结果）——同一台机器只有一个公网地址，不另起第二个探测器。
// 快照给的是地址（可能带端口），这里只取 IP：LiveKit 的改写只换 IP 不换端口。
func (a *API) stageExternalIPs() []string {
	externals, _ := a.ember.AnnounceSnapshot()
	var out []string
	seen := map[string]bool{}
	for _, e := range externals {
		ip := e
		if ap, err := netip.ParseAddrPort(e); err == nil {
			ip = ap.Addr().String()
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil || !addr.Is4() || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	return out
}

// EnsureStageKernel 按当前 stage_provider 启停进程内 LiveKit：选中 lkembed 才起，
// 切走就停（纯语音部署零额外端口与日志）。幂等，可重复调用；启动失败只记日志，
// 舞台线起不来不拖垮语音线。
func (a *API) EnsureStageKernel(ctx context.Context) {
	a.embedMu.Lock()
	defer a.embedMu.Unlock()
	want := a.dynVal(ctx, "stage_provider") == AliasLkembed
	if want == (a.embedSrv != nil) {
		return
	}
	if !want {
		srv := a.embedSrv
		a.embedSrv = nil
		srv.Stop()
		return
	}
	key, secret, err := a.ensureEmbedKeys(ctx)
	if err != nil {
		log.Printf("进程内 LiveKit 密钥落库失败，舞台线未启动: %v", err)
		return
	}
	srv, err := livekitembed.Start(ctx, livekitembed.Options{
		HTTPPort:    dynPort(a.dynVal(ctx, "lkembed_port")),
		UDPPort:     dynPort(a.dynVal(ctx, "lkembed_udp_port")),
		TCPPort:     dynPort(a.dynVal(ctx, "lkembed_tcp_port")),
		APIKey:      key,
		APISecret:   secret,
		ExternalIPs: a.stageExternalIPs,
		LogSink:     log.Printf,
	})
	if err != nil {
		log.Printf("进程内 LiveKit 启动失败（舞台线不可用，语音线照常）: %v", err)
		return
	}
	a.embedSrv = srv
}

// StopStageKernel 进程退出时收尾。
func (a *API) StopStageKernel() {
	a.embedMu.Lock()
	srv := a.embedSrv
	a.embedSrv = nil
	a.embedMu.Unlock()
	if srv != nil {
		srv.Stop()
	}
}
