// 通行证（grant）测试：签验往返；篡改 payload / 错 secret / 过期 / offer 哈希不符 /
// key 不符各拒；远端形态 POST 验签建会话；撤销端点验签与幂等；RevokeRemoteSessions 端到端。
package bellows

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSecret = "test-shared-secret"

// remoteCfg 远端形态配置：带共享密钥，避免外网 IP 探测。
func remoteCfg(udpPort string) func(context.Context, string) string {
	return func(_ context.Context, name string) string {
		switch name {
		case "bellows_udp_port":
			return udpPort
		case "bellows_public_ip":
			return "127.0.0.1"
		case "bellows_shared_secret":
			return testSecret
		}
		return ""
	}
}

// mustSign 以测试密钥签 publish 通行证。
func mustSign(t *testing.T, g *Gateway, key, offer string) string {
	t.Helper()
	h, v, err := g.IssueWHIPGrant(context.Background(), key, "chan1", "alice", []byte(offer))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if h != GrantHeader {
		t.Fatalf("签发头应为 %s，实际 %q", GrantHeader, h)
	}
	return v
}

func TestGrantRoundTrip(t *testing.T) {
	p := grantPayload{V: 1, Op: "publish", Key: "k", Room: "r", User: "u", Offer: "abc",
		Exp: time.Now().Add(grantTTL).Unix()}
	v, err := signGrant(testSecret, p)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	got, err := verifyGrant(testSecret, v)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if *got != p {
		t.Fatalf("往返不一致: %+v != %+v", *got, p)
	}
}

