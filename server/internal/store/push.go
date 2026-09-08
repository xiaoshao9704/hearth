// Web Push 订阅的存取。这一层只搬字节：加密、VAPID 签名与投递结果的解释全在
// api/push.go，存储侧不认识推送协议。
// endpoint 与两把密钥是浏览器发的凭据，不出库、不进日志——对外只按 uid 汇总。
package store

import (
	"context"
	"time"

	"github.com/uptrace/bun"
)

// PushSubscription 一条浏览器推送订阅。Endpoint/P256dh/Auth 是投递凭据，
// json 标签一律 "-"：它们只在服务端内部流转，不进任何响应。
type PushSubscription struct {
	ID        int64  `json:"-"`
	UserID    int64  `json:"-"`
	Endpoint  string `json:"-"`
	P256dh    string `json:"-"`
	Auth      string `json:"-"`
	FailCount int    `json:"-"`
}

// SavePushSubscription 记下一条订阅。同一 endpoint 只留一条：先删后插而不是三方言各写
// 一套 upsert——订阅行没有要保留的历史（失败计数重置正是想要的）。
func (s *Store) SavePushSubscription(ctx context.Context, userID int64, sessionID, endpoint, p256dh, auth, userAgent string) error {
	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}
	if _, err := s.bun.NewRaw("DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint).Exec(ctx); err != nil {
		return err
	}
	_, err := s.bun.NewInsert().Model(&pushSubscriptionRow{
		UserID:    userID,
		SessionID: sessionID,
		Endpoint:  endpoint,
		P256dh:    p256dh,
		Auth:      auth,
		UserAgent: userAgent,
		CreatedAt: time.Now(),
	}).Exec(ctx)
	return err
}

// SubscriptionsOf 这批用户的全部订阅（一个人多设备就多条）。uids 为空返回空切片。
func (s *Store) SubscriptionsOf(ctx context.Context, uids []int64) ([]PushSubscription, error) {
	out := []PushSubscription{}
	if len(uids) == 0 {
		return out, nil
	}
	q := "SELECT id, user_id, endpoint, p256dh, auth, fail_count FROM push_subscriptions WHERE user_id IN (?)"
	rows, err := s.bun.QueryContext(ctx, q, bun.In(uids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PushSubscription
		var fail int64
		if err := rows.Scan(&p.ID, &p.UserID, &p.Endpoint, &p.P256dh, &p.Auth, &fail); err != nil {
			return nil, err
		}
		p.FailCount = int(fail)
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPushSubscriptions 该用户的订阅条数（测试与诊断用）。
func (s *Store) CountPushSubscriptions(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.bun.NewRaw("SELECT COUNT(1) FROM push_subscriptions WHERE user_id = ?", userID).Scan(ctx, &n)
	return n, err
}

// DeletePushSubscription 按 endpoint 删（退订）；限本人的订阅，别人的地址删不掉。
func (s *Store) DeletePushSubscription(ctx context.Context, userID int64, endpoint string) error {
	_, err := s.bun.NewRaw("DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?",
		endpoint, userID).Exec(ctx)
	return err
}

// DeletePushSubscriptionByID 按行 id 删（投递返回 404/410：这个地址已经作废）。
func (s *Store) DeletePushSubscriptionByID(ctx context.Context, id int64) error {
	_, err := s.bun.NewRaw("DELETE FROM push_subscriptions WHERE id = ?", id).Exec(ctx)
	return err
}

// DeletePushSubscriptionsBySession 删一条会话留下的订阅（退出登录 / 远程下线该会话）。
func (s *Store) DeletePushSubscriptionsBySession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.bun.NewRaw("DELETE FROM push_subscriptions WHERE session_id = ?", sessionID).Exec(ctx)
	return err
}

// DeletePushSubscriptionsOf 删一个用户的全部订阅（停用账号）。
func (s *Store) DeletePushSubscriptionsOf(ctx context.Context, userID int64) error {
	_, err := s.bun.NewRaw("DELETE FROM push_subscriptions WHERE user_id = ?", userID).Exec(ctx)
	return err
}

// TouchPushSubscription 投递成功：记时间并清零失败计数。
func (s *Store) TouchPushSubscription(ctx context.Context, id int64) error {
	_, err := s.bun.NewRaw("UPDATE push_subscriptions SET last_ok_at = ?, fail_count = 0 WHERE id = ?",
		time.Now(), id).Exec(ctx)
	return err
}

// IncPushFailure 投递失败：失败计数 +1 并返回累计值（调用方按上限决定是否删除）。
func (s *Store) IncPushFailure(ctx context.Context, id int64) (int, error) {
	if _, err := s.bun.NewRaw("UPDATE push_subscriptions SET fail_count = fail_count + 1 WHERE id = ?", id).Exec(ctx); err != nil {
		return 0, err
	}
	var n int
	err := s.bun.NewRaw("SELECT fail_count FROM push_subscriptions WHERE id = ?", id).Scan(ctx, &n)
	return n, err
}
