package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"hearth/server/internal/store"
)

// chatFixture 造一个频道与一个普通成员，返回 API、频道名与成员 token。
func chatFixture(t *testing.T) (*API, string, string) {
	t.Helper()
	a := testAPI(t)
	ctx := context.Background()
	owner, err := a.st.CreateUser(ctx, "owner", "x") // 首个账号是 super
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.CreateChannel(ctx, "general", owner.ID); err != nil {
		t.Fatal(err)
	}
	member, err := a.st.CreateUser(ctx, "member", "x")
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.st.CreateSession(ctx, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, "general", token
}

func decodeMessage(t *testing.T, body []byte) store.Message {
	t.Helper()
	var m store.Message
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("解析消息失败: %v (%s)", err, body)
	}
	return m
}

func TestPostAndListMessages(t *testing.T) {
	a, ch, token := chatFixture(t)
	r := a.Router()
	path := "/api/channels/" + ch + "/messages"

	if rec := doReq(t, r, http.MethodPost, path, "", map[string]any{"content": "hi"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权状态码=%d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if rec := doReq(t, r, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权读历史状态码=%d, want %d", rec.Code, http.StatusUnauthorized)
	}

	rec := doReq(t, r, http.MethodPost, path, token, map[string]any{"content": "  hi  "})
	if rec.Code != http.StatusCreated {
		t.Fatalf("发文本状态码=%d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	first := decodeMessage(t, rec.Body.Bytes())
	if first.ID == 0 || first.Kind != store.KindText || first.Content != "hi" ||
		first.Username != "member" || first.File != nil {
		t.Fatalf("落库消息不符: %+v", first)
	}

	// 空白内容与超长文本都拒
	if rec := doReq(t, r, http.MethodPost, path, token, map[string]any{"content": "   "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("空消息状态码=%d, want %d", rec.Code, http.StatusBadRequest)
	}
	if rec := doReq(t, r, http.MethodPost, path, token, map[string]any{
		"content": strings.Repeat("字", messageMaxRunes+1),
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("超长消息状态码=%d, want %d", rec.Code, http.StatusBadRequest)
	}

	rec = doReq(t, r, http.MethodPost, path, token, map[string]any{"content": "second"})
	second := decodeMessage(t, rec.Body.Bytes())

	// after 缺省回全部；after=首条 id 只回更新的
	rec = doReq(t, r, http.MethodGet, path, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读历史状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var all []store.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("解析历史失败: %v", err)
	}
	if len(all) != 2 || all[0].ID != first.ID || all[1].ID != second.ID {
		t.Fatalf("历史应按时间正序回两条: %+v", all)
	}
	rec = doReq(t, r, http.MethodGet, path+"?after="+strconv.FormatInt(first.ID, 10), token, nil)
	var after []store.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("解析补齐失败: %v", err)
	}
	if len(after) != 1 || after[0].ID != second.ID {
		t.Fatalf("after= 只应回更新的消息: %+v", after)
	}
}

// 禁言两层各自独立成立：这里是 HTTP 侧（内核侧是进房票的 CanPublishData）。
func TestPostMessageGaggedForbidden(t *testing.T) {
	a, ch, token := chatFixture(t)
	r := a.Router()
	ctx := context.Background()
	u, err := a.st.UserByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.st.ChannelByName(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.Gag(ctx, c.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/channels/" + ch + "/messages"
	if rec := doReq(t, r, http.MethodPost, path, token, map[string]any{"content": "hi"}); rec.Code != http.StatusForbidden {
		t.Fatalf("禁言发言状态码=%d, want %d", rec.Code, http.StatusForbidden)
	}
	// 禁言只禁发言，历史照读
	if rec := doReq(t, r, http.MethodGet, path, token, nil); rec.Code != http.StatusOK {
		t.Fatalf("禁言读历史状态码=%d, want %d", rec.Code, http.StatusOK)
	}
}

// 邀请制频道的非白名单用户读写都 403（与进房同一套 admitUser 判定）。
func TestChatMessagesNonMemberForbidden(t *testing.T) {
	a, ch, token := chatFixture(t)
	r := a.Router()
	ctx := context.Background()
	c, err := a.st.ChannelByName(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetInviteOnly(ctx, c.ID, true); err != nil {
		t.Fatal(err)
	}
	path := "/api/channels/" + ch + "/messages"
	if rec := doReq(t, r, http.MethodGet, path, token, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("非成员读历史状态码=%d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := doReq(t, r, http.MethodPost, path, token, map[string]any{"content": "hi"}); rec.Code != http.StatusForbidden {
		t.Fatalf("非成员发言状态码=%d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestPostFileMessage(t *testing.T) {
	a, ch, token := chatFixture(t)
	r := a.Router()
	path := "/api/channels/" + ch + "/messages"

	rec := doReq(t, r, http.MethodPost, path, token, map[string]any{
		"kind": "file",
		"file": map[string]any{"name": "shot.png", "mime": "image/png", "size": 12345},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("发文件卡片状态码=%d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	m := decodeMessage(t, rec.Body.Bytes())
	if m.ID == 0 || m.Kind != store.KindFile || m.File == nil ||
		m.File.Name != "shot.png" || m.File.Mime != "image/png" || m.File.Size != 12345 {
		t.Fatalf("文件卡片不符: %+v", m)
	}

	// 超过 chat_file_max_mb（默认 25MB）→ 413
	if rec := doReq(t, r, http.MethodPost, path, token, map[string]any{
		"kind": "file",
		"file": map[string]any{"name": "big.bin", "mime": "application/octet-stream", "size": 26 << 20},
	}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限文件状态码=%d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	// 上限可配：调小后原本合法的大小也超限
	if err := a.st.SetSetting(context.Background(), "cfg_chat_file_max_mb", "1"); err != nil {
		t.Fatal(err)
	}
	if rec := doReq(t, r, http.MethodPost, path, token, map[string]any{
		"kind": "file",
		"file": map[string]any{"name": "mid.bin", "mime": "application/octet-stream", "size": 2 << 20},
	}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("改小上限后状态码=%d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}

	for _, bad := range []map[string]any{
		{"kind": "file"},
		{"kind": "file", "file": map[string]any{"name": "  ", "mime": "image/png", "size": 1}},
		{"kind": "file", "file": map[string]any{"name": "x.png", "mime": "image/png; charset=x", "size": 1}},
		{"kind": "file", "file": map[string]any{"name": "x.png", "mime": "image/png", "size": 0}},
		{"kind": "sticker", "content": "hi"},
	} {
		if rec := doReq(t, r, http.MethodPost, path, token, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("非法文件消息 %+v 状态码=%d, want %d", bad, rec.Code, http.StatusBadRequest)
		}
	}
}

// 数据线选择：默认 auto，管理后台改了立刻生效（joinToken 把它原样放进 data_line 下发）。
func TestChatDataLineSetting(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	if got := a.dynVal(ctx, "chat_data_line"); got != "auto" {
		t.Fatalf("默认数据线=%q, want auto", got)
	}
	if err := a.st.SetSetting(ctx, "cfg_chat_data_line", "voice"); err != nil {
		t.Fatal(err)
	}
	if got := a.dynVal(ctx, "chat_data_line"); got != "voice" {
		t.Fatalf("改配置后数据线=%q, want voice", got)
	}
	if got := a.dynVal(ctx, "chat_file_max_mb"); got != "25" {
		t.Fatalf("默认文件上限=%q, want 25", got)
	}
}
