package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"hearth/server/internal/store"
)

// auditFixture 造一个频道主（首个账号即 super）与一个普通成员，返回两人的 token。
func auditFixture(t *testing.T) (a *API, ch string, ownerTok, memberTok string, owner, member *store.User) {
	t.Helper()
	a = testAPI(t)
	ctx := context.Background()
	var err error
	if owner, err = a.st.CreateUser(ctx, "owner", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.st.CreateChannel(ctx, "general", owner.ID); err != nil {
		t.Fatal(err)
	}
	if member, err = a.st.CreateUser(ctx, "member", "x"); err != nil {
		t.Fatal(err)
	}
	if ownerTok, err = a.st.CreateSessionWithUA(ctx, owner.ID, "owner-device/1.0"); err != nil {
		t.Fatal(err)
	}
	if memberTok, err = a.st.CreateSessionWithUA(ctx, member.ID, "member-device/1.0"); err != nil {
		t.Fatal(err)
	}
	return a, "general", ownerTok, memberTok, owner, member
}

func decodeAudit(t *testing.T, body []byte) ([]store.AuditEntry, int64) {
	t.Helper()
	var resp struct {
		Entries []store.AuditEntry `json:"entries"`
		Next    int64              `json:"next"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析审计列表失败: %v (%s)", err, body)
	}
	return resp.Entries, resp.Next
}

func TestAuditRecordsModeration(t *testing.T) {
	a, ch, ownerTok, _, owner, member := auditFixture(t)
	r := a.Router()
	base := "/api/channels/" + ch
	// 踢出（AuditKick）不在此列：evict 要内核在线，测试环境里 kick 一律 502，走不到审计那一行

	uid := map[string]any{"user_id": member.ID}
	if rec := doReq(t, r, http.MethodPost, base+"/mute", ownerTok, uid); rec.Code != http.StatusOK {
		t.Fatalf("禁言状态码=%d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, base+"/unmute", ownerTok, uid); rec.Code != http.StatusOK {
		t.Fatalf("解禁状态码=%d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, base+"/ban", ownerTok, uid); rec.Code != http.StatusOK {
		t.Fatalf("封禁状态码=%d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, base+"/unban", ownerTok, uid); rec.Code != http.StatusNoContent {
		t.Fatalf("解封状态码=%d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, r, http.MethodPost, base+"/moderators", ownerTok, uid); rec.Code != http.StatusNoContent {
		t.Fatalf("授予频道管理员状态码=%d: %s", rec.Code, rec.Body.String())
	}
	entries, _ := decodeAudit(t, doReq(t, r, http.MethodGet, "/api/admin/audit", ownerTok, nil).Body.Bytes())
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
		if e.ActorUID != owner.ID || e.ActorName != "owner" {
			t.Fatalf("操作者应是 owner: %+v", e)
		}
		if e.TargetUID != member.ID || e.TargetName != "member" {
			t.Fatalf("目标应是 member: %+v", e)
		}
		if e.ChannelName != ch {
			t.Fatalf("频道名未填充: %+v", e)
		}
	}
	want := []string{
		store.AuditChannelRole, store.AuditUnban, store.AuditBan, store.AuditUnmute, store.AuditMute,
	}
	if len(actions) != len(want) {
		t.Fatalf("审计条目=%v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] { // 倒序：最新在前
			t.Fatalf("审计条目=%v, want %v", actions, want)
		}
	}
}

func TestAuditFilterAndCursor(t *testing.T) {
	a, ch, ownerTok, _, owner, member := auditFixture(t)
	ctx := context.Background()
	other, err := a.st.CreateChannel(ctx, "other", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	general, err := a.st.ChannelByName(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	// 直接写库造数：三条 general 的禁言 + 一条 other 的踢出
	for i := 0; i < 3; i++ {
		a.audit(ctx, owner.ID, store.AuditMute, member.ID, general.ID, "")
	}
	a.audit(ctx, member.ID, store.AuditKick, owner.ID, other.ID, "")

	r := a.Router()
	entries, _ := decodeAudit(t, doReq(t, r, http.MethodGet,
		"/api/admin/audit?channel="+strconv.FormatInt(other.ID, 10), ownerTok, nil).Body.Bytes())
	if len(entries) != 1 || entries[0].Action != store.AuditKick {
		t.Fatalf("按频道筛选不符: %+v", entries)
	}
	entries, _ = decodeAudit(t, doReq(t, r, http.MethodGet,
		"/api/admin/audit?actor="+strconv.FormatInt(member.ID, 10), ownerTok, nil).Body.Bytes())
	if len(entries) != 1 || entries[0].ActorUID != member.ID {
		t.Fatalf("按操作者筛选不符: %+v", entries)
	}
	entries, _ = decodeAudit(t, doReq(t, r, http.MethodGet,
		"/api/admin/audit?action="+store.AuditMute, ownerTok, nil).Body.Bytes())
	if len(entries) != 3 {
		t.Fatalf("按动作筛选应有 3 条: %+v", entries)
	}
	if rec := doReq(t, r, http.MethodGet, "/api/admin/audit?action=nope", ownerTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知动作应 400，实际 %d", rec.Code)
	}

	// 游标翻页：limit=2 → next 指向本页最后一条，再取一页应是更早的记录
	first, next := decodeAudit(t, doReq(t, r, http.MethodGet, "/api/admin/audit?limit=2", ownerTok, nil).Body.Bytes())
	if len(first) != 2 || next != first[1].ID {
		t.Fatalf("首页/游标不符: %+v next=%d", first, next)
	}
	second, _ := decodeAudit(t, doReq(t, r, http.MethodGet,
		"/api/admin/audit?limit=2&after="+strconv.FormatInt(next, 10), ownerTok, nil).Body.Bytes())
	if len(second) != 2 || second[0].ID >= next {
		t.Fatalf("第二页应是更早的记录: %+v", second)
	}
}

func TestAuditRequiresAdmin(t *testing.T) {
	a, _, _, memberTok, _, _ := auditFixture(t)
	if rec := doReq(t, a.Router(), http.MethodGet, "/api/admin/audit", memberTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户查审计应 403，实际 %d", rec.Code)
	}
}

func TestAuditRetentionPurge(t *testing.T) {
	a, ch, ownerTok, _, owner, member := auditFixture(t)
	ctx := context.Background()
	general, err := a.st.ChannelByName(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	a.audit(ctx, owner.ID, store.AuditMute, member.ID, general.ID, "")
	a.audit(ctx, owner.ID, store.AuditKick, member.ID, general.ID, "")
	list := func() []store.AuditEntry {
		entries, _ := decodeAudit(t, doReq(t, a.Router(), http.MethodGet, "/api/admin/audit", ownerTok, nil).Body.Bytes())
		return entries
	}
	if len(list()) != 2 {
		t.Fatalf("应有两条审计: %+v", list())
	}

	// 默认保留天数（180）与显式 7 天下，刚写的记录都不该被清
	a.purgeAudit(ctx)
	if err := a.st.SetSetting(ctx, "cfg_audit_retention_days", "7"); err != nil {
		t.Fatal(err)
	}
	a.purgeAudit(ctx)
	if len(list()) != 2 {
		t.Fatalf("未超期的记录不该被清: %+v", list())
	}

	// 0 = 永久保留：即使把边界推到未来也不动（配置分支优先于 SQL 边界）
	if err := a.st.SetSetting(ctx, "cfg_audit_retention_days", "0"); err != nil {
		t.Fatal(err)
	}
	a.purgeAudit(ctx)
	if len(list()) != 2 {
		t.Fatalf("保留天数 0 时不该删记录: %+v", list())
	}

	// 边界本身生效：把边界放到将来即全清（按时间删的 SQL 由 store 层测试覆盖更细）
	if n, err := a.st.PurgeAudit(ctx, time.Now().Add(time.Minute)); err != nil || n != 2 {
		t.Fatalf("PurgeAudit 应删 2 条: n=%d err=%v", n, err)
	}
	if len(list()) != 0 {
		t.Fatalf("清理后应为空: %+v", list())
	}
}
