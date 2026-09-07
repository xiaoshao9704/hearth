// 审计日志：管制动作发生过的事实记录。
// 只写不改：审计是旁路记录，不参与任何权限判定（权威仍在 channel_gags/channel_bans 等表），
// 因此写失败不得让业务动作回滚，由调用方打日志放过（见 api/audit.go）。
package store

import (
	"context"
	"time"
)

// 审计动作名：审计表里的 action 列取值，前端筛选下拉与后端写入共用这一份。
const (
	AuditMute         = "mute"           // 禁言
	AuditUnmute       = "unmute"         // 解禁
	AuditKick         = "kick"           // 踢出现场
	AuditBan          = "ban"            // 频道封禁
	AuditUnban        = "unban"          // 解除封禁
	AuditChannelRole  = "channel_role"   // 频道角色变更（授予/收回管理员、转让频道）
	AuditMessageDel   = "message_delete" // 删他人消息
	AuditChannelClear = "channel_clear"  // 清空频道聊天记录
	AuditGuestClaim   = "guest_claim"    // 访客转正为注册账号（user_id 不变）
)

// AuditActions 全部合法动作名（管理后台筛选下拉的取值来源）。
var AuditActions = []string{
	AuditMute, AuditUnmute, AuditKick, AuditBan, AuditUnban,
	AuditChannelRole, AuditMessageDel, AuditChannelClear, AuditGuestClaim,
}

// AuditRecord 一条待写入的审计记录。TargetUID/ChannelID 为 0 表示不适用（落库为 NULL）。
type AuditRecord struct {
	ActorUID  int64
	Action    string
	TargetUID int64
	ChannelID int64
	Detail    string
}

// AuditEntry 读出的审计记录，附带用户名/频道名（JOIN 填充，纯展示）。
type AuditEntry struct {
	ID          int64     `json:"id"`
	At          time.Time `json:"at"`
	ActorUID    int64     `json:"actor_uid"`
	ActorName   string    `json:"actor_name"`
	Action      string    `json:"action"`
	TargetUID   int64     `json:"target_uid"`   // 0 = 不适用
	TargetName  string    `json:"target_name"`  // 目标账号已删除时为空
	ChannelID   int64     `json:"channel_id"`   // 0 = 不属于某频道
	ChannelName string    `json:"channel_name"` // 频道已删除时为空
	Detail      string    `json:"detail"`
}

// AuditFilter 审计查询条件；零值都表示不过滤。
// AfterID 是往更早翻页的游标（列表按 id 倒序，游标语义是"比这一条更早"）。
type AuditFilter struct {
	ChannelID int64
	ActorUID  int64
	Action    string
	AfterID   int64
	Limit     int
}

// Audit 写一条审计记录。
func (s *Store) Audit(ctx context.Context, rec AuditRecord) error {
	row := &auditRow{
		At: time.Now(), ActorUID: rec.ActorUID, Action: rec.Action, Detail: rec.Detail,
	}
	if rec.TargetUID > 0 {
		row.TargetUID = &rec.TargetUID
	}
	if rec.ChannelID > 0 {
		row.ChannelID = &rec.ChannelID
	}
	_, err := s.bun.NewInsert().Model(row).Exec(ctx)
	return err
}

// ListAudit 按条件读审计记录，id 倒序（最新在前）。
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	q := s.bun.NewSelect().
		ColumnExpr("a.id, a.at, a.actor_uid, COALESCE(au.username, ''), a.action").
		ColumnExpr("COALESCE(a.target_uid, 0), COALESCE(tu.username, '')").
		ColumnExpr("COALESCE(a.channel_id, 0), COALESCE(c.name, ''), COALESCE(a.detail, '')").
		TableExpr("audit_log AS a").
		Join("LEFT JOIN users au ON au.id = a.actor_uid").
		Join("LEFT JOIN users tu ON tu.id = a.target_uid").
		Join("LEFT JOIN channels c ON c.id = a.channel_id").
		OrderExpr("a.id DESC").
		Limit(f.Limit)
	if f.ChannelID > 0 {
		q = q.Where("a.channel_id = ?", f.ChannelID)
	}
	if f.ActorUID > 0 {
		q = q.Where("a.actor_uid = ?", f.ActorUID)
	}
	if f.Action != "" {
		q = q.Where("a.action = ?", f.Action)
	}
	if f.AfterID > 0 {
		q = q.Where("a.id < ?", f.AfterID)
	}
	rows, err := q.Rows(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.ActorUID, &e.ActorName, &e.Action,
			&e.TargetUID, &e.TargetName, &e.ChannelID, &e.ChannelName, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeAudit 删除 before 之前的审计记录，返回删除条数。
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.bun.NewRaw("DELETE FROM audit_log WHERE at < ?", before).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
