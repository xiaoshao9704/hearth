// 信令 WebSocket 桥：/providers/{alias}/rtc 的 Upgrade 请求不走 httputil.ReverseProxy
// （它 hijack 后只做双向 io.Copy，看不见帧），改用手写桥逐帧转发，以便改写 Join/Reconnect
// 里下发给浏览器的 ICE 服务器（见 iceservers.go）。/rtc/validate 等普通 HTTP 仍走反代。
package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

const (
	// signalReadLimit 单帧上限：JoinResponse 与 SDP 都远小于此，只防异常大帧撑爆内存。
	signalReadLimit = 1 << 20
	// signalCloseGrace 一个方向结束后等另一个方向收尾的宽限；超时由 defer 的 CloseNow 兜底。
	signalCloseGrace = 3 * time.Second
)

// isWebSocketUpgrade 该请求是否是 WebSocket 握手（其余请求保持走 ReverseProxy）。
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			return true
		}
	}
	return false
}

// bridgeSignal 客户端 ⇄ 上游的逐帧转发。
func (a *API) bridgeSignal(w http.ResponseWriter, r *http.Request, upstream, stripPrefix string) {
	if upstream == "" {
		writeErr(w, http.StatusServiceUnavailable, "上游未配置")
		return
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" {
		log.Printf("反代上游地址无效（%q）: %v", upstream, err)
		writeErr(w, http.StatusBadGateway, "上游地址无效")
		return
	}
	stunCfg := a.dynVal(r.Context(), "client_stun_servers")

	// 桥的生命周期不能挂 r.Context()：websocket.Accept hijack 后 net/http 会取消它
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	// 先连上游再 Accept 客户端：LiveKit 在升级前就校验令牌，失败是普通 HTTP 应答，
	// 原样回给客户端（SDK 靠状态码区分"票过期"与"服务不可达"），与 ReverseProxy 一致。
	up, resp, err := websocket.Dial(ctx, signalUpstreamURL(target, r, stripPrefix), &websocket.DialOptions{
		HTTPHeader:   signalDialHeader(r),
		Subprotocols: requestSubprotocols(r),
		Host:         target.Host,
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			relayHandshakeFailure(w, resp)
			return
		}
		log.Printf("信令上游握手失败: %v", err)
		writeErr(w, http.StatusBadGateway, "上游不可达")
		return
	}
	defer up.CloseNow()
	up.SetReadLimit(signalReadLimit)

	var subs []string
	if sp := up.Subprotocol(); sp != "" {
		subs = []string{sp}
	}
	// InsecureSkipVerify：既有 ReverseProxy 对 Origin 不作限制，桥不引入新限制
	cli, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       subs,
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer cli.CloseNow()
	cli.SetReadLimit(signalReadLimit)

	// 每个方向一个 goroutine，各自只写自己那一端（nhooyr 不允许并发写）
	done := make(chan struct{}, 2)
	go func() { defer func() { done <- struct{}{} }(); pumpSignal(ctx, cli, up, nil) }()
	go func() {
		defer func() { done <- struct{}{} }()
		pumpSignal(ctx, up, cli, func(b []byte) []byte { return rewriteSignalFrame(b, stunCfg) })
	}()

	// 任一方向结束时已把关闭码写给对端，另一方向随之读到关闭帧退出；
	// 对端不回应时靠 defer 的 CloseNow 强断，goroutine 不会滞留。
	<-done
	select {
	case <-done:
	case <-time.After(signalCloseGrace):
	}
}

// pumpSignal 单向转发；rewrite 非 nil 时对 binary 帧尝试改写。
// 读到关闭帧就把关闭码与原因透传给对端；其它读错误按异常断开处理（不伪造正常关闭码）。
// 写失败不额外处理：两个方向共用同一对连接，对端很快也会读失败。
func pumpSignal(ctx context.Context, src, dst *websocket.Conn, rewrite func([]byte) []byte) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				dst.Close(ce.Code, truncateCloseReason(ce.Reason))
			} else {
				dst.CloseNow()
			}
			return
		}
		if rewrite != nil {
			if typ == websocket.MessageBinary {
				data = rewrite(data)
			} else {
				warnTextSignal.Do(func() {
					log.Printf("信令为 text 帧（JSON 模式），ICE 服务器改写不生效，原样透传")
				})
			}
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

var warnTextSignal sync.Once

// truncateCloseReason 关闭原因按协议不得超过 125 字节。
func truncateCloseReason(s string) string {
	if len(s) <= 123 {
		return s
	}
	return strings.ToValidUTF8(s[:123], "")
}

// rewriteSignalFrame 上游 → 客户端的 binary 帧：只在 Join/Reconnect 上改 IceServers。
// 解析失败或其它消息一律原样返回——信令绝不因解析问题断连。
func rewriteSignalFrame(data []byte, stunCfg string) []byte {
	var msg livekit.SignalResponse
	if err := proto.Unmarshal(data, &msg); err != nil {
		return data
	}
	switch {
	case msg.GetJoin() != nil:
		j := msg.GetJoin()
		j.IceServers = clientICEServers(j.GetIceServers(), stunCfg)
	case msg.GetReconnect() != nil:
		rc := msg.GetReconnect()
		rc.IceServers = clientICEServers(rc.GetIceServers(), stunCfg)
	default:
		return data
	}
	out, err := proto.Marshal(&msg)
	if err != nil {
		return data
	}
	return out
}

// signalUpstreamURL 组上游 WebSocket 地址；路径与 query 的处理与 newReverseProxy 的
// Director 同规则（剥前缀、拼上游基路径、并入上游固定参数）。
func signalUpstreamURL(target *url.URL, req *http.Request, stripPrefix string) string {
	u := *target
	if u.Scheme == "https" || u.Scheme == "wss" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	p := req.URL.Path
	if stripPrefix != "" {
		p = strings.TrimPrefix(p, stripPrefix)
	}
	if base := strings.TrimSuffix(target.Path, "/"); base != "" {
		p = base + p
	}
	if p == "" {
		p = "/"
	}
	u.Path, u.RawPath = p, ""
	u.RawQuery = req.URL.RawQuery
	if target.RawQuery != "" {
		if req.URL.RawQuery == "" {
			u.RawQuery = target.RawQuery
		} else {
			u.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
		}
	}
	return u.String()
}

// signalHopHeaders 不转发的请求头：逐跳头，以及由 websocket.Dial 自己重新生成的握手头。
var signalHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol",
	"Sec-Websocket-Extensions", "Host",
}

// signalDialHeader 把客户端请求头带给上游（Authorization 等原样过），并追加 X-Forwarded-For。
func signalDialHeader(r *http.Request) http.Header {
	h := r.Header.Clone()
	for _, k := range signalHopHeaders {
		h.Del(k)
	}
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := h.Get("X-Forwarded-For"); prior != "" {
			ip = prior + ", " + ip
		}
		h.Set("X-Forwarded-For", ip)
	}
	return h
}

func requestSubprotocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values("Sec-Websocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// relayHandshakeFailure 上游在升级前拒绝（令牌无效/房间满等）时把应答原样回给客户端。
func relayHandshakeFailure(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if k == "Connection" || k == "Upgrade" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
