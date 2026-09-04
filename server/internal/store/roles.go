// 角色模型：系统角色（users.role，严格阶梯）与频道角色（channel_members.role，三档）。
// 判定逻辑（谁能操作谁、隐含房主等）在 perm 包收口，这里只放类型、档位数与存取。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"
)

// Role 系统角色：严格阶梯，高档包含低档能力。
type Role string

const (
	RoleGuest Role = "guest" // 访客：设备绑定、有过期时间，只进被授予的频道
	RoleUser  Role = "user"  // 注册用户
	RolePower Role = "power" // 高级用户：可建频道、发注册邀请
	RoleAdmin Role = "admin" // 管理员：管理后台全部能力，任何频道隐含房主
	RoleSuper Role = "super" // 超级管理员：全站恰好一个，只能经 CLI 转移
)

// Rank 阶梯档位，越大越高档；未知值按最低档处理。
func (r Role) Rank() int {
	switch r {
	case RoleUser:
		return 1
	case RolePower:
		return 2
	case RoleAdmin:
		return 3
	case RoleSuper:
		return 4
	default: // guest 与空值
		return 0
	}
}

// ChannelRole 频道角色：channel_members.role 的三档 + 无行。
type ChannelRole string

const (
	ChannelRoleNone      ChannelRole = ""          // 无成员行
	ChannelRoleMember    ChannelRole = "member"    // 邀请制白名单成员 / 访客的频道授予
	ChannelRoleModerator ChannelRole = "moderator" // 频道管理员
	ChannelRoleOwner     ChannelRole = "owner"     // 频道主（每频道恰好一行，归属的权威）
)

// Rank 频道角色档位（member 不参与「管理」比较，仅表示有行）。
func (r ChannelRole) Rank() int {
	switch r {
	case ChannelRoleMember:
		return 1
	case ChannelRoleModerator:
		return 2
	case ChannelRoleOwner:
		return 3
	default:
		return 0
	}
}

// SetUserRole 设置系统角色；is_admin 列不再写入（只读派生，下个版本删列）。
func (s *Store) SetUserRole(ctx context.Context, userID int64, role Role) error {
	_, err := s.bun.NewRaw("UPDATE users SET role = ? WHERE id = ?", string(role), userID).Exec(ctx)
	return err
}

// ChannelRoleOf 读用户在频道的成员角色；无行返回 ChannelRoleNone。
// 系统 admin+ 的隐含 owner 不在此展开（那是 perm.ChannelRole 的职责）。
func (s *Store) ChannelRoleOf(ctx context.Context, channelID, userID int64) (ChannelRole, error) {
	var role string
	err := s.bun.NewRaw(
		"SELECT role FROM channel_members WHERE channel_id = ? AND user_id = ?", channelID, userID).
		Scan(ctx, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelRoleNone, nil
	}
	return ChannelRole(role), err
}

// ChannelRolesOf 读用户在全部频道的成员角色（大厅列表的 my_role 批量填充用）。
func (s *Store) ChannelRolesOf(ctx context.Context, userID int64) (map[int64]ChannelRole, error) {
	out := map[int64]ChannelRole{}
	rows, err := s.bun.QueryContext(ctx,
		"SELECT channel_id, role FROM channel_members WHERE user_id = ?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int64
		var role string
		if err := rows.Scan(&cid, &role); err != nil {
			return nil, err
		}
		out[cid] = ChannelRole(role)
	}
	return out, rows.Err()
}

// SetChannelRole 写入/变更成员角色（upsert：无行建行，有行改档）。
func (s *Store) SetChannelRole(ctx context.Context, channelID, userID int64, role ChannelRole) error {
	row := &channelMemberRow{ChannelID: channelID, UserID: userID, Role: string(role)}
	q := s.bun.NewInsert().Model(row).
		Column("channel_id", "user_id", "role").Value("role", "?", string(role))
	if s.d.name == "mysql" {
		q = q.On("DUPLICATE KEY UPDATE").Set("role = VALUES(role)")
	} else { // sqlite / postgres 同语法
		q = q.On("CONFLICT (channel_id, user_id) DO UPDATE").Set("role = EXCLUDED.role")
	}
	_, err := q.Exec(ctx)
	return err
}

// TransferChannel 频道转让：旧 owner 降为 moderator，新主写 owner 行（其旧行被覆盖升档）。
func (s *Store) TransferChannel(ctx context.Context, channelID, newOwnerID int64) error {
	if _, err := s.bun.NewRaw(
		"UPDATE channel_members SET role = 'moderator' WHERE channel_id = ? AND role = 'owner'",
		channelID).Exec(ctx); err != nil {
		return err
	}
	return s.SetChannelRole(ctx, channelID, newOwnerID, ChannelRoleOwner)
}