func TestGrantReject(t *testing.T) {
	newGrant := func(mutate func(*grantPayload)) string {
		p := grantPayload{V: 1, Op: "publish", Key: "k", Exp: time.Now().Add(grantTTL).Unix()}
		if mutate != nil {
			mutate(&p)
		}
		v, err := signGrant(testSecret, p)
		if err != nil {
			t.Fatalf("签发失败: %v", err)
		}
		return v
	}

	// 篡改 payload：签名覆盖的是原 payload，改后必须验不过
	tampered := newGrant(nil)
	body, sig, _ := strings.Cut(tampered, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	raw[len(raw)-5] ^= 0xff // 翻一个 payload 字节（exp 数字区）
	tampered = base64.RawURLEncoding.EncodeToString(raw) + "." + sig

	cases := map[string]struct {
		secret string
		header string
	}{
		"篡改 payload": {testSecret, tampered},
		"错 secret":  {"wrong-secret", newGrant(nil)},
		"空 secret":  {"", newGrant(nil)},
		"过期":       {testSecret, newGrant(func(p *grantPayload) { p.Exp = time.Now().Add(-time.Hour).Unix() })},
		"版本不符":     {testSecret, newGrant(func(p *grantPayload) { p.V = 2 })},
		"格式错误":     {testSecret, "no-dot-here"},
	}
	for name, c := range cases {
		if _, err := verifyGrant(c.secret, c.header); err == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
}

// 远端形态 POST：有效 grant → 201；无 grant / offer 哈希不符 / key 不符 → 401。
func TestRemotePostGrant(t *testing.T) {
	g := NewRemote(remoteCfg("47722"))
	offer := testOffer("a=rtpmap:96 H264/90000\na=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f")

	post := func(key, body, grant string) int {
		req := httptest.NewRequest("POST", "/w/"+key, strings.NewReader(body))
		if grant != "" {
			req.Header.Set(GrantHeader, grant)
		}
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post("good", offer, ""); code != 401 {
		t.Fatalf("无 grant 期望 401，实际 %d", code)
	}
	// grant 绑定的是另一份 offer：改一个字节即哈希不符（防重放挪用）
	other := mustSign(t, g, "good", offer+" ")
	if code := post("good", offer, other); code != 401 {
		t.Fatalf("offer 哈希不符期望 401，实际 %d", code)
	}
	// grant 的 key 与请求路径里的密钥不一致
	mismatch := mustSign(t, g, "other-key", offer)
	if code := post("good", offer, mismatch); code != 401 {
		t.Fatalf("key 不符期望 401，实际 %d", code)
	}
	if code := post("good", offer, mustSign(t, g, "good", offer)); code != 201 {
		t.Fatalf("有效 grant 期望 201，实际 %d", code)
	}
}

// 撤销端点：验签（无/错 grant → 401）→ 掐会话 → 幂等 204。
func TestRevokeSessions(t *testing.T) {
	g := NewRemote(remoteCfg("47723"))
	offer := testOffer("a=rtpmap:96 H264/90000")
	req := httptest.NewRequest("POST", "/w/good", strings.NewReader(offer))
	req.Header.Set(GrantHeader, mustSign(t, g, "good", offer))
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("建会话期望 201，实际 %d", rec.Code)
	}
	rid := strings.TrimPrefix(rec.Header().Get("Location"), "/w/")

	// revokeGrant 发送指定通行证值的撤销请求（空 = 不带头）。
	revokeGrant := func(header string) int {
		dreq := httptest.NewRequest("DELETE", "/w/sessions/good", nil)
		if header != "" {
			dreq.Header.Set(GrantHeader, header)
		}
		drec := httptest.NewRecorder()
		g.Handler().ServeHTTP(drec, dreq)
		return drec.Code
	}
	signRevoke := func() string {
		v, err := signGrant(testSecret, grantPayload{V: 1, Op: "revoke", Key: "good",
			Exp: time.Now().Add(grantTTL).Unix()})
		if err != nil {
			t.Fatalf("签发失败: %v", err)
		}
		return v
	}

	// 无 grant 与 op 不符（拿 publish grant 当 revoke 用）都拒
	if code := revokeGrant(""); code != 401 {
		t.Fatalf("无 grant 期望 401，实际 %d", code)
	}
	if code := revokeGrant(mustSign(t, g, "good", offer)); code != 401 {
		t.Fatalf("publish grant 当 revoke 用期望 401，实际 %d", code)
	}
	if code := revokeGrant(signRevoke()); code != 204 {
		t.Fatalf("撤销期望 204，实际 %d", code)
	}
	if g.HasSession(rid) {
		t.Fatal("撤销后会话应已掐断")
	}
	if code := revokeGrant(signRevoke()); code != 204 {
		t.Fatalf("重复撤销应幂等 204，实际 %d", code)
	}
}

// RevokeRemoteSessions 端到端：hearth 侧 Gateway（配 remote_url）→ 远端 Handler()。
func TestRevokeRemoteSessions(t *testing.T) {
	remote := NewRemote(remoteCfg("47724"))
	srv := httptest.NewServer(remote.Handler())
	defer srv.Close()

	offer := testOffer("a=rtpmap:96 H264/90000")
	req := httptest.NewRequest("POST", "/w/good", strings.NewReader(offer))
	req.Header.Set(GrantHeader, mustSign(t, remote, "good", offer))
	rec := httptest.NewRecorder()
	remote.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("建会话期望 201，实际 %d", rec.Code)
	}
	rid := strings.TrimPrefix(rec.Header().Get("Location"), "/w/")

	// hearth 侧：同一 secret + remote_url 指向测试服务器
	hearthSide := New(func(_ context.Context, name string) string {
		switch name {
		case "bellows_remote_url":
			return srv.URL
		case "bellows_shared_secret":
			return testSecret
		}
		return ""
	}, nil)
	if err := hearthSide.RevokeRemoteSessions(context.Background(), "good"); err != nil {
		t.Fatalf("远端撤销失败: %v", err)
	}
	if remote.HasSession(rid) {
		t.Fatal("远端会话应已被掐断")
	}
	// 无该密钥会话时仍应成功（幂等）
	if err := hearthSide.RevokeRemoteSessions(context.Background(), "good"); err != nil {
		t.Fatalf("重复远端撤销应幂等: %v", err)
	}
}
