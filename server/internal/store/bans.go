package store

import (
	"context"
)

// ---- 封禁 ----

// IsBanned 用户是否被该频道封禁。
func (s *Store) IsBanned(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.bun.NewRaw(
		"SELECT COUNT(1) FROM channel_bans WHERE channel_id = ? AND user_id = ?", channelID, userID).Scan(ctx, &n)
	return n > 0, err
}

func (s *Store) Ban(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewInsert().Model(&channelBanRow{ChannelID: channelID, UserID: userID}).Ignore().Exec(ctx)
	return err
}

func (s *Store) Unban(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewRaw(
		"DELETE FROM channel_bans WHERE channel_id = ? AND user_id = ?", channelID, userID).Exec(ctx)
	return err
}

// ListBans 返回该频道被封禁的用户名列表。
func (s *Store) ListBans(ctx context.Context, channelID int64) ([]UserRef, error) {
	return s.listUserRefs(ctx, "channel_bans", channelID)
}

// BansOf 该用户被封禁的全部频道 id 集合（大厅列表批量填充 banned 用，避免逐频道查询）。
func (s *Store) BansOf(ctx context.Context, userID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	rows, err := s.bun.QueryContext(ctx,
		"SELECT channel_id FROM channel_bans WHERE user_id = ?", userID)
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

// ---- 禁言 ----

// IsGagged 用户是否被该频道禁言。
func (s *Store) IsGagged(ctx context.Context, channelID, userID int64) (bool, error) {
	var n int
	err := s.bun.NewRaw(
		"SELECT COUNT(1) FROM channel_gags WHERE channel_id = ? AND user_id = ?", channelID, userID).Scan(ctx, &n)
	return n > 0, err
}

func (s *Store) Gag(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewInsert().Model(&channelGagRow{ChannelID: channelID, UserID: userID}).Ignore().Exec(ctx)
	return err
}

func (s *Store) Ungag(ctx context.Context, channelID, userID int64) error {
	_, err := s.bun.NewRaw(
		"DELETE FROM channel_gags WHERE channel_id = ? AND user_id = ?", channelID, userID).Exec(ctx)
	return err
}

// ListGags 返回该频道被禁言的用户名列表。
func (s *Store) ListGags(ctx context.Context, channelID int64) ([]UserRef, error) {
	return s.listUserRefs(ctx, "channel_gags", channelID)
}
