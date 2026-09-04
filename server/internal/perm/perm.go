// perm 权限判定收口：所有「谁能做什么」的判断只允许出现在这里与 store 层
// （角色类型/存取）。handler 与中间件一律调本包，不得手写 IsAdmin/OwnerID 比较。
// 入场判定（谁能进房、能否发布）的唯一决策函数仍是 api.admitUser。
package perm

import (
	"context"

	"hearth/server/internal/store"
)

// SysAtLeast 用户的系统角色是否达到指定档（阶梯：高档包含低档能力）。
func SysAtLeast(u *store.User, r store.Role) bool {
	return u.Role.Rank() >= r.Rank()
}

// CanActOn 系统角色的高低约束：只能操作比自己低档的人。
// super 对任何人都 true；任何人（含 super）对 super 都 false——super 只能经 CLI 转移。
func CanActOn(actor, target *store.User) bool {
	return actor.Role.Rank() > target.Role.Rank()
}

// SettableRoles actor 可对 target 授予的系统角色候选（管理后台用户行的下拉项，
// 前端显隐以此为准、不推导阶梯）。访客不可经此授予（升级走注册邀请路径）。
func SettableRoles(actor, target *store.User) []store.Role {
	if !CanActOn(actor, target) || target.Role == store.RoleGuest {
		return nil
	}
	var out []store.Role
	for _, r := range []store.Role{store.RoleUser, store.RolePower, store.RoleAdmin} {
		if r.Rank() < actor.Role.Rank() {
			out = append(out, r)
		}
	}
	return out
}

// ChannelRole 用户在频道的有效角色：系统 admin+ 在任何频道隐含 owner，
// 其余读 channel_members 的成员行（owner/moderator/member/无行）。
func ChannelRole(ctx context.Context, st *store.Store, c *store.Channel, u *store.User) (store.ChannelRole, error) {
	if SysAtLeast(u, store.RoleAdmin) {
		return store.ChannelRoleOwner, nil
	}
	return st.ChannelRoleOf(ctx, c.ID, u.ID)
}

// ChannelAtLeast 频道角色是否达到指定档。
func ChannelAtLeast(cr, need store.ChannelRole) bool {
	return cr.Rank() >= need.Rank()
}

// CanActOnChannel 频道内只能管制比自己低的频道角色。
// 系统角色的 admin+ 保护是另一条正交约束，由调用方单独判定。
func CanActOnChannel(actor, target store.ChannelRole) bool {
	return actor.Rank() > target.Rank()
}
