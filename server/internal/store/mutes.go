// 频道通知静音的存取：每用户每频道一行，有行 = 静音。
// 与 channel_gags（禁言，别人施加的管制）无关：这里是本人对自己的提醒开关，
// 只影响提示音/通知/推送，不影响能不能进房与发言。
package store

import "context"

// MuteChannel 静音一个频道；已静音再静音算成功（幂等，去重靠唯一约束冲突）。
func (s *Store) MuteChannel(ctx context.Context, userID, channelID int64) error {
	_, err := s.bun.NewInsert().Model(&channelMuteRow{ChannelID: channelID, UserID: userID}).Exec(ctx)
	if err != nil && IsUniqueViolation(err) {
		return nil
	}
	return err
}

// UnmuteChannel 取消静音；没静音过也算成功（幂等）。
func (s *Store) UnmuteChannel(ctx context.Context, userID, channelID int64) error {
	_, err := s.bun.NewRaw("DELETE FROM channel_mutes WHERE channel_id = ? AND user_id = ?",
		channelID, userID).Exec(ctx)
	return err
}

// MutedChannelIDs 该用户静音了哪些频道（频道列表的 muted 字段用，一次查询批量填充）。
func (s *Store) MutedChannelIDs(ctx context.Context, userID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	var ids []int64
	if err := s.bun.NewRaw("SELECT channel_id FROM channel_mutes WHERE user_id = ?", userID).Scan(ctx, &ids); err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// MutedUsersOf 这个频道被谁静音了（推送目标集合据此剔人）。
func (s *Store) MutedUsersOf(ctx context.Context, channelID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	var uids []int64
	if err := s.bun.NewRaw("SELECT user_id FROM channel_mutes WHERE channel_id = ?", channelID).Scan(ctx, &uids); err != nil {
		return nil, err
	}
	for _, uid := range uids {
		out[uid] = true
	}
	return out, nil
}
