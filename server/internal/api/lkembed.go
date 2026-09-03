// 进程内 LiveKit（内建实例 lkembed）的接线：配置映射、密钥生成、按舞台选择器启停。
// 实例对象本身复用 livekitrtc.New——舞台槽位、令牌签发、/providers/lkembed/rtc 反代、
// Bellows 的 Publisher 全部零改动，只是把 livekit_api_url 指向回环。
package api

import (
	"context"
	"log"
	"strings"

	"hearth/server/internal/rtc/lite"
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
func (a *API) stageExternalIPs() []string {
	externals, _ := a.ember.AnnounceSnapshot()
	return lite.ExternalIPv4s(externals)
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
		a.registerStageAnnouncePort(0) // 撤销登记：快照不再回显舞台端口的映射结果
		return
	}
	key, secret, err := a.ensureEmbedKeys(ctx)
	if err != nil {
		log.Printf("进程内 LiveKit 密钥落库失败，舞台线未启动: %v", err)
		return
	}
	udpPort := dynPort(a.dynVal(ctx, "lkembed_udp_port"))
	srv, err := livekitembed.Start(ctx, livekitembed.Options{
		HTTPPort:    dynPort(a.dynVal(ctx, "lkembed_port")),
		UDPPort:     udpPort,
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
	// 显式登记舞台 UDP 端口：LiveKit 自己建 PeerConnection、不经过 ember 的 Announce()，
	// 不登记的话 Announcer 要等语音线出过一条 SDP 才知道有个端口需要查映射（见
	// lite.Announcer 的 registered 字段注释），语音线一次没用过时舞台端口的映射结果
	// 就进不了 stageExternalIPs 的快照。
	a.registerStageAnnouncePort(udpPort)
}

// registerStageAnnouncePort 见 EnsureStageKernel 调用处的注释；a.ember 恒非空（New 里已建），
// 这里仍判空防御式编程，避免测试或未来重构漏初始化时 panic。
func (a *API) registerStageAnnouncePort(port int) {
	if a.ember != nil {
		a.ember.RegisterAnnouncePort(AliasLkembed, port)
	}
}

// StopStageKernel 进程退出时收尾。
func (a *API) StopStageKernel() {
	a.embedMu.Lock()
	srv := a.embedSrv
	a.embedSrv = nil
	a.embedMu.Unlock()
	a.registerStageAnnouncePort(0)
	if srv != nil {
		srv.Stop()
	}
}

// stageKernelRunning 进程内 LiveKit 当前是否在跑（推流面 lkembed 的 Enabled 据此判断：
// 舞台线没选中 lkembed 时服务端根本没起，推过来只会撞连接失败）。
func (a *API) stageKernelRunning(context.Context) bool {
	a.embedMu.Lock()
	defer a.embedMu.Unlock()
	return a.embedSrv != nil
}
