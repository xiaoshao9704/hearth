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

	ID           int64      `bun:",pk,autoincrement"`
	Username     string     `bun:",notnull,unique,type:varchar(64)"`
	PasswordHash string     `bun:",notnull,type:varchar(255)"`
	IsAdmin      int64      `bun:",notnull,default:0"` // 冻结列：role 是权威，is_admin 只读派生，下个版本删列
	Disabled     int64      `bun:",notnull,default:0"`
	Role         string     `bun:",notnull,default:'user',type:varchar(16)"`
	ExpiresAt    *time.Time // 仅访客有值，过期即清理
	InviteID     *int64     // 访客来源邀请（升级/审计用）
	CreatedAt    time.Time  `bun:",notnull,default:current_timestamp"`
}

type sessionRow struct {
	bun.BaseModel `bun:"table:sessions"`

	Token     string    `bun:",pk,type:varchar(128)"`
	UserID    int64     `bun:",notnull"`
	ExpiresAt time.Time `bun:",notnull"`
	DeviceID  string    `bun:",notnull,default:'',type:varchar(32)"` // 非空 = 绑定设备（访客会话）；普通会话留空不绑定
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
	UserAgent string    `bun:",notnull,default:'',type:varchar(255)"` // 登录时的 UA 原文，只做「我的登录设备」列表展示
	// 存量库补列只能加可空列（sqlite 不接受 NOT NULL DEFAULT CURRENT_TIMESTAMP 的 ALTER），
	// 读取侧一律把空值当作「没记录过」，见 sessions.go
	LastSeen *time.Time
}

// auditRow 管制动作的审计事实（00006 迁移建表）。actor 是动作发起者，
// target_uid / channel_id 允许为空（不针对某人、或不属于某频道的动作）。
type auditRow struct {
	bun.BaseModel `bun:"table:audit_log"`

	ID        int64     `bun:",pk,autoincrement"`
	At        time.Time `bun:",notnull,default:current_timestamp"`
	ActorUID  int64     `bun:",notnull"`
	Action    string    `bun:",notnull,type:varchar(32)"`
	TargetUID *int64
	ChannelID *int64
	Detail    string `bun:",notnull,type:text"` // mysql 的 TEXT 列不能带 DEFAULT，写入侧一律给值
}

// passkeyRow 一枚通行密钥凭证（00007 迁移建表）。credential_id / public_key / aaguid
// 存 base64url 文本（见迁移文件的理由）；sign_count 与 backup_state 每次成功断言后更新，
// user_verified 是规范里的 uvInitialized（只从 false 变 true，不回退）。
type passkeyRow struct {
	bun.BaseModel `bun:"table:passkeys"`

	ID             int64      `bun:",pk,autoincrement"`
	UserID         int64      `bun:",notnull"`
	CredentialID   string     `bun:",notnull,unique,type:varchar(512)"`
	PublicKey      string     `bun:",notnull,type:text"`
	SignCount      int64      `bun:",notnull,default:0"`
	AAGUID         string     `bun:"aaguid,notnull,default:'',type:varchar(64)"`
	Transports     string     `bun:",notnull,default:'',type:varchar(128)"` // 逗号分隔的 AuthenticatorTransport
	BackupEligible int64      `bun:",notnull,default:0"`
	BackupState    int64      `bun:",notnull,default:0"`
	UserVerified   int64      `bun:",notnull,default:0"`
	Name           string     `bun:",notnull,default:'',type:varchar(64)"` // 用户可改，默认按注册时的 UA 生成
	CreatedAt      time.Time  `bun:",notnull,default:current_timestamp"`
	LastUsedAt     *time.Time // 从未用过为空
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

	ID        int64      `bun:",pk,autoincrement"`
	ChannelID int64      `bun:",notnull"`
	UserID    int64      `bun:",notnull"`
	Content   string     `bun:",notnull,type:text"`
	Kind      string     `bun:",notnull,default:'text',type:varchar(16)"` // text/file
	Meta      *string    `bun:",type:text"`                               // kind=file 时的卡片 JSON {name,mime,size}；字节不入库
	ReplyTo   *int64     // 引用回复指向的同频道消息 id；不做外键（被引消息软删后仍留指向）
	DeletedAt *time.Time // 非空 = 已撤回/被删；行保留，内容与文件卡片已清空
	CreatedAt time.Time  `bun:",notnull,default:current_timestamp"`
}

// messageReactionRow 消息表情反应：(消息, 用户, 表情) 三元组即主键，去重靠主键冲突而非应用层查重。
// 表情集合由接口层白名单限定（不入库校验），列宽按最长的带变体选择符表情留足。
type messageReactionRow struct {
	bun.BaseModel `bun:"table:message_reactions"`

	MessageID int64     `bun:",pk"`
	UserID    int64     `bun:",pk"`
	Emoji     string    `bun:",pk,type:varchar(32)"`
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
	Role      string    `bun:",notnull,default:'member',type:varchar(16)"` // owner/moderator/member；owner 是频道归属的权威
	CreatedAt time.Time `bun:",notnull,default:current_timestamp"`
}

type inviteRow struct {
	bun.BaseModel `bun:"table:invites"`

	ID         int64     `bun:",pk,autoincrement"`
	Code       string    `bun:",notnull,unique,type:varchar(32)"`
	Kind       string    `bun:",notnull,default:'register',type:varchar(16)"` // register/guest
	ChannelID  *int64    // guest 类必填：授予的频道
	Role       string    `bun:",notnull,default:'user',type:varchar(16)"` // register 产出的系统角色（user/power）
	GuestTTL   int       `bun:"guest_ttl_sec,notnull,default:0"`          // guest 类：产出访客的寿命（秒）
	AllowGuest int64     `bun:",notnull,default:0"`                       // register 类：是否允许「先以访客进入」
	Note       string    `bun:",notnull,default:'',type:varchar(255)"`
	MaxUses    int       `bun:",notnull,default:1"`
	Used       int       `bun:",notnull,default:0"`
	Revoked    int64     `bun:",notnull,default:0"`
	CreatedBy  int64     `bun:",notnull"`
	CreatedAt  time.Time `bun:",notnull,default:current_timestamp"`
	ExpiresAt  time.Time `bun:",notnull"`
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
