package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"
)

type Channel struct {
	bun.BaseModel `bun:"table:channels,alias:c"`

	ID         int64     `bun:",pk,autoincrement" json:"id"`
	Name       string    `json:"name"`
	CreatedBy  string    `bun:"-" json:"created_by"` // 房主用户名（JOIN 填充，房主归属见 OwnerID 注释）
	CreatedAt  time.Time `json:"created_at"`
	InviteOnly bool      `json:"invite_only"`
	MyRole     string    `bun:"-" json:"my_role"` // 对当前请求用户的频道角色（接口层填充，owner/moderator/member/""）
	Online     int       `bun:"-" json:"online"`  // 当前在房人数（接口层从内核填充）
	OwnerID    int64     `bun:"-" json:"-"`       // 房主用户 ID（内部用；权威是 channel_members 的 owner 行）
	Banned     bool      `bun:"-" json:"banned"`  // 当前用户是否被该频道封禁（接口层填充，大厅列表用）
	Muted      bool      `bun:"-" json:"muted"`   // 当前用户是否静音了该频道的提醒（接口层填充）
	Hidden     bool      `bun:"-" json:"hidden"`  // 仅 super 能看到的、非成员邀请制频道（普通人看不到，接口层填充）
}

// ---- 频道 ----

// scanChannel 统一扫描频道行（invite_only 以整数存储，兼容三种方言的布尔语义）。
func scanChannel(scanner interface{ Scan(...any) error }, c *Channel) error {
	var inviteOnly int64
	err := scanner.Scan(&c.ID, &c.Name, &c.CreatedBy, &c.CreatedAt, &inviteOnly, &c.OwnerID)
	c.InviteOnly = inviteOnly != 0
	return err
}

// 房主（OwnerID/CreatedBy）只认 channel_members 的 owner 行；channels.created_by 只作历史记录。
const channelCols = `c.id, c.name, COALESCE(ou.username, ''), c.created_at, c.invite_only, COALESCE(om.user_id, 0)`
const channelJoins = ` FROM channels c
LEFT JOIN channel_members om ON om.channel_id = c.id AND om.role = 'owner'
LEFT JOIN users ou ON ou.id = om.user_id`

func (s *Store) CreateChannel(ctx context.Context, name string, userID int64) (*Channel, error) {
	row := &channelRow{Name: name, CreatedBy: userID}
	if _, err := s.bun.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	// 归属的权威行：建频道即写 owner 成员行
	if err := s.SetChannelRole(ctx, row.ID, userID, ChannelRoleOwner); err != nil {
		return nil, err
	}
	return s.ChannelByID(ctx, row.ID)
}

// ChannelByID 按 ID 取频道完整信息（含房主与邀请制标记）。
func (s *Store) ChannelByID(ctx context.Context, id int64) (*Channel, error) {
	var c Channel
	err := scanChannel(s.bun.QueryRowContext(ctx,
		`SELECT `+channelCols+channelJoins+` WHERE c.id = ?`, id), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.bun.QueryContext(ctx,
		`SELECT `+channelCols+channelJoins+` ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := scanChannel(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ChannelByName(ctx context.Context, name string) (*Channel, error) {
	var c Channel
	err := scanChannel(s.bun.QueryRowContext(ctx,
		`SELECT `+channelCols+channelJoins+` WHERE c.name = ?`, name), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) SetInviteOnly(ctx context.Context, channelID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.bun.NewRaw("UPDATE channels SET invite_only = ? WHERE id = ?", v, channelID).Exec(ctx)
	return err
}
