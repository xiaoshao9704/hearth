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
	result, err := a.st.MigrateRoleData(ctx)
	if err != nil {
		return err
	}
	if result.SuperID == 0 {
		log.Printf("数据迁移 v5：没有可用的旧管理员可提升；空库的首个账号会自动成为 super，已有账号时可执行 hearth promote <用户名>")
	} else {
		log.Printf("数据迁移 v5：当前 super uid=%d", result.SuperID)
	}
	if result.SkippedChannel > 0 {
		log.Printf("数据迁移 v5 警告：跳过 %d 个 created_by 指向已删用户的频道", result.SkippedChannel)
	}
	log.Printf("数据迁移 v5 完成：系统角色阶梯与频道角色已落地")
	return nil
}
