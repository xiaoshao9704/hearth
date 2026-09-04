// 数据语义迁移 v5：分层权限与频道角色（schema 在 store 迁移 00003）。
// 规则见 docs/plan-roles-guests.md 第二节：is_admin→role（首个 admin 提 super）、
// 房主提 power、created_by 迁入 channel_members(owner)。store.MigrateRoleData 幂等，
// 失败不前进游标、下次启动重试。
package api

import (
	"context"
	"log"
)

func (a *API) migrateRoles(ctx context.Context) error {
	if err := a.st.MigrateRoleData(ctx); err != nil {
		return err
	}
	log.Printf("数据迁移 v5 完成：系统角色阶梯与频道角色已落地")
	return nil
}
