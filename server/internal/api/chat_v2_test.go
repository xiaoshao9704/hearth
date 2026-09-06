// 聊天第二版数据模型的接口测试：删除权限矩阵、reply_to 校验、反应去重、
// 保留策略清理、历史聚合形状。
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"context"

	"hearth/server/internal/store"
)

// chatEnv 一个频道 + 四种身份（频道主 / 普通成员 / 频道管理员 / 另一个普通成员）。
// 带 dbPath 是为了保留策略测试要把消息时间推回过去——接口不提供改时间的入口。
type chatEnv struct {
	a                         *API
	ch                        string
	ownerUID                  int64
	owner, member, mod, other string // 会话 token
	dbPath                    string
}

func newChatEnv(t *testing.T) chatEnv {
	t.Helper()
	a, dbPath := testAPIWithDB(t)
	ctx := context.Background()
	owner, err := a.st.CreateUser(ctx, "owner", "x") // 首个账号是 super
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.st.CreateChannel(ctx, "general", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	env := chatEnv{a: a, ch: c.Name, ownerUID: owner.ID, dbPath: dbPath}
	env.owner = newSession(t, a, owner.ID)
	for _, spec := range []struct {
		name string
		role store.ChannelRole
		dst  *string
	}{
		{"member", store.ChannelRoleMember, &env.member},
		{"mod", store.ChannelRoleModerator, &env.mod},
		{"other", store.ChannelRoleMember, &env.other},
	} {
		u, err := a.st.CreateUser(ctx, spec.name, "x")
		if err != nil {
			t.Fatal(err)
		}
		if err := a.st.SetChannelRole(ctx, c.ID, u.ID, spec.role); err != nil {
			t.Fatal(err)
		}
		*spec.dst = newSession(t, a, u.ID)
	}
	return env
}

func newSession(t *testing.T, a *API, uid int64) string {
	t.Helper()
	tok, err := a.st.CreateSession(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e chatEnv) msgPath() string { return "/api/channels/" + e.ch + "/messages" }

func (e chatEnv) idPath(id int64) string { return e.msgPath() + "/" + strconv.FormatInt(id, 10) }

func (e chatEnv) reactionPath(id int64, emoji string) string {
	return e.idPath(id) + "/reactions/" + url.PathEscape(emoji)
}

func (e chatEnv) postText(t *testing.T, token, content string) store.Message {
	t.Helper()
	rec := doReq(t, e.a.Router(), http.MethodPost, e.msgPath(), token, map[string]any{"content": content})
	if rec.Code != http.StatusCreated {
		t.Fatalf("发消息状态码=%d: %s", rec.Code, rec.Body.String())
	}
	return decodeMessage(t, rec.Body.Bytes())
}

func (e chatEnv) list(t *testing.T, token string) []store.Message {
	t.Helper()
	rec := doReq(t, e.a.Router(), http.MethodGet, e.msgPath(), token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读历史状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var out []store.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析历史失败: %v", err)
	}
	return out
}

// rawDB 另开一条连接直接改库（只用于造"很久以前发的消息"这种接口造不出的状态）。
func (e chatEnv) rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// 删除权限矩阵：作者本人可撤回，频道管理员可删别人的，普通成员删不了别人的。
func TestDeleteMessagePermissionMatrix(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()

	mine := e.postText(t, e.member, "我自己发的")
	path := e.idPath(mine.ID)

	// 旁观的普通成员删别人的：403，消息原样在
	if rec := doReq(t, r, http.MethodDelete, path, e.other, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("普通成员删他人消息状态码=%d, want %d", rec.Code, http.StatusForbidden)
	}
	if list := e.list(t, e.member); len(list) != 1 || list[0].Deleted || list[0].Content != "我自己发的" {
		t.Fatalf("403 后消息不该变: %+v", list)
	}

	// 作者本人撤回：204，历史里留占位（行还在、内容清空、deleted=true）
	if rec := doReq(t, r, http.MethodDelete, path, e.member, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("作者撤回状态码=%d: %s", rec.Code, rec.Body.String())
	}
	list := e.list(t, e.member)
	if len(list) != 1 || !list[0].Deleted || list[0].Content != "" || list[0].ID != mine.ID {
		t.Fatalf("撤回后应留占位: %+v", list)
	}
	// 幂等：再撤一次仍是 204
	if rec := doReq(t, r, http.MethodDelete, path, e.member, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("重复撤回状态码=%d, want %d", rec.Code, http.StatusNoContent)
	}

	// 频道管理员删别人的：204
	victim := e.postText(t, e.other, "别人发的")
	if rec := doReq(t, r, http.MethodDelete, e.idPath(victim.ID), e.mod, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("管理员删他人消息状态码=%d: %s", rec.Code, rec.Body.String())
	}

	// 不存在的 id：404
	if rec := doReq(t, r, http.MethodDelete, e.idPath(99999), e.mod, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在的消息状态码=%d, want %d", rec.Code, http.StatusNotFound)
	}
}

// 文件消息撤回后卡片一并清空：库里不留文件名/类型/大小。
func TestDeleteFileMessageClearsCard(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()
	rec := doReq(t, r, http.MethodPost, e.msgPath(), e.member, map[string]any{
		"kind": "file",
		"file": map[string]any{"name": "shot.png", "mime": "image/png", "size": 1024},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("发文件状态码=%d: %s", rec.Code, rec.Body.String())
	}
	m := decodeMessage(t, rec.Body.Bytes())
	doReq(t, r, http.MethodDelete, e.idPath(m.ID), e.member, nil)
	list := e.list(t, e.member)
	if len(list) != 1 || !list[0].Deleted || list[0].File != nil {
		t.Fatalf("撤回后文件卡片应清空: %+v", list)
	}
}

// reply_to 校验：同频道未删的消息可引用；不存在、已撤回、跨频道一律 400。
func TestPostMessageReplyToValidation(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()
	ctx := context.Background()

	target := e.postText(t, e.member, "被引用的")
	rec := doReq(t, r, http.MethodPost, e.msgPath(), e.member, map[string]any{"content": "回复", "reply_to": target.ID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("带 reply_to 发消息状态码=%d: %s", rec.Code, rec.Body.String())
	}
	reply := decodeMessage(t, rec.Body.Bytes())
	if reply.ReplyTo == nil || *reply.ReplyTo != target.ID {
		t.Fatalf("reply_to 未落库: %+v", reply)
	}

	for _, tc := range []struct {
		name string
		id   any
	}{
		{"不存在的 id", 99999},
		{"零值", 0},
		{"负数", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, r, http.MethodPost, e.msgPath(), e.member, map[string]any{"content": "x", "reply_to": tc.id})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码=%d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}

	// 跨频道：另一个频道里的消息不能被引用（查找按频道限定）
	other, err := e.a.st.CreateChannel(ctx, "other", e.ownerUID)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := e.a.st.AddMessage(ctx, other.ID, e.ownerUID, store.KindText, "别的频道", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, r, http.MethodPost, e.msgPath(), e.member, map[string]any{"content": "x", "reply_to": foreign.ID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("跨频道引用状态码=%d, want %d", rec.Code, http.StatusBadRequest)
	}

	// 已撤回的不能再被引用
	doReq(t, r, http.MethodDelete, e.idPath(target.ID), e.member, nil)
	rec = doReq(t, r, http.MethodPost, e.msgPath(), e.member, map[string]any{"content": "x", "reply_to": target.ID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("引用已撤回消息状态码=%d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func decodeReactions(t *testing.T, body []byte) []store.Reaction {
	t.Helper()
	var resp struct {
		ID        int64            `json:"id"`
		Reactions []store.Reaction `json:"reactions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析反应失败: %v (%s)", err, body)
	}
	return resp.Reactions
}

// 反应去重：同一个人对同一条同一个表情点两次只算一次；不同人各算一次；取消是幂等的。
func TestReactionsDedupeAndToggle(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()
	m := e.postText(t, e.member, "有人来点个赞")
	path := e.reactionPath(m.ID, "👍")

	if rec := doReq(t, r, http.MethodPut, path, e.member, nil); rec.Code != http.StatusOK {
		t.Fatalf("加反应状态码=%d: %s", rec.Code, rec.Body.String())
	}
	// 同一人重复点：仍是一条一人
	rec := doReq(t, r, http.MethodPut, path, e.member, nil)
	list := decodeReactions(t, rec.Body.Bytes())
	if len(list) != 1 || list[0].Emoji != "👍" || len(list[0].UIDs) != 1 {
		t.Fatalf("重复点应去重: %+v", list)
	}

	// 换个人点同一个表情：同一条聚合里两个 uid
	rec = doReq(t, r, http.MethodPut, path, e.mod, nil)
	list = decodeReactions(t, rec.Body.Bytes())
	if len(list) != 1 || len(list[0].UIDs) != 2 {
		t.Fatalf("两人点同一表情应聚合成一条两 uid: %+v", list)
	}

	// 取消自己的：只掉自己那份
	rec = doReq(t, r, http.MethodDelete, path, e.member, nil)
	list = decodeReactions(t, rec.Body.Bytes())
	if len(list) != 1 || len(list[0].UIDs) != 1 {
		t.Fatalf("取消后应只剩一人: %+v", list)
	}
	// 没点过再取消：幂等
	if rec := doReq(t, r, http.MethodDelete, path, e.member, nil); rec.Code != http.StatusOK {
		t.Fatalf("重复取消状态码=%d, want %d", rec.Code, http.StatusOK)
	}

	// 白名单之外的表情：400
	if rec := doReq(t, r, http.MethodPut, e.reactionPath(m.ID, "🍕"), e.member, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("非白名单表情状态码=%d, want %d", rec.Code, http.StatusBadRequest)
	}
	// 不存在的消息：404
	if rec := doReq(t, r, http.MethodPut, e.reactionPath(99999, "👍"), e.member, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("对不存在的消息加反应状态码=%d, want %d", rec.Code, http.StatusNotFound)
	}
	// 已撤回的消息不接受反应
	doReq(t, r, http.MethodDelete, e.idPath(m.ID), e.member, nil)
	if rec := doReq(t, r, http.MethodPut, path, e.mod, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("对已撤回消息加反应状态码=%d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// 历史聚合形状：一次 GET 就带齐 reply_to / deleted / reactions，没反应的是空数组不是 null。
func TestListMessagesAggregateShape(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()

	first := e.postText(t, e.member, "第一条")
	rec := doReq(t, r, http.MethodPost, e.msgPath(), e.mod, map[string]any{"content": "第二条", "reply_to": first.ID})
	second := decodeMessage(t, rec.Body.Bytes())
	doReq(t, r, http.MethodPut, e.reactionPath(first.ID, "👍"), e.member, nil)
	doReq(t, r, http.MethodPut, e.reactionPath(first.ID, "🎉"), e.mod, nil)

	list := e.list(t, e.member)
	if len(list) != 2 {
		t.Fatalf("应有两条: %+v", list)
	}
	if list[0].ReplyTo != nil || len(list[0].Reactions) != 2 {
		t.Fatalf("首条应无引用、带两种反应: %+v", list[0])
	}
	// 两种表情各一人，且顺序稳定（同秒内按表情码点兜底排序，不随查询变化）
	seen := map[string]int{}
	for _, x := range list[0].Reactions {
		seen[x.Emoji] = len(x.UIDs)
	}
	if seen["👍"] != 1 || seen["🎉"] != 1 {
		t.Fatalf("两种反应应各一人: %+v", list[0].Reactions)
	}
	if again := e.list(t, e.member); again[0].Reactions[0].Emoji != list[0].Reactions[0].Emoji {
		t.Fatalf("两次拉取的反应顺序应一致: %+v vs %+v", list[0].Reactions, again[0].Reactions)
	}
	if list[1].ID != second.ID || list[1].ReplyTo == nil || *list[1].ReplyTo != first.ID {
		t.Fatalf("次条应带 reply_to: %+v", list[1])
	}
	// 没有反应的那条必须是 []，不是 null——前端直接按数组渲染
	raw, err := json.Marshal(list[1])
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if string(probe["reactions"]) != "[]" {
		t.Fatalf("无反应时 reactions 应为空数组，实际 %s", probe["reactions"])
	}
	if string(probe["deleted"]) != "false" {
		t.Fatalf("未删时 deleted 应为 false，实际 %s", probe["deleted"])
	}
}

// 保留策略：超期的真删（连同反应），未超期的留着；0 = 永久保留。
func TestChatRetentionPurge(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()
	ctx := context.Background()

	old := e.postText(t, e.member, "很久以前")
	fresh := e.postText(t, e.member, "刚刚")
	doReq(t, r, http.MethodPut, e.reactionPath(old.ID, "👍"), e.member, nil)
	db := e.rawDB(t)
	if _, err := db.ExecContext(ctx, `UPDATE messages SET created_at = ? WHERE id = ?`,
		time.Now().Add(-40*24*time.Hour), old.ID); err != nil {
		t.Fatal(err)
	}

	// 默认 30 天：第一条该被清掉
	if n := e.a.PurgeExpiredMessages(ctx); n != 1 {
		t.Fatalf("应清理 1 条，实际 %d", n)
	}
	list := e.list(t, e.member)
	if len(list) != 1 || list[0].ID != fresh.ID {
		t.Fatalf("清理后应只剩新消息: %+v", list)
	}
	// 反应跟着走：被清消息的反应不该留在表里
	var left int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_reactions WHERE message_id = ?`, old.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("被清消息的反应应一并删除，实际留下 %d 行", left)
	}

	// 0 = 永久保留：再老的也不动
	if err := e.a.st.SetSetting(ctx, "cfg_chat_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET created_at = ?`,
		time.Now().Add(-400*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := e.a.PurgeExpiredMessages(ctx); n != 0 {
		t.Fatalf("保留天数为 0 时不该清理，实际清了 %d 条", n)
	}
	if len(e.list(t, e.member)) != 1 {
		t.Fatal("保留天数为 0 时消息应还在")
	}
}

// 清空频道：只有频道主/系统 admin+ 能清；频道管理员与普通成员都不行，别的频道不受影响。
func TestClearChannelMessages(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()
	ctx := context.Background()

	e.postText(t, e.member, "一")
	e.postText(t, e.member, "二")
	// 另建一个频道并留一条，验证清空只作用于本频道
	other, err := e.a.st.CreateChannel(ctx, "other", e.ownerUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.st.AddMessage(ctx, other.ID, e.ownerUID, store.KindText, "别动我", nil, nil); err != nil {
		t.Fatal(err)
	}

	for _, tok := range []string{e.member, e.mod} {
		if rec := doReq(t, r, http.MethodDelete, e.msgPath(), tok, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("非频道主清空状态码=%d, want %d", rec.Code, http.StatusForbidden)
		}
	}

	rec := doReq(t, r, http.MethodDelete, e.msgPath(), e.owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("频道主清空状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Deleted != 2 {
		t.Fatalf("应清掉 2 条，实际 %d", resp.Deleted)
	}
	if list := e.list(t, e.member); len(list) != 0 {
		t.Fatalf("清空后不该还有消息: %+v", list)
	}
	if left, err := e.a.st.RecentMessages(ctx, other.ID, 10); err != nil || len(left) != 1 {
		t.Fatalf("别的频道不该受影响: %+v (%v)", left, err)
	}
}

// 版主删他人消息落一条审计；作者自己撤回不落（撤回不是管制动作）。
func TestDeleteMessageWritesAudit(t *testing.T) {
	e := newChatEnv(t)
	r := e.a.Router()
	ctx := context.Background()

	mine := e.postText(t, e.member, "我自己发的")
	if rec := doReq(t, r, http.MethodDelete, e.idPath(mine.ID), e.member, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("作者撤回状态码=%d: %s", rec.Code, rec.Body.String())
	}
	entries, err := e.a.st.ListAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("自己撤回不该落审计: %+v", entries)
	}

	victim := e.postText(t, e.other, "别人发的")
	if rec := doReq(t, r, http.MethodDelete, e.idPath(victim.ID), e.mod, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("管理员删他人消息状态码=%d: %s", rec.Code, rec.Body.String())
	}
	entries, err = e.a.st.ListAudit(ctx, store.AuditFilter{Action: store.AuditMessageDel})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].TargetUID != victim.UserID || entries[0].ChannelID == 0 {
		t.Fatalf("版主删他人消息应落一条 message_delete: %+v", entries)
	}
}
