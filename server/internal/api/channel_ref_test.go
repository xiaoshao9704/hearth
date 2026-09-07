// 频道寻址：路径段/请求体的 id 优先、名字兜底（channelByRef），及新建频道禁纯数字名。
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestChannelByRefResolution channelByRef 三种情形：id 命中、名字命中、纯数字名兜底。
func TestChannelByRefResolution(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatal(err)
	}
	named, err := a.st.CreateChannel(ctx, "general", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 纯数字名的历史频道：绕过 createChannel 的校验直接落库（新建入口已禁止，见下一个用例）
	numeric, err := a.st.CreateChannel(ctx, "123", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if numeric.ID == 123 {
		t.Fatalf("测试前提失效：名为 123 的频道恰好 id=123，兜底分支无法覆盖")
	}

	if c, err := a.channelByRef(ctx, strconv.FormatInt(named.ID, 10)); err != nil || c.ID != named.ID {
		t.Fatalf("按 id 应命中 general: %+v err=%v", c, err)
	}
	if c, err := a.channelByRef(ctx, "general"); err != nil || c.ID != named.ID {
		t.Fatalf("按名字应命中 general: %+v err=%v", c, err)
	}
	// 没有 id=123 的频道 → 落到名字兜底
	if c, err := a.channelByRef(ctx, "123"); err != nil || c.ID != numeric.ID {
		t.Fatalf("纯数字名应由名字兜底命中: %+v err=%v", c, err)
	}
	if _, err := a.channelByRef(ctx, "nosuch"); err == nil {
		t.Fatal("不存在的频道应报错")
	}
}

// TestChannelPathAcceptsIDAndName 24 条 /api/channels/{channel}/* 共用的 channelOf：
// id 与名字都通，纯数字名靠兜底继续工作，不存在 404。
func TestChannelPathAcceptsIDAndName(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.st.CreateChannel(ctx, "general", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	numeric, err := a.st.CreateChannel(ctx, "123", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if numeric.ID == 123 {
		t.Fatalf("测试前提失效：名为 123 的频道恰好 id=123")
	}
	token, _ := a.st.CreateSession(ctx, u.ID)
	r := a.Router()

	for _, ref := range []string{"general", strconv.FormatInt(c.ID, 10), "123"} {
		if rec := doReq(t, r, http.MethodGet, "/api/channels/"+ref+"/messages", token, nil); rec.Code != http.StatusOK {
			t.Fatalf("频道引用 %q 应 200，实际 %d: %s", ref, rec.Code, rec.Body.String())
		}
	}
	if rec := doReq(t, r, http.MethodGet, "/api/channels/nosuch/messages", token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的频道应 404，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, http.MethodGet, "/api/channels/99999/messages", token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的频道 id 应 404，实际 %d", rec.Code)
	}
}

// TestJoinTokenChannelID 进房令牌：channel_id 优先于 channel（两者指向不同频道时按 id 走）；
// 两者都空 400。
func TestJoinTokenChannelID(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.CreateChannel(ctx, "general", u.ID); err != nil {
		t.Fatal(err)
	}
	other, err := a.st.CreateChannel(ctx, "other", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 内建实例的密钥平时首启生成，测试里直接给一对，令牌才签得出来
	if _, _, err := a.ensureEmbedKeys(ctx); err != nil {
		t.Fatal(err)
	}
	a.reloadProviders(ctx)
	token, _ := a.st.CreateSession(ctx, u.ID)
	r := a.Router()

	// 名字不存在但 id 存在 → 200，说明按 id 解析
	if rec := doReq(t, r, http.MethodPost, "/api/token", token,
		map[string]any{"channel": "nosuch", "channel_id": other.ID}); rec.Code != http.StatusOK {
		t.Fatalf("channel_id 应优先于 channel（名字不存在也该 200），实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 反向：名字存在但 id 不存在 → 404，说明没有回落到名字
	if rec := doReq(t, r, http.MethodPost, "/api/token", token,
		map[string]any{"channel": "general", "channel_id": int64(99999)}); rec.Code != http.StatusNotFound {
		t.Fatalf("channel_id 不存在时不应回落到名字，应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 只给名字仍照旧
	if rec := doReq(t, r, http.MethodPost, "/api/token", token,
		map[string]any{"channel": "general"}); rec.Code != http.StatusOK {
		t.Fatalf("只给频道名应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	if rec := doReq(t, r, http.MethodPost, "/api/token", token, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少频道应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, "/api/token", token,
		map[string]any{"channel_id": int64(99999)}); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的 channel_id 应 404，实际 %d", rec.Code)
	}
}

// TestCreateChannelRejectsNumericName 新建频道禁纯数字名（与路径段 id 优先的歧义）。
func TestCreateChannelRejectsNumericName(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", "x")
	if err != nil {
		t.Fatal(err)
	}
	token, _ := a.st.CreateSession(ctx, u.ID)
	r := a.Router()

	rec := doReq(t, r, http.MethodPost, "/api/channels", token, map[string]any{"name": "123"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "纯数字") {
		t.Fatalf("纯数字名应 400 并说明原因，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, "/api/channels", token,
		map[string]any{"name": "chan1"}); rec.Code != http.StatusCreated {
		t.Fatalf("正常频道名应 201，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestWHIPPathAcceptsChannelID WHIP 路径段用频道 id：过门禁并走到令牌反查（非 404）。
func TestWHIPPathAcceptsChannelID(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	_, it := seedIngestUser(t, a, "alice", "chan1")
	c, err := a.st.ChannelByName(ctx, "chan1")
	if err != nil {
		t.Fatal(err)
	}
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST",
		"/providers/"+AliasLkembed+"/w/"+strconv.FormatInt(c.ID, 10)+"/"+it.Token, strings.NewReader("sdp")))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("id 形态的 WHIP 地址不应 404（上游未起会 502）: %d %s", rec.Code, rec.Body.String())
	}
	// 频道 id 不存在仍 404
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST",
		"/providers/"+AliasLkembed+"/w/99999/"+it.Token, strings.NewReader("sdp")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的频道 id 应 404，实际 %d", rec.Code)
	}
}