// ListModerators 列出频道管理员（不含房主）。
func (s *Store) ListModerators(ctx context.Context, channelID int64) ([]UserRef, error) {
	out := []UserRef{}
	rows, err := s.bun.QueryContext(ctx, `
SELECT u.id, u.username FROM channel_members t JOIN users u ON u.id = t.user_id
WHERE t.channel_id = ? AND t.role = 'moderator' ORDER BY t.created_at`, channelID)
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

// TransferSuper 转移超级管理员（CLI promote）：旧 super 降为 admin，目标升为 super。
func (s *Store) TransferSuper(ctx context.Context, userID int64) error {
	if _, err := s.bun.NewRaw(
		"UPDATE users SET role = 'admin' WHERE role = 'super'").Exec(ctx); err != nil {
		return err
	}
	return s.SetUserRole(ctx, userID, RoleSuper)
}

// MigrateRoleData 角色数据迁移（api 游标 v5 调用，幂等）：
//  1. is_admin=1 → role=admin，其中 id 最小者 → super（同时保证全站有一个 super）；
//  2. 其余用户中拥有任何频道的 → power（不让现有房主失去建频道能力）；
//  3. 每个频道按 created_by 写 channel_members(role=owner) 行（已有行则升档）。
func (s *Store) MigrateRoleData(ctx context.Context) error {
	if _, err := s.bun.NewRaw(
		"UPDATE users SET role = 'admin' WHERE is_admin = 1 AND role = 'user'").Exec(ctx); err != nil {
		return err
	}
	var superID int64
	if err := s.bun.NewRaw(
		"SELECT COALESCE(MIN(id), 0) FROM users WHERE role = 'super'").Scan(ctx, &superID); err != nil {
		return err
	}
	if superID == 0 {
		var adminID int64
		if err := s.bun.NewRaw(
			"SELECT COALESCE(MIN(id), 0) FROM users WHERE role = 'admin'").Scan(ctx, &adminID); err != nil {
			return err
		}
		if adminID != 0 {
			if err := s.SetUserRole(ctx, adminID, RoleSuper); err != nil {
				return err
			}
		}
	}
	if _, err := s.bun.NewRaw(
		"UPDATE users SET role = 'power' WHERE role = 'user' AND id IN (SELECT created_by FROM channels)").
		Exec(ctx); err != nil {
		return err
	}
	rows, err := s.bun.QueryContext(ctx, "SELECT id, created_by FROM channels")
	if err != nil {
		return err
	}
	type ch struct{ id, owner int64 }
	var chs []ch
	for rows.Next() {
		var c ch
		if err := rows.Scan(&c.id, &c.owner); err != nil {
			rows.Close()
			return err
		}
		chs = append(chs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range chs {
		if err := s.SetChannelRole(ctx, c.id, c.owner, ChannelRoleOwner); err != nil {
			return err
		}
	}
	return nil
}

// CountOwnedChannels 用户名下（owner 行）的频道数：降级提示与删除过户用。
func (s *Store) CountOwnedChannels(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.bun.NewRaw(
		"SELECT COUNT(1) FROM channel_members WHERE user_id = ? AND role = 'owner'", userID).Scan(ctx, &n)
	return n, err
}

// scanUserParts 统一 users 读取：role 是权威，is_admin 由 role 派生（双写期只读）。
func scanUserParts(u *User, role string, expiresAt *time.Time) {
	u.Role = Role(role)
	u.IsAdmin = u.Role.Rank() >= RoleAdmin.Rank()
	u.ExpiresAt = expiresAt
}

// ---- 访客 ----

// CreateGuest 创建访客：role=guest、expires_at=now+ttl、invite_id 记来源邀请。
// 展示名即 username（全表唯一，冲突由调用方提示换一个）；访客没有密码。
func (s *Store) CreateGuest(ctx context.Context, username string, inviteID int64, ttl time.Duration) (*User, error) {
	exp := time.Now().Add(ttl)
	row := &userRow{Username: username, PasswordHash: "", Role: string(RoleGuest),
		ExpiresAt: &exp, InviteID: &inviteID}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	u := &User{ID: row.ID, Username: username, InviteID: &inviteID}
	scanUserParts(u, string(RoleGuest), &exp)
	return u, nil
}

// ListExpiredGuests 列出已过期的访客（清理协程用）。
func (s *Store) ListExpiredGuests(ctx context.Context, now time.Time) ([]User, error) {
	out := []User{}
	rows, err := s.bun.QueryContext(ctx,
		"SELECT id, username FROM users WHERE role = 'guest' AND expires_at IS NOT NULL AND expires_at < ?", now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username); err != nil {
			return nil, err
		}
		u.Role = RoleGuest
		out = append(out, u)
	}
	return out, rows.Err()
}

// ChannelIDsOfUser 用户的 channel_members 行覆盖的频道 id（清理访客时逐频道踢现场用）。
func (s *Store) ChannelIDsOfUser(ctx context.Context, userID int64) ([]int64, error) {
	var out []int64
	rows, err := s.bun.QueryContext(ctx,
		"SELECT channel_id FROM channel_members WHERE user_id = ?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CreateGuestInvite 创建访客类邀请：绑定频道，guestTTLSec 是产出访客的寿命。
func (s *Store) CreateGuestInvite(ctx context.Context, createdBy, channelID int64, maxUses int, ttl, guestTTL time.Duration) (*Invite, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	code := make([]byte, 8)
	for i, b := range buf {
		code[i] = inviteAlphabet[int(b)%len(inviteAlphabet)]
	}
	inv := &Invite{Code: string(code), Kind: "guest", ChannelID: &channelID,
		GuestTTLSec: int(guestTTL.Seconds()), MaxUses: maxUses, ExpiresAt: time.Now().Add(ttl)}
	row := &inviteRow{Code: inv.Code, Kind: "guest", ChannelID: &channelID,
		GuestTTL: inv.GuestTTLSec, MaxUses: maxUses, CreatedBy: createdBy, ExpiresAt: inv.ExpiresAt}
	_, err := s.bun.NewInsert().Model(row).
		Column("code", "kind", "channel_id", "guest_ttl_sec", "max_uses", "expires_at", "created_by").
		Value("max_uses", "?", maxUses).
		Returning("id").
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	inv.ID = row.ID
	inv.CreatedByID = createdBy
	return inv, nil
}

// ListGuestInvites 某频道的访客邀请（频道管理「访客邀请」分区用）。
func (s *Store) ListGuestInvites(ctx context.Context, channelID int64) ([]Invite, error) {
	return s.listInvites(ctx, " WHERE i.channel_id = ? AND i.kind = 'guest'", channelID)
}
