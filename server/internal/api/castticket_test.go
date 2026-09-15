// 设备票链路测试：签发端点（判定、设备标识校验、返回形状）与 admitIngest 的认票分支
// （验签、过期、频道不符、撤权后立即失效）。
package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"hearth/server/internal/rtc"
)

type castTicketResp struct {
	Ticket    string `json:"ticket"`
	Base      string `json:"base"`
	ExpiresIn int    `json:"expires_in"`
}

// 签发：正常拿票（形状 + base 与推流令牌一致）、非法设备标识 400、未登录 401、
// 禁言后拒发；兑换：identity/meta 按设备标签组，篡改/过期/频道不符一律 403。
func TestCastTicketIssueAndAdmit(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	c1, err := a.st.CreateChannel(ctx, "chan1", u.ID)
	if err != nil {
		t.Fatalf("建频道失败: %v", err)
	}
	if _, err := a.st.CreateChannel(ctx, "chan2", u.ID); err != nil {
		t.Fatalf("建频道失败: %v", err)
	}
	sess, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	r := a.Router()

	issue := func(channel string, token string, body any) (int, castTicketResp) {
		rec := doReq(t, r, "POST", "/api/channels/"+channel+"/cast-ticket", token, body)
		var out castTicketResp
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, got := issue("chan1", sess, map[string]string{"device_id": "ab12cd34"})
	if code != 200 || got.Ticket == "" || got.ExpiresIn != 600 {
		t.Fatalf("签发应成功: %d %+v", code, got)
	}
	if got.Base != "http://example.com/providers/lkembed/w/" {
		t.Fatalf("base 应与推流令牌一致，实际 %q", got.Base)
	}
	if code, _ := issue("chan1", sess, map[string]string{"device_id": "??"}); code != 400 {
		t.Fatalf("非法设备标识应 400，实际 %d", code)
	}
	if code, _ := issue("chan1", "", map[string]string{"device_id": "ab12cd34"}); code != 401 {
		t.Fatalf("未登录应 401，实际 %d", code)
	}
	if code, _ := issue("nosuch", sess, map[string]string{"device_id": "ab12cd34"}); code != 404 {
		t.Fatalf("未知频道应 404，实际 %d", code)
	}

	admit := func(channel, token string) (int, ingestAdmission, bool) {
		rec := httptest.NewRecorder()
		adm, ok := a.admitIngest(ctx, rec, AliasLkembed, channel, token)
		return rec.Code, adm, ok
	}

	// 兑换：identity = u{uid}-cast-{device_id}，元数据按设备票的标签下发
	code, adm, ok := admit("chan1", got.Ticket)
	if !ok || adm.Room != "chan1" || adm.Identity != rtc.Identity(u.ID, "cast-ab12cd34") ||
		adm.Meta.UID != u.ID || adm.Meta.Kind != "ingest" || adm.Meta.Tag != "cast-ab12cd34" {
		t.Fatalf("设备票推流判定不符: code=%d ok=%v adm=%+v", code, ok, adm)
	}
	// 频道 id 寻址同样认（新地址一律给 id）
	if _, _, ok := admit("1", got.Ticket); !ok {
		t.Fatal("按频道 id 的地址应同样认票")
	}
	// 篡改：改一个字节即验签失败
	flip := "A"
	if got.Ticket[len(got.Ticket)-1] == 'A' {
		flip = "B"
	}
	bad := got.Ticket[:len(got.Ticket)-1] + flip
	if code, _, ok := admit("chan1", bad); ok || code != 403 {
		t.Fatalf("篡改的票应 403，实际 %d ok=%v", code, ok)
	}
	// 频道不符：chan1 的票推 chan2
	if code, _, ok := admit("chan2", got.Ticket); ok || code != 403 {
		t.Fatalf("频道不符应 403，实际 %d ok=%v", code, ok)
	}
	// 过期：用同一把密钥现签一张已过期的票
	secret, err := a.castSecret(ctx)
	if err != nil {
		t.Fatalf("取密钥失败: %v", err)
	}
	expired, err := signCastTicket(secret, castTicket{UID: u.ID, ChannelID: c1.ID,
		Tag: "cast-ab12cd34", Exp: time.Now().Add(-time.Second).Unix()})
	if err != nil {
		t.Fatalf("签票失败: %v", err)
	}
	if code, _, ok := admit("chan1", expired); ok || code != 403 {
		t.Fatalf("过期的票应 403，实际 %d ok=%v", code, ok)
	}
	// 别的密钥签的票（换密钥/换实例）同样不认
	other, _ := signCastTicket("deadbeef", castTicket{UID: u.ID, ChannelID: c1.ID,
		Tag: "cast-ab12cd34", Exp: time.Now().Add(time.Minute).Unix()})
	if code, _, ok := admit("chan1", other); ok || code != 403 {
		t.Fatalf("外来密钥的票应 403，实际 %d ok=%v", code, ok)
	}

	// 撤权立即生效：禁言后既不给票，在手的票也兑换不了
	if err := a.st.Gag(ctx, c1.ID, u.ID); err != nil {
		t.Fatalf("禁言失败: %v", err)
	}
	if code, _ := issue("chan1", sess, map[string]string{"device_id": "ab12cd34"}); code != 403 {
		t.Fatalf("禁言后应拒发票，实际 %d", code)
	}
	if code, _, ok := admit("chan1", got.Ticket); ok || code != 403 {
		t.Fatalf("禁言后在手的票也应 403，实际 %d ok=%v", code, ok)
	}
	a.st.Ungag(ctx, c1.ID, u.ID)
	// 封禁同理
	if err := a.st.Ban(ctx, c1.ID, u.ID); err != nil {
		t.Fatalf("封禁失败: %v", err)
	}
	if code, _ := issue("chan1", sess, map[string]string{"device_id": "ab12cd34"}); code != 403 {
		t.Fatalf("封禁后应拒发票，实际 %d", code)
	}
	if code, _, ok := admit("chan1", got.Ticket); ok || code != 403 {
		t.Fatalf("封禁后在手的票也应 403，实际 %d ok=%v", code, ok)
	}
	a.st.Unban(ctx, c1.ID, u.ID)
}

// 密钥首次使用时生成一把落库，之后稳定不变（重签的票仍能验通过）。
func TestCastSecretPersists(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	if v, err := a.st.GetSetting(ctx, "cfg_cast_secret"); err == nil && v != "" {
		t.Fatalf("首次使用前不应有密钥: %q", v)
	}
	s1, err := a.castSecret(ctx)
	if err != nil || len(s1) != 64 {
		t.Fatalf("应生成 32 字节 hex 密钥: %q %v", s1, err)
	}
	s2, err := a.castSecret(ctx)
	if err != nil || s2 != s1 {
		t.Fatalf("密钥应稳定: %q → %q %v", s1, s2, err)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_cast_secret"); v != s1 {
		t.Fatalf("密钥应落库: %q", v)
	}
}
