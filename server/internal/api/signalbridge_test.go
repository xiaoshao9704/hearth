// 信令桥的端到端测试：起真实的进程内 LiveKit，经 hearth 的 /providers/lkembed/rtc
// 建立 WebSocket，断言首帧 Join 里下发给浏览器的 ICE 服务器已按 client_stun_servers 改写。
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"

	"hearth/server/internal/lktoken"
	"hearth/server/internal/rtc"
)

// signalJoin 经 hearth 反代握手并读到首帧 Join。
func signalJoin(t *testing.T, a *API, base, channel, tag string) *livekit.JoinResponse {
	t.Helper()
	ctx := context.Background()
	key, secret := a.dynVal(ctx, "lkembed_api_key"), a.dynVal(ctx, "lkembed_api_secret")
	if key == "" || secret == "" {
		t.Fatal("进程内 LiveKit 密钥未生成")
	}
	tok, err := lktoken.Sign(key, secret, channel, rtc.Meta{UID: 4242, Username: "ice", Tag: tag}, true)
	if err != nil {
		t.Fatalf("签令牌: %v", err)
	}
	u := fmt.Sprintf("%s/providers/%s/rtc?access_token=%s&protocol=15&auto_subscribe=true&sdk=go",
		strings.Replace(base, "http://", "ws://", 1), AliasLkembed, tok)

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, u, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("经反代握手失败（HTTP %d）: %v", code, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(signalReadLimit)

	for {
		typ, data, err := conn.Read(dialCtx)
		if err != nil {
			t.Fatalf("读信令帧失败: %v", err)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("期望 binary 信令帧，实际 %v", typ)
		}
		var msg livekit.SignalResponse
		if err := proto.Unmarshal(data, &msg); err != nil {
			t.Fatalf("解析信令帧失败: %v", err)
		}
		if join := msg.GetJoin(); join != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			return join
		}
	}
}

func iceURLs(servers []*livekit.ICEServer) []string {
	var out []string
	for _, s := range servers {
		out = append(out, s.GetUrls()...)
	}
	return out
}

func TestSignalBridgeRewritesJoinICEServers(t *testing.T) {
	a, base, _ := stageAPI(t)
	ctx := context.Background()

	// none：一条 STUN 都不下发（上游未配 stun/turn 时本会回落到内置的境外默认表）
	if err := a.st.SetSetting(ctx, "cfg_client_stun_servers", "none"); err != nil {
		t.Fatal(err)
	}
	got := iceURLs(signalJoin(t, a, base, "icechan", "none").GetIceServers())
	for _, u := range got {
		if strings.HasPrefix(strings.ToLower(u), "stun:") {
			t.Fatalf("none 时仍下发 STUN: %v", got)
		}
	}

	// 自定义：恰好下发配置项，内置默认表不得漏出
	if err := a.st.SetSetting(ctx, "cfg_client_stun_servers", "stun.example.com:3478"); err != nil {
		t.Fatal(err)
	}
	got = iceURLs(signalJoin(t, a, base, "icechan", "custom").GetIceServers())
	if len(got) != 1 || got[0] != "stun:stun.example.com:3478" {
		t.Fatalf("下发列表=%v, want [stun:stun.example.com:3478]", got)
	}
}

// 令牌无效时上游在升级前就用普通 HTTP 应答拒绝，桥要原样回给客户端（SDK 靠状态码区分
// "票过期"与"服务不可达"），不能一律吞成 502。
func TestSignalBridgeRelaysHandshakeRejection(t *testing.T) {
	_, base, _ := stageAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u := strings.Replace(base, "http://", "ws://", 1) + "/providers/" + AliasLkembed + "/rtc?access_token=bogus&protocol=15"
	conn, resp, err := websocket.Dial(ctx, u, nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("无效令牌不应握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("状态码=%d, want %d", code, http.StatusUnauthorized)
	}
}
