// 过期访客清理：每 10 分钟扫一次，删除 expires_at 已过的访客（复用 DeleteUser 级联，
// 访客没有 owner 行，不涉及频道过户）。在房的访客先逐频道踢出现场再删行——
// 不踢的话已建立的 ember/livekit 会话会一直连着（进房凭证只在入会时判定一次）。
package api

import (
	"context"
	"log"
	"time"
)

const guestSweepInterval = 10 * time.Minute

// GuestSweepLoop 周期清理过期访客，随服务生命周期运行（ctx 取消即停）。
func (a *API) GuestSweepLoop(ctx context.Context) {
	t := time.NewTicker(guestSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.sweepGuests(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *API) sweepGuests(ctx context.Context) {
	guests, err := a.st.ListExpiredGuests(ctx, time.Now())
	if err != nil {
		log.Printf("过期访客扫描失败: %v", err)
		return
	}
	for _, g := range guests {
		// 先踢现场再删行：删行后成员关系没了，拿不到该踢哪些频道
		if chans, err := a.st.ChannelIDsOfUser(ctx, g.ID); err == nil {
			for _, cid := range chans {
				if c, cerr := a.st.ChannelByID(ctx, cid); cerr == nil {
					a.evictCtx(ctx, c, &g, "")
				}
			}
		}
		if _, err := a.st.DeleteUser(ctx, g.ID, 0); err != nil {
			log.Printf("清理过期访客 %s (id=%d) 失败: %v", g.Username, g.ID, err)
			continue
		}
		log.Printf("过期访客已清理: %s (id=%d)", g.Username, g.ID)
	}
}
