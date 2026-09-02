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

// identity 主体是 user_id：改名不影响归属，且互为前缀的用户名不再互相命中。
// 旧实现（identity 拼用户名 + 前缀匹配）在这两处都会错——用户名允许含 `-`，
// 用户 alice 的推流 identity "alice-obs" 与用户 "alice-obs" 的名字同形，
// 禁言后者会连带掐掉前者的推流。
func TestMatchesUserByID(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		userID   int64
		want     bool
	}{
		{"无设备标签", Identity(17, ""), 17, true},
		{"带设备标签", Identity(17, "mac-a1b2"), 17, true},
		{"推流标签", Identity(17, "obs"), 17, true},
		{"标签含分隔符", Identity(17, "cam-1-x"), 17, true},
		{"别的用户", Identity(18, "obs"), 17, false},
		{"id 前缀不算命中", Identity(170, "obs"), 17, false},
		{"空 identity", "", 17, false},
		{"用户名形状的 identity 不再命中", "alice-obs", 17, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchesUser(c.identity, c.userID); got != c.want {
				t.Fatalf("MatchesUser(%q, %d) = %v，期望 %v", c.identity, c.userID, got, c.want)
			}
		})
	}
}

// 归属只认 user_id：同一用户改名前后 identity 不变，管制照常命中。
func TestIdentityStableAcrossRename(t *testing.T) {
	before := Identity(42, "mac-a1b2") // 用户名 alice 时
	after := Identity(42, "mac-a1b2")  // 改名成 bob 之后
	if before != after {
		t.Fatalf("改名不应影响 identity: %q → %q", before, after)
	}
	if !MatchesUser(after, 42) {
		t.Fatalf("改名后仍应归属原用户: %q", after)
	}
}
