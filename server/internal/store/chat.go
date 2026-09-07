// 聊天第二版数据模型的 store 侧：引用回复的存在性查询、软删、表情反应、保留策略清理。
// 消息本体的落库与历史回放仍在 store.go（AddMessage/MessagesAfter）。
// 一条约束贯穿全文件：软删只清内容不删行——id 是数据线广播与前端去重的键，
// 行删了对端就补不齐"这条被撤回了"，历史里会凭空少一条。
package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/uptrace/bun"
)

// Reaction 一条消息上某个表情的聚合结果：谁点过（uid 列表，顺序 = 最早点的在前）。
// 计数由前端取长度，服务端不另存冗余计数。
type Reaction struct {
	Emoji string  `json:"emoji"`
	UIDs  []int64 `json:"uids"`
}

// MessageByID 取同频道内的一条消息（含 deleted 派生列）；不存在返回 sql.ErrNoRows。
func (s *Store) MessageByID(ctx context.Context, channelID, id int64) (*Message, error) {
	m, err := scanMessage(s.bun.QueryRowContext(ctx, `SELECT `+messageCols+`
FROM messages m LEFT JOIN users u ON u.id = m.user_id WHERE m.id = ? AND m.channel_id = ?`, id, channelID))
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// SoftDeleteMessage 软删一条消息：打 deleted_at，同时清空内容与文件卡片、删掉它的表情反应。
// 内容是真清（不是只在响应里藏），撤回后库里不再留原文。已删的再删返回 false。
func (s *Store) SoftDeleteMessage(ctx context.Context, channelID, id int64) (bool, error) {
	res, err := s.bun.ExecContext(ctx, `UPDATE messages
SET content = '', meta = NULL, deleted_at = CURRENT_TIMESTAMP
WHERE id = ? AND channel_id = ? AND deleted_at IS NULL`, id, channelID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	_, err = s.bun.ExecContext(ctx, `DELETE FROM message_reactions WHERE message_id = ?`, id)
	return true, err
}

// AddReaction 记一次表情反应；同一 (消息, 用户, 表情) 重复点视为已存在，不报错。
// 去重靠主键冲突而不是先查后插：并发下前者才是真去重。
func (s *Store) AddReaction(ctx context.Context, messageID, userID int64, emoji string) error {
	_, err := s.bun.NewInsert().Model(&messageReactionRow{MessageID: messageID, UserID: userID, Emoji: emoji}).Exec(ctx)
	if err != nil && IsUniqueViolation(err) {
		return nil
	}
	return err
}

// RemoveReaction 取消一次表情反应；没点过也算成功（幂等）。
func (s *Store) RemoveReaction(ctx context.Context, messageID, userID int64, emoji string) error {
	_, err := s.bun.ExecContext(ctx,
		`DELETE FROM message_reactions WHERE message_id = ? AND user_id = ? AND emoji = ?`, messageID, userID, emoji)
	return err
}

// ReactionsOf 取一条消息的表情反应聚合。
func (s *Store) ReactionsOf(ctx context.Context, messageID int64) ([]Reaction, error) {
	byMsg, err := s.reactionsByMessage(ctx, []int64{messageID})
	if err != nil {
		return nil, err
	}
	if r := byMsg[messageID]; r != nil {
		return r, nil
	}
	return []Reaction{}, nil
}

// AttachReactions 给一批消息填 Reactions：整批一次查询，不按条 N+1。
func (s *Store) AttachReactions(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	byMsg, err := s.reactionsByMessage(ctx, ids)
	if err != nil {
		return err
	}
	for i := range msgs {
		if r := byMsg[msgs[i].ID]; r != nil {
			msgs[i].Reactions = r
		} else {
			msgs[i].Reactions = []Reaction{}
		}
	}
	return nil
}

// reactionsByMessage 取回三元组后在内存里聚合（不靠数据库的 GROUP_CONCAT，那三方言各写各的）。
// 排序先按 created_at（早点的在前），同秒内按表情/uid 兜底——CURRENT_TIMESTAMP 只到秒，
// 没有兜底键的话同一秒内的顺序由数据库自由决定，前端每次拉历史都可能换个排法。
func (s *Store) reactionsByMessage(ctx context.Context, ids []int64) (map[int64][]Reaction, error) {
	rows, err := s.bun.QueryContext(ctx, `SELECT message_id, emoji, user_id FROM message_reactions
WHERE message_id IN (?) ORDER BY message_id, created_at, emoji, user_id`, bun.In(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]Reaction{}
	for rows.Next() {
		var mid, uid int64
		var emoji string
		if err := rows.Scan(&mid, &emoji, &uid); err != nil {
			return nil, err
		}
		list := out[mid]
		at := -1
		for i := range list {
			if list[i].Emoji == emoji {
				at = i
				break
			}
		}
		if at < 0 {
			list = append(list, Reaction{Emoji: emoji, UIDs: []int64{uid}})
		} else {
			list[at].UIDs = append(list[at].UIDs, uid)
		}
		out[mid] = list
	}
	return out, rows.Err()
}

// PurgeMessagesBefore 按保留策略删除 cutoff 之前的消息（连同它们的表情反应），返回删掉的条数。
// 这里是真删行——过期的历史本来就不该再出现在任何地方，与软删的语义不同。
func (s *Store) PurgeMessagesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if _, err := s.bun.ExecContext(ctx, `DELETE FROM message_reactions
WHERE message_id IN (SELECT id FROM messages WHERE created_at < ?)`, cutoff); err != nil {
		return 0, err
	}
	res, err := s.bun.ExecContext(ctx, `DELETE FROM messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res), nil
}

// ClearChannelMessages 清空一个频道的聊天记录（连同表情反应），返回删掉的条数。
func (s *Store) ClearChannelMessages(ctx context.Context, channelID int64) (int64, error) {
	if _, err := s.bun.ExecContext(ctx, `DELETE FROM message_reactions
WHERE message_id IN (SELECT id FROM messages WHERE channel_id = ?)`, channelID); err != nil {
		return 0, err
	}
	res, err := s.bun.ExecContext(ctx, `DELETE FROM messages WHERE channel_id = ?`, channelID)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res), nil
}

// rowsAffected 影响行数只用于日志与回执，驱动不支持计数时按 0 算，不当失败。
func rowsAffected(res sql.Result) int64 {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}
