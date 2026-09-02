package rtc

import (
	"net/http/httptest"
	"testing"
)

// WHIPToken 两种填法：bearer 模式（/w/{channel} + Bearer 头，bearer=true）与
// 路径模式（/w/{channel}/{token}）；非 POST 的 /w/sessions/{rid} 走 WHIPSessionRID。
func TestWHIPToken(t *testing.T) {
	cases := []struct {
		name        string
		method, url string
		bearerHdr   string
		channel     string
		token       string
		bearer      bool
	}{
		{"bearer 模式", "POST", "/w/chan1", "tok123", "chan1", "tok123", true},
		{"路径模式", "POST", "/w/chan1/tok123", "", "chan1", "tok123", false},
		{"路径模式带多余段", "POST", "/w/chan1/tok123/extra", "", "chan1", "tok123", false},
		{"bearer 无头", "POST", "/w/chan1", "", "chan1", "", true},
		{"路径令牌优先于 Bearer 头", "POST", "/w/chan1/tok123", "other", "chan1", "tok123", false},
		{"裸 /w", "POST", "/w", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.url, nil)
			if c.bearerHdr != "" {
				r.Header.Set("Authorization", "Bearer "+c.bearerHdr)
			}
			channel, token, bearer := WHIPToken(r)
			if channel != c.channel || token != c.token || bearer != c.bearer {
				t.Fatalf("期望 (%q,%q,%v)，实际 (%q,%q,%v)",
					c.channel, c.token, c.bearer, channel, token, bearer)
			}
		})
	}
}

func TestWHIPSessionRID(t *testing.T) {
	r := httptest.NewRequest("DELETE", "/w/sessions/abc123", nil)
	if rid := WHIPSessionRID(r); rid != "abc123" {
		t.Fatalf("期望 abc123，实际 %q", rid)
	}
	// PATCH 同样打会话资源地址
	r = httptest.NewRequest("PATCH", "/w/sessions/abc123", nil)
	if rid := WHIPSessionRID(r); rid != "abc123" {
		t.Fatalf("期望 abc123，实际 %q", rid)
	}
}
