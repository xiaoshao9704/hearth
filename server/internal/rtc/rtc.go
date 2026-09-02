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
	"net/http"
	"strings"
)

// ErrNoParticipant 目标用户在指定房间内没有参与者（服务端静音等操作的前置条件不满足）。
var ErrNoParticipant = errors.New("目标用户不在房间")

// MatchesUser identity 是否归属该用户（identity 约定：{用户名} 或 {用户名}-{设备标签/obs}）。
// kick/禁言等按用户扫参与者的实现统一用它，避免各处手写前缀判断漂移。
func MatchesUser(identity, username string) bool {
	return identity == username || strings.HasPrefix(identity, username+"-")
}

// ConfigFunc 取动态配置的生效值（环境变量 > 数据库 > 默认），由接入层注入。
type ConfigFunc func(ctx context.Context, name string) string

// ConfigKey 实现声明自己的配置键：接入层据此渲染管理后台表单与判定环境锁定。
type ConfigKey struct {
	Name    string   `json:"name"` // settings 键名（cfg_ 前缀存储），实现自定命名空间
	Env     string   `json:"env"`  // 对应环境变量（设置了则锁定为只读）
	Label   string   `json:"label"`
	Hint    string   `json:"hint"`
	Secret  bool     `json:"secret"`
	Group   string   `json:"group"`             // 管理后台分组
	Options []string `json:"options,omitempty"` // 枚举可选值（管理后台渲染选择框）；空 = 自由文本
	Default string   `json:"-"`                 // 环境与数据库都未设置时的兜底值，由实现声明
}

// Credentials 进房凭证。Engine 告诉前端用哪个客户端引擎与内核对话
// （信令无标准协议，前后端实现必须配套；前端按名字动态加载对应 engine）。
type Credentials struct {
	URL    string // 浏览器连接地址；空 = 由接入层推导同源信令代理地址
	Token  string
	Engine string // 客户端引擎名：livekit / ember …
}

// Participant 房间参与者的精简信息。
type Participant struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	JoinedAt int64  `json:"joined_at"` // Unix 秒
}

// Provider 语音（房间）内核：签发进房凭证与房间管理。语音槽位与舞台槽位都建立在它之上。
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
	// MuteUserAudio 服务端禁言/解禁某用户全部设备。
	// 契约：禁言 = 禁止该用户的**全部媒体发布**（音频、摄像头、投屏一并没收），
	// 不只是音频——LiveKit 实现收走 CanPublish；纯音频内核丢弃全部上行即等效。
	// 新实现必须遵循同一语义，与 joinToken 按 canPublish=false 签发的进房凭证一致。
	// 用户不在房间时返回 ErrNoParticipant。
	MuteUserAudio(ctx context.Context, room, username string, muted bool) error
	// SignalProxyUpstream 同源信令代理的上游地址；空 = 该内核不需要信令代理。
	SignalProxyUpstream(ctx context.Context) string
}

// StageProvider 舞台内核：投屏/摄像头等视频能力，**包含全部语音能力**（内嵌 Provider）。
// 舞台槽位只接受本接口：纯语音内核（如 ember）不能被选作舞台线；
// 语音内核补齐视频能力时实现本接口即可上舞台线，视频专属方法届时加在这里。
type StageProvider interface {
	Provider
}

// IngestProvider 推流入口（如 WHIP 协议的硬编推流），可整体停用。
type IngestProvider interface {
	Name() string
	// Enabled 推流入口是否可用；false 时推流相关接口整体停用。
	Enabled(ctx context.Context) bool
	// CreateEndpoint 为「用户 × 房间」创建推流端点，返回内核侧 ID 与推流密钥。
	CreateEndpoint(ctx context.Context, room, username string) (id, streamKey string, err error)
	DeleteEndpoint(ctx context.Context, id string) error
	// ProxyUpstream 同源推流代理的上游地址；空 = 该实现不需要推流代理。
	ProxyUpstream(ctx context.Context) string
}

// WHIPToken 从 WHIP 请求取令牌：路径 /w/{token} 的首段；POST 且路径无段时取
// Authorization: Bearer（OBS bearer 模式，bearer=true，端点应规范化为精确 /w）。
func WHIPToken(r *http.Request) (token string, bearer bool) {
	token = strings.Trim(strings.TrimPrefix(r.URL.Path, "/w"), "/")
	if i := strings.IndexByte(token, '/'); i >= 0 {
		token = token[:i]
	}
	if token == "" && r.Method == http.MethodPost {
		return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), true
	}
	return token, false
}

// WHIPGrantIssuer 可选能力：远端 WHIP 网关的通行证签发。接入层在反代前做完入场判定，
// 把结果签成短时效通行证塞进请求头，网关本地验签即可（与进房凭证同一模型：
// 接入层签、内核验）。实现该接口且 ProxyUpstream 非空时，/w POST 的判定是
// definitive（密钥不存在 404、不许推 403、查询出错 503），不再 fail-open。
type WHIPGrantIssuer interface {
	// IssueWHIPGrant 为一次 WHIP POST 签发通行证，返回请求头名与值；
	// offer 是完整请求体（通行证应与其绑定，防重放挪用）。
	IssueWHIPGrant(ctx context.Context, streamKey, room, username string, offer []byte) (header, value string, err error)
	// RevokeRemoteSessions 通知远端网关掐断该推流密钥名下的全部会话（尽力）。
	RevokeRemoteSessions(ctx context.Context, streamKey string) error
}

// WHIPServer 可选能力：进程内处理 WHIP 推流的 IngestProvider（ProxyUpstream 为空时
// 接入层把 /w 请求直接交给它；ProxyUpstream 非空则照常反代，实现可据配置在两者间切换）。
type WHIPServer interface {
	// ServeWHIP 处理一个 WHIP 请求（POST 建会话 / PATCH trickle / DELETE 结束）。
	// token 由接入层从路径解析（POST 另支持 Bearer 头，且已过禁言拦截）：
	// POST 时是推流密钥，PATCH/DELETE 时是 POST 应答 Location 里的会话资源 id。
	ServeWHIP(w http.ResponseWriter, r *http.Request, token string)
	// HasSession 该会话资源 id 是否归本实现。推流中途切换 ingest_provider 时，
	// 接入层据此把收尾请求路由给会话归属方而不是当前选中实现。
	HasSession(token string) bool
}
