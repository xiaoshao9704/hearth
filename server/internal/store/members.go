package store

import (
	"context"
)

// ---- 成员（邀请制白名单）----

// IsMember 用户是否在该频道白名单内。
func (s *Store) IsMember(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.bun.NewRaw(
		"SELECT COUNT(1) FROM channel_members WHERE channel_id = ? AND user_id = ?", channelID, userID).Scan(ctx, &n)
	return n > 0, err
}

// MembershipsOf 该用户在的全部频道 id 集合（大厅列表批量填充可见性用，避免逐频道查询）。
func (s *Store) MembershipsOf(ctx context.Context, userID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	rows, err := s.bun.QueryContext(ctx,
		"SELECT channel_id FROM channel_members WHERE user_id = ?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			return nil, err
		}
		out[cid] = true
	}
	return out, rows.Err()
}

func (s *Store) AddMember(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewInsert().Model(&channelMemberRow{ChannelID: channelID, UserID: userID}).Ignore().Exec(ctx)
	return err
}

// RemoveMember 只移除普通 member；owner/moderator 必须走专用的降级或转让路径。
// 返回值表示是否真正删除了一行，让 handler 不会对空操作误报成功。
func (s *Store) RemoveMember(ctx context.Context, channelID, userID int64) (bool, error) {
	result, err := s.bun.NewRaw(
		"DELETE FROM channel_members WHERE channel_id = ? AND user_id = ? AND role = 'member'", channelID, userID).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// ListMembers 返回该频道全部成员行（owner/moderator/member 都带角色）。
func (s *Store) ListMembers(ctx context.Context, channelID int64) ([]UserRef, error) {
	out := []UserRef{}
	rows, err := s.bun.QueryContext(ctx, `
SELECT u.id, u.username, t.role FROM channel_members t JOIN users u ON u.id = t.user_id
WHERE t.channel_id = ? ORDER BY t.created_at`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref UserRef
		if err := rows.Scan(&ref.ID, &ref.Username, &ref.Role); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// UserRef 名单条目：id 是操作目标（解禁/移出白名单按它发），username 只用于展示；
// Role 仅 ListMembers 填充（channel_members 的角色列），其余名单为空。
type UserRef struct {
	ID       int64       `json:"id"`
	Username string      `json:"username"`
	Role     ChannelRole `json:"role,omitempty"`
}

// listUserRefs 取 channel_bans/channel_gags/channel_members 的名单（三表结构相同）。
func (s *Store) listUserRefs(ctx context.Context, table string, channelID int64) ([]UserRef, error) {
	out := []UserRef{}
	rows, err := s.bun.QueryContext(ctx, `
SELECT u.id, u.username FROM `+table+` t JOIN users u ON u.id = t.user_id
WHERE t.channel_id = ? ORDER BY t.created_at`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref UserRef
		if err := rows.Scan(&ref.ID, &ref.Username); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// CanJoin 进入权限：被封禁拒绝；邀请制频道只认 channel_members 成员行。
// owner/moderator 本身也是成员行，不再用 channels.created_by 历史列兜底。
func (s *Store) CanJoin(ctx context.Context, c *Channel, userID int64) (bool, string, error) {
	banned, err := s.IsBanned(ctx, c.ID, userID)
	if err != nil {
		return false, "", err
	}
	if banned {
		return false, "已被该频道封禁", nil
	}
	if c.InviteOnly {
		member, err := s.IsMember(ctx, c.ID, userID)
		if err != nil {
			return false, "", err
		}
		if !member {
			return false, "该频道为邀请制，未在白名单内", nil
		}
	}
	return true, "", nil
}
