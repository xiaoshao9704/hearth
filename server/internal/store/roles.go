// 角色模型：系统角色（users.role，严格阶梯）与频道角色（channel_members.role，三档）。
// 判定逻辑（谁能操作谁、隐含房主等）在 perm 包收口，这里只放类型、档位数与存取。
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"
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
	return setChannelRole(ctx, s.bun, s.d.name, channelID, userID, role)
}

func setChannelRole(ctx context.Context, db bun.IDB, dialectName string, channelID, userID int64, role ChannelRole) error {
	row := &channelMemberRow{ChannelID: channelID, UserID: userID, Role: string(role)}
	q := db.NewInsert().Model(row).
		Column("channel_id", "user_id", "role").Value("role", "?", string(role))
	if dialectName == "mysql" {
		q = q.On("DUPLICATE KEY UPDATE").Set("role = VALUES(role)")
	} else { // sqlite / postgres 同语法
		q = q.On("CONFLICT (channel_id, user_id) DO UPDATE").Set("role = EXCLUDED.role")
	}
	_, err := q.Exec(ctx)
	return err
}

// TransferChannel 频道转让：旧 owner 降为 moderator，新主写 owner 行（其旧行被覆盖升档）。
// 两步必须在同一事务内；MySQL 没有部分唯一索引，这也是它的唯一 owner 约束。
func (s *Store) TransferChannel(ctx context.Context, channelID, newOwnerID int64) error {
	return s.bun.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewRaw(
			"UPDATE channel_members SET role = 'moderator' WHERE channel_id = ? AND role = 'owner'",
			channelID).Exec(ctx); err != nil {
			return err
		}
		return setChannelRole(ctx, tx, s.d.name, channelID, newOwnerID, ChannelRoleOwner)
	})
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

// RoleMigrationResult 是 v5 迁移需要写入启动日志的结果。
type RoleMigrationResult struct {
	SuperID        int64
	SkippedChannel int
}

// MigrateRoleData 角色数据迁移（api 游标 v5 调用，幂等）。所有数据改动在同一事务内：
//  1. is_admin=1 → role=admin，其中 id 最小的可用 admin → super；
//  2. 其余用户中拥有任何频道的 → power（不让现有房主失去建频道能力）；
//  3. 每个创建者仍存在的频道按 created_by 重建唯一 owner 行；
//  4. 按最终 role 同步一次冻结的 is_admin 兼容列，避免重入时复活已降级账号。
func (s *Store) MigrateRoleData(ctx context.Context) (RoleMigrationResult, error) {
	var result RoleMigrationResult
	err := s.bun.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewRaw(
			"UPDATE users SET role = 'admin' WHERE is_admin = 1 AND role = 'user'").Exec(ctx); err != nil {
			return err
		}

		var anySuperID int64
		if err := tx.NewRaw(
			"SELECT COALESCE(MIN(id), 0) FROM users WHERE role = 'super'").Scan(ctx, &anySuperID); err != nil {
			return err
		}
		if anySuperID == 0 {
			var adminID int64
			if err := tx.NewRaw(
				"SELECT COALESCE(MIN(id), 0) FROM users WHERE role = 'admin' AND disabled = 0").Scan(ctx, &adminID); err != nil {
				return err
			}
			if adminID != 0 {
				if _, err := tx.NewRaw("UPDATE users SET role = 'super' WHERE id = ?", adminID).Exec(ctx); err != nil {
					return err
				}
			}
		}

		if _, err := tx.NewRaw(
			"UPDATE users SET role = 'power' WHERE role = 'user' AND id IN (SELECT created_by FROM channels)").
			Exec(ctx); err != nil {
			return err
		}
		if err := tx.NewRaw(`
SELECT COUNT(1) FROM channels c LEFT JOIN users u ON u.id = c.created_by
WHERE u.id IS NULL`).Scan(ctx, &result.SkippedChannel); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
SELECT c.id, c.created_by FROM channels c JOIN users u ON u.id = c.created_by
ORDER BY c.id`)
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
			if _, err := tx.NewRaw(
				"UPDATE channel_members SET role = 'member' WHERE channel_id = ? AND role = 'owner'", c.id).
				Exec(ctx); err != nil {
				return err
			}
			if err := setChannelRole(ctx, tx, s.d.name, c.id, c.owner, ChannelRoleOwner); err != nil {
				return err
			}
		}

		if _, err := tx.NewRaw(`
UPDATE users SET is_admin = CASE WHEN role IN ('admin', 'super') THEN 1 ELSE 0 END`).Exec(ctx); err != nil {
			return err
		}
		return tx.NewRaw(
			"SELECT COALESCE(MIN(id), 0) FROM users WHERE role = 'super' AND disabled = 0").Scan(ctx, &result.SuperID)
	})
	return result, err
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
