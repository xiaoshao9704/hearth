// Bun 建表模型：导出类型的列子集不足以描述整表（users.password_hash、sessions 等），
// 这里用未导出行结构体给出完整列定义，仅供 baseline 迁移 CreateTable 与后续模型写入使用；
// 读取仍 Scan 到现有导出类型。模型一律不声明外键（与 mysql 现状对齐，删除级联在应用层做）。
// 布尔语义的列（is_admin/disabled/invite_only/revoked）沿用整数存储，兼容三方言。
package store

import (
	"time"

	"github.com/uptrace/bun"
)

// 字符串列的 type: 与旧 mysql DDL 键长对齐（索引/主键列不能用 TEXT）；
// sqlite/pg 下这些措辞差异不影响行为。
type userRow struct {
	bun.BaseModel `bun:"table:users"`

	ID           int64     `bun:",pk,autoincrement"`
	Username     string    `bun:",notnull,unique,type:varchar(64)"`
	PasswordHash string    `bun:",notnull,type:varchar(255)"`
	IsAdmin      int64     `bun:",notnull,default:0"`
	Disabled     int64     `bun:",notnull,default:0"`
	CreatedAt    time.Time `bun:",notnull,default:current_timestamp"`
}

type sessionRow struct {
	bun.BaseModel `bun:"table:sessions"`

	Token     string    `bun:",pk,type:varchar(128)"`
	UserID    int64     `bun:",notnull"`
	ExpiresAt time.Time `bun:",notnull"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

type channelRow struct {
	bun.BaseModel `bun:"table:channels"`

	ID         int64     `bun:",pk,autoincrement"`
	Name       string    `bun:",notnull,unique,type:varchar(128)"`
	CreatedBy  int64     `bun:",notnull"`
	InviteOnly int64     `bun:",notnull,default:0"`
	CreatedAt  time.Time `bun:",notnull,default:current_timestamp"`
}

type messageRow struct {
	bun.BaseModel `bun:"table:messages"`

	ID        int64     `bun:",pk,autoincrement"`
	ChannelID int64     `bun:",notnull"`
	UserID    int64     `bun:",notnull"`
	Content   string    `bun:",notnull,type:text"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

type deviceRow struct {
	bun.BaseModel `bun:"table:devices"`

	ID        int64     `bun:",pk,autoincrement"`
	UserID    int64     `bun:",notnull,unique:uk_devices"`
	DeviceID  string    `bun:",notnull,unique:uk_devices,type:varchar(32)"`
	Tag       string    `bun:",notnull,default:'',type:varchar(64)"`
	FirstSeen time.Time `bun:",notnull,default:current_timestamp"`
	LastSeen  time.Time `bun:",notnull,default:current_timestamp"`
}

type ingressRow struct {
	bun.BaseModel `bun:"table:ingresses"`

	ID        int64     `bun:",pk,autoincrement"`
	UserID    int64     `bun:",notnull,unique:uk_ingresses"`
	ChannelID int64     `bun:",notnull,unique:uk_ingresses"`
	IngressID string    `bun:",notnull,type:varchar(128)"`
	StreamKey string    `bun:",notnull,type:varchar(128)"`
	Provider  string    `bun:",notnull,default:'livekit',type:varchar(32)"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

// ingestTokenRow 每用户一把推流令牌（00002 迁移建表；不分频道/设备，频道在 WHIP URL 里）。
// tag 是可改的设备标签属性（默认 obs），token 是全局唯一的用户凭证。
type ingestTokenRow struct {
	bun.BaseModel `bun:"table:ingest_tokens"`

	ID        int64     `bun:",pk,autoincrement"`
	UserID    int64     `bun:",notnull,unique"`
	Tag       string    `bun:",notnull,default:'obs',type:varchar(32)"`
	Token     string    `bun:",notnull,unique,type:varchar(64)"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

// ingestEndpointRow livekit-ingress 实例按（令牌, alias）持有的上游端点凭证（00002 迁移建表）。
// bound_room 空 = 未绑定/已解绑；令牌重置/改标签时整行清空（应用层删除）。
type ingestEndpointRow struct {
	bun.BaseModel `bun:"table:ingest_endpoints"`

	ID          int64  `bun:",pk,autoincrement"`
	TokenID     int64  `bun:",notnull,unique:uk_ingest_endpoints"`
	Alias       string `bun:",notnull,unique:uk_ingest_endpoints,type:varchar(64)"`
	IngressID   string `bun:",notnull,type:varchar(128)"`
	UpstreamKey string `bun:",notnull,type:varchar(128)"`
	BoundRoom   string `bun:",notnull,default:'',type:varchar(128)"`
}

// channel_bans / channel_gags / channel_members 三表结构相同，各立一个行结构体（表名不同）。
type channelBanRow struct {
	bun.BaseModel `bun:"table:channel_bans"`

	ChannelID int64     `bun:",notnull,unique:uk_channel_bans"`
	UserID    int64     `bun:",notnull,unique:uk_channel_bans"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

type channelGagRow struct {
	bun.BaseModel `bun:"table:channel_gags"`

	ChannelID int64     `bun:",notnull,unique:uk_channel_gags"`
	UserID    int64     `bun:",notnull,unique:uk_channel_gags"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

type channelMemberRow struct {
	bun.BaseModel `bun:"table:channel_members"`

	ChannelID int64     `bun:",notnull,unique:uk_channel_members"`
	UserID    int64     `bun:",notnull,unique:uk_channel_members"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

type inviteRow struct {
	bun.BaseModel `bun:"table:invites"`

	ID        int64     `bun:",pk,autoincrement"`
	Code      string    `bun:",notnull,unique,type:varchar(32)"`
	Note      string    `bun:",notnull,default:'',type:varchar(255)"`
	MaxUses   int       `bun:",notnull,default:1"`
	Used      int       `bun:",notnull,default:0"`
	Revoked   int64     `bun:",notnull,default:0"`
	CreatedBy int64     `bun:",notnull"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
	ExpiresAt time.Time `bun:",notnull"`
}

type settingRow struct {
	bun.BaseModel `bun:"table:settings"`

	K string `bun:",pk,type:varchar(64)"`
	V string `bun:",notnull,type:text"`
}

type providerRow struct {
	bun.BaseModel `bun:"table:providers"`

	Alias     string    `bun:",pk,type:varchar(64)"`
	Type      string    `bun:",notnull,type:varchar(32)"`
	Params    string    `bun:",notnull,type:text"`
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
	UpdatedAt time.Time `bun:",notnull,default:current_timestamp"`
}
