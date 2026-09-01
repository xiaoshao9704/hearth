// Package rtc 定义实时音视频内核的中性抽象：房间内核（Provider）与推流入口（IngestProvider）。
// 设计目标：
//   - api 层只依赖本包接口，不出现任何具体内核（LiveKit / SRS / 自研 P2P …）的类型与术语；
//   - 每个实现自带配置键（命名空间隔离），换实现时旧配置原样保留，新实现读自己的键，
//     不需要迁移或改造既有配置；
//   - 浏览器可见地址与同源代理上游都由实现声明，接入层只负责挂路由与兜底推导。
package rtc

import (
	"context"
	"errors"
)

// ErrNoParticipant 目标用户在指定房间内没有参与者（服务端静音等操作的前置条件不满足）。
var ErrNoParticipant = errors.New("目标用户不在房间")

// ConfigFunc 取动态配置的生效值（环境变量 > 数据库 > 默认），由接入层注入。
type ConfigFunc func(ctx context.Context, name string) string

// ConfigKey 实现声明自己的配置键：接入层据此渲染管理后台表单与判定环境锁定。
type ConfigKey struct {
	Name    string `json:"name"` // settings 键名（cfg_ 前缀存储），实现自定命名空间
	Env     string `json:"env"`  // 对应环境变量（设置了则锁定为只读）
	Label   string `json:"label"`
	Hint    string `json:"hint"`
	Secret  bool   `json:"secret"`
	Group   string   `json:"group"`             // 管理后台分组
	Options []string `json:"options,omitempty"` // 枚举可选值（管理后台渲染选择框）；空 = 自由文本
	Default string   `json:"-"`                 // 环境与数据库都未设置时的兜底值，由实现声明
}

// Credentials 进房凭证。Engine 告诉前端用哪个客户端引擎与内核对话
// （信令无标准协议，前后端实现必须配套；前端按名字动态加载对应 engine）。
type Credentials struct {
	URL    string // 浏览器连接地址；空 = 由接入层推导同源信令代理地址
	Token  string
	Engine string // 客户端引擎名：livekit / （将来）pion-voice …
}

// Participant 房间参与者的精简信息。
type Participant struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	JoinedAt int64  `json:"joined_at"` // Unix 秒
}

// Provider 房间内核：签发进房凭证与房间管理。
type Provider interface {
	Name() string
	// JoinCredentials 为用户签发进入房间的凭证；deviceTag 用于区分同账号多设备；
	// canPublish=false 表示用户被禁言（channel_gags），签发无发布权限的凭证。
	JoinCredentials(ctx context.Context, room, username, deviceTag string, canPublish bool) (Credentials, error)
	// RoomCounts 各房间在房人数（房间名 -> 人数）；错误视为内核不可达。
	RoomCounts(ctx context.Context) (map[string]int, error)
	ListParticipants(ctx context.Context, room string) ([]Participant, error)
	// RemoveParticipantsOf 把某用户的全部设备移出房间，返回移除数量。
	RemoveParticipantsOf(ctx context.Context, room, username string) (int, error)
	// MuteUserAudio 服务端禁言/解禁某用户全部设备（改写发布权限，禁言期间无法自行开麦/推流）；
	// 用户不在房间时返回 ErrNoParticipant。
	MuteUserAudio(ctx context.Context, room, username string, muted bool) error
	// SignalProxyUpstream 同源信令代理的上游地址；空 = 该内核不需要信令代理。
	SignalProxyUpstream(ctx context.Context) string
}

// IngestProvider 推流入口（如 WHIP 协议的硬编推流），可整体停用。
type IngestProvider interface {
	Name() string
	// Enabled 推流入口是否可用；false 时推流相关接口整体停用。
	Enabled(ctx context.Context) bool
	// CreateEndpoint 为「用户 × 房间」创建推流端点，返回内核侧 ID 与推流密钥。
	CreateEndpoint(ctx context.Context, room, username string) (id, streamKey string, err error)
	DeleteEndpoint(ctx context.Context, id string) error
	// PublicBase 浏览器可见的推流基地址；空 = 由接入层推导同源推流代理地址。
	PublicBase(ctx context.Context) string
	// ProxyUpstream 同源推流代理的上游地址；空 = 该实现不需要推流代理。
	ProxyUpstream(ctx context.Context) string
}
