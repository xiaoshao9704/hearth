// 下发给浏览器的 ICE 服务器列表改写规则（在信令反代层生效，见 signalbridge.go）。
// 为什么在 hearth 改而不是给 livekit 打补丁：补丁只对 fork 实例生效，注册制里接入官方
// LiveKit 实例时它照样下发内置的境外默认 STUN；而浏览器信令一律经 /providers/{alias}/rtc
// 进来，改在这里对所有实例类型一视同仁，且改配置即生效不必重启。
package api

import (
	"strings"

	"github.com/livekit/protocol/livekit"
)

// defaultClientSTUN 默认并列两个不同地域的 STUN：浏览器并行探测、谁先回用谁，
// 选错也不影响连通性（客户端永远是主动方，服务端从收到的包按 peer-reflexive 学到对端地址）。
const defaultClientSTUN = "stun.miwifi.com:3478,stun.l.google.com:19302"

// clientICEServers 上游给的列表 → 下发给浏览器的列表。
// 只剔除纯 STUN 项（LiveKit 未配 stun/turn 时会回落到内置默认表，那是我们不想要的）；
// 含 turn/turns 的项一律原样保留——TURN 凭证由 LiveKit 按参与者短时签发，不经我们的手。
func clientICEServers(in []*livekit.ICEServer, cfg string) []*livekit.ICEServer {
	out := make([]*livekit.ICEServer, 0, len(in)+1)
	for _, s := range in {
		if hasTURNURL(s.GetUrls()) {
			out = append(out, s)
		}
	}
	if urls := clientSTUNURLs(cfg); len(urls) > 0 {
		out = append(out, &livekit.ICEServer{Urls: urls})
	}
	return out
}

func hasTURNURL(urls []string) bool {
	for _, u := range urls {
		l := strings.ToLower(strings.TrimSpace(u))
		if strings.HasPrefix(l, "turn:") || strings.HasPrefix(l, "turns:") {
			return true
		}
	}
	return false
}

// clientSTUNURLs 解析 client_stun_servers 的取值：空 = 默认列表，none = 一条都不下发。
func clientSTUNURLs(cfg string) []string {
	cfg = strings.TrimSpace(cfg)
	if cfg == "" {
		cfg = defaultClientSTUN
	}
	if strings.EqualFold(cfg, "none") {
		return nil
	}
	var urls []string
	for _, part := range strings.Split(cfg, ",") {
		host := strings.TrimSpace(part)
		if host == "" {
			continue
		}
		if l := strings.ToLower(host); !strings.HasPrefix(l, "stun:") && !strings.HasPrefix(l, "stuns:") {
			host = "stun:" + host
		}
		urls = append(urls, host)
	}
	return urls
}
