// 聊天消息的落库与历史回放，以及第二版能力：引用回复存在性查询、软删、表情反应、保留策略清理。
// 一条约束贯穿全文件：软删只清内容不删行——id 是数据线广播与前端去重的键，
// 行删了对端就补不齐"这条被撤回了"，历史里会凭空少一条。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

type Message struct {
	bun.BaseModel `bun:"table:messages,alias:m"`

	ID        int64        `bun:",pk,autoincrement" json:"id"`
	ChannelID int64        `json:"channel_id"`
	UserID    int64        `json:"uid"`              // 发送者 user_id（右键菜单等管理操作的目标）
	Username  string       `bun:"-" json:"username"` // 发送者用户名（JOIN 填充，纯展示）
	Kind      string       `json:"kind"`             // text/file
	Content   string       `json:"content"`
	File      *MessageFile `bun:"-" json:"file,omitempty"` // kind=file 才有；由 meta 列的 JSON 解出
	ReplyTo   *int64       `json:"reply_to"`              // 引用回复指向的同频道消息 id
	Deleted   bool         `bun:"-" json:"deleted"`       // 由 deleted_at 派生；true 时 Content/File 已清空
	Reactions []Reaction   `bun:"-" json:"reactions"`     // 表情反应聚合（AttachReactions 填充，见 chat.go）
	CreatedAt time.Time    `json:"created_at"`
}

// MessageFile 文件消息的卡片元数据。字节不经 hearth（走内核数据通道扇出），
// 库里只留"发过这么一个文件"的记录，晚进房的人看到卡片但拿不到字节。
type MessageFile struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
}

// ---- 消息 ----

// 消息类型：文本 / 文件卡片。
const (
	KindText = "text"
	KindFile = "file"
)

// deleted 用 CASE 而非直接取 deleted_at：三方言对布尔表达式与 NULL 时间的返回类型各不相同，
// 折成 0/1 整数最省事——业务只关心"删没删"，删除时刻不出现在任何接口上。
// 用户名走 LEFT JOIN + 兜底文案：访客过期被清理后消息行仍在，JOIN 落空不该让整条消息消失。
const messageCols = `m.id, m.channel_id, m.user_id, COALESCE(u.username, '已离开的访客'), m.kind, m.content, m.meta, m.reply_to,
CASE WHEN m.deleted_at IS NULL THEN 0 ELSE 1 END, m.created_at`

// scanMessage 按 messageCols 的列序读一行，并把 meta 列的 JSON 解成 File。
func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var meta sql.NullString
	var replyTo sql.NullInt64
	var deleted int64
	if err := sc.Scan(&m.ID, &m.ChannelID, &m.UserID, &m.Username, &m.Kind, &m.Content, &meta,
		&replyTo, &deleted, &m.CreatedAt); err != nil {
		return Message{}, err
	}
	if replyTo.Valid {
		m.ReplyTo = &replyTo.Int64
	}
	m.Deleted = deleted != 0
	m.Reactions = []Reaction{} // 没有反应时也要是空数组：前端按数组渲染，null 会多一层判空
	if m.Kind == KindFile && meta.Valid && meta.String != "" {
		var f MessageFile
		if json.Unmarshal([]byte(meta.String), &f) == nil {
			m.File = &f
		}
	}
	return m, nil
}

// AddMessage 落一条消息。kind=text 时 file 传 nil；kind=file 时 file 是卡片元数据
// （字节不入库，由发送方经内核数据通道扇出）。replyTo 非 nil 时是引用回复的目标 id
// （同频道存在且未删由接口层校验，见 api/chat_messages.go）。
// mentions 是服务端校验过的被@用户（见 api/push.go 的 validMentions），与文件卡片共用
// meta 列的同一个 JSON 对象；变参而非必填参数，是为了让不关心提及的调用方原样不动。
func (s *Store) AddMessage(ctx context.Context, channelID, userID int64, kind, content string, file *MessageFile, replyTo *int64, mentions ...int64) (*Message, error) {
	row := &messageRow{ChannelID: channelID, UserID: userID, Kind: kind, Content: content, ReplyTo: replyTo}
	meta := map[string]any{}
	if file != nil {
		meta["name"], meta["mime"], meta["size"] = file.Name, file.Mime, file.Size
	}
	if len(mentions) > 0 {
		meta["mentions"] = mentions
	}
	if len(meta) > 0 {
		raw, err := json.Marshal(meta)
		if err != nil {
			return nil, err
		}
		encoded := string(raw)
		row.Meta = &encoded
	}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	m, err := scanMessage(s.bun.QueryRowContext(ctx, `SELECT `+messageCols+`
FROM messages m LEFT JOIN users u ON u.id = m.user_id WHERE m.id = ?`, row.ID))
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// RecentMessages 返回频道最近 limit 条消息（按时间正序）。
func (s *Store) RecentMessages(ctx context.Context, channelID int64, limit int) ([]Message, error) {
	rows, err := s.bun.QueryContext(ctx, `SELECT `+messageCols+`
FROM messages m LEFT JOIN users u ON u.id = m.user_id
WHERE m.channel_id = ? ORDER BY m.id DESC LIMIT ?`, channelID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	// 反转为时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// MessagesAfter 返回频道内 id 大于 afterID 的消息（按时间正序，至多 limit 条）；
// afterID<=0 等价"最近 limit 条"。断线重连后前端带最大已知 id 来补齐用。
func (s *Store) MessagesAfter(ctx context.Context, channelID, afterID int64, limit int) ([]Message, error) {
	if afterID <= 0 {
		return s.RecentMessages(ctx, channelID, limit)
	}
	rows, err := s.bun.QueryContext(ctx, `SELECT `+messageCols+`
FROM messages m LEFT JOIN users u ON u.id = m.user_id
WHERE m.channel_id = ? AND m.id > ? ORDER BY m.id ASC LIMIT ?`, channelID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

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
