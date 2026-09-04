package livekitrtc

import (
	"context"
	"testing"

	"hearth/server/internal/rtc"
)

// staticCfg 固定配置值的 rtc.ConfigFunc。
func staticCfg(values map[string]string) rtc.ConfigFunc {
	return func(_ context.Context, name string) string { return values[name] }
}

// livekit_api_url 填 ws(s)://（照抄浏览器信令地址的常见填法）时，Twirp 管理面与信令反代
// 上游都要归一成 http(s)://，否则报 unsupported protocol scheme "ws"。
func TestAPIURLNormalizesWSScheme(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ws://10.0.0.2:7880", "http://10.0.0.2:7880"},
		{"wss://lk.example.com", "https://lk.example.com"},
		{"http://10.0.0.2:7880", "http://10.0.0.2:7880"},
	} {
		p := New(staticCfg(map[string]string{"livekit_api_url": tc.in}))
		if got := p.apiURL(context.Background()); got != tc.want {
			t.Fatalf("apiURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
		if got := p.SignalProxyUpstream(context.Background()); got != tc.want {
			t.Fatalf("SignalProxyUpstream(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
		w := NewWHIP(staticCfg(map[string]string{"livekit_api_url": tc.in}), nil, nil)
		if got := w.whipBase(context.Background()); got != tc.want {
			t.Fatalf("whipBase(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
