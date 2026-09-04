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
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4"
)

// ErrNoParticipant 目标用户在指定房间内没有参与者（服务端静音等操作的前置条件不满足）。
var ErrNoParticipant = errors.New("目标用户不在房间")

// Identity 组装内核参与者 identity：`u{user_id}` 或 `u{user_id}-{设备标签}`。
// 主体用 user_id 而非用户名是硬约束：用户名可改、改后旧名即释放、且字符集含 `-`，
// 拿它做判定键会在改名后让归属错位，也会让互为前缀的两个用户名彼此误伤
//（禁言一个掐掉另一个的推流）。用户名只经参与者元数据（Meta）下发，纯展示。
func Identity(userID int64, tag string) string {
	s := "u" + strconv.FormatInt(userID, 10)
	if tag == "" {
		return s
	}
	return s + "-" + tag
}

// MatchesUser identity 是否归属该用户（identity 约定见 Identity）。
// kick/禁言等按用户扫参与者的实现统一用它，避免各处手写主体解析漂移。
func MatchesUser(identity string, userID int64) bool {
	subject, _, _ := strings.Cut(identity, "-")
	return subject == "u"+strconv.FormatInt(userID, 10)
}

// Meta 参与者元数据，由 hearth 组好、内核原样透传给观众端。
// 身份判定一律走 identity 里的 user_id，本结构只承载展示信息与设备类别——
// 前端据此显示名字、聚合设备、识别推流设备，不再解析 identity。
type Meta struct {
	UID      int64  `json:"uid"`
	Username string `json:"username"`
	Kind     string `json:"kind,omitempty"` // ingest = 推流设备；浏览器参与者为空
	Tag      string `json:"tag,omitempty"`  // 设备标签
	Guest    bool   `json:"guest,omitempty"` // 访客（前端名册/聊天打「访客」标签）
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
	UID      int64  `json:"uid"`            // 归属用户 id（元数据透传；管理操作的目标就是它）
	Username string `json:"username"`       // 归属用户名（元数据透传，纯展示）
	Name     string `json:"name"`           // 内核侧显示名
	JoinedAt int64  `json:"joined_at"`      // Unix 秒
	Kind     string `json:"kind,omitempty"` // 参与者类别（内核元数据透传；ingest = 推流设备）
	Tag      string `json:"tag,omitempty"`  // 设备标签
	Guest    bool   `json:"guest,omitempty"` // 访客（元数据透传）
}

// Provider 语音（房间）内核：签发进房凭证与房间管理。语音槽位与舞台槽位都建立在它之上。
type Provider interface {
	Name() string
	// JoinCredentials 为用户签发进入房间的凭证：identity 由 meta 的 UID+Tag 组成
	// （Tag 区分同账号多设备），meta 原样作为参与者元数据下发。
	// canPublish=false 表示用户被禁言（channel_gags），签发无发布权限的凭证。
	JoinCredentials(ctx context.Context, room string, meta Meta, canPublish bool) (Credentials, error)
	// RoomCounts 各房间在房人数（房间名 -> 人数）；错误视为内核不可达。
	RoomCounts(ctx context.Context) (map[string]int, error)
	ListParticipants(ctx context.Context, room string) ([]Participant, error)
	// RemoveParticipantsOf 把某用户移出房间，返回移除数量。
	// device 为空 = 该用户全部设备；非空 = 只移除该 identity（仍受 userID 归属约束，
	// 传来的 identity 不属于该用户时不移除任何人）。
	RemoveParticipantsOf(ctx context.Context, room string, userID int64, device string) (int, error)
	// MuteUserAudio 服务端禁言/解禁某用户全部设备。
	// 契约：禁言 = 禁止该用户的**全部媒体发布**（音频、摄像头、投屏一并没收），
	// 不只是音频——LiveKit 实现收走 CanPublish；纯音频内核丢弃全部上行即等效。
	// 新实现必须遵循同一语义，与 joinToken 按 canPublish=false 签发的进房凭证一致。
	// 用户不在房间时返回 ErrNoParticipant。
	MuteUserAudio(ctx context.Context, room string, userID int64, muted bool) error
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
// 令牌是用户维度凭证（每用户一把，与实例、房间无关），房间在 WHIP URL 里；
// 端点方法按（identity, 标签）语义管理「用户令牌 → 实例凭证」的上游映射。
type IngestProvider interface {
	Name() string
	// Enabled 推流入口是否可用；false 时推流相关接口整体停用。
	Enabled(ctx context.Context) bool
	// ProxyUpstream 同源推流代理的上游地址；空 = 该实现不需要推流代理。
	ProxyUpstream(ctx context.Context) string
	// RevokeToken 掐断该令牌名下的全部进行会话（令牌重置时调用；幂等尽力）。
	RevokeToken(ctx context.Context, token string) error
	// EnsureEndpoint 确保该发布身份在本实例有上游端点，返回端点 id 与上游凭证
	// （反代前改写 Bearer 用）。无上游凭证概念的实现（Bellows）返回空串。
	EnsureEndpoint(ctx context.Context, identity, name string, meta Meta) (id, upstreamKey string, err error)
	// BindRoom 把端点的目标房间改为 room（livekit-ingress 的 UpdateIngress.room_name）。
	BindRoom(ctx context.Context, id, room string) error
	// DeleteEndpoint 删除端点（令牌重置/改标签时清空该令牌名下全部端点）。
	DeleteEndpoint(ctx context.Context, id string) error
}

// Publisher 是「从这里把轨发布进某个舞台内核」的**客户端**能力，由各内核的接入适配器实现：
// LiveKit 用 lksdk 走网络，进程内 Ember 直接写房间，将来的远端 Ember 走它自己的发布协议。
// 它挂在注册表的实例对象上（实例对象本来就是内核的客户端适配器），进程内只是网络距离为零的特例；
// 远端 cmd/bellows 编译进同一批实现，由 BELLOWS_SINK 选用。
// meta 会作为参与者元数据下发给观众端（见 Meta）。
type Publisher interface {
	PublishRemote(ctx context.Context, room, identity, name string, meta Meta,
		tr *webrtc.TrackRemote) (unpublish func(), err error)
}

// keyframeRelayKey WHIP 关键帧回执通道的 context 键：TrackRemote 不暴露所属连接，
// 「观众关键帧请求 → 推流端」的回执无法经 Publisher 接口参数传递，由 bellows 注入、Publisher 消费。
type keyframeRelayKey struct{}

// WithKeyframeRelay 把「请求推流端为指定 SSRC 发关键帧」的函数挂到 ctx
// （bellows 在调 PublishRemote 前注入；ssrc 取 TrackRemote.SSRC()）。
func WithKeyframeRelay(ctx context.Context, request func(ssrc uint32)) context.Context {
	return context.WithValue(ctx, keyframeRelayKey{}, request)
}

// KeyframeRelay 取 ctx 上的关键帧回执通道；无则返回 nil（Publisher 丢弃关键帧请求）。
func KeyframeRelay(ctx context.Context) func(ssrc uint32) {
	f, _ := ctx.Value(keyframeRelayKey{}).(func(ssrc uint32))
	return f
}

// publishLostKey 发布中断回执的 context 键（与 keyframeRelayKey 同因走 ctx：
// Publisher 接口只描述「发布一条轨」，容纳不下会话级事件）。
type publishLostKey struct{}

// WithPublishLost 把「发布出口已不可用」的回执挂到 ctx：Publisher 与舞台内核的连接断开后
// 无法自愈——已建立的推流会话不会再产生新轨去触发重连，轨会一直写进死连接。
// 发布方（bellows）据此拆掉推流会话，让推流端重推走完整的建会话流程。
func WithPublishLost(ctx context.Context, lost func()) context.Context {
	return context.WithValue(ctx, publishLostKey{}, lost)
}

// PublishLost 取 ctx 上的发布中断回执；无则返回 nil（Publisher 静默丢弃该事件）。
func PublishLost(ctx context.Context) func() {
	f, _ := ctx.Value(publishLostKey{}).(func())
	return f
}

// WHIPToken 解析 WHIP POST 的频道与令牌：路径 /w/{channel}（令牌在 Authorization: Bearer，
// bearer=true）或 /w/{channel}/{token}。会话收尾（/w/sessions/{rid}，非 POST）走 WHIPSessionRID。
func WHIPToken(r *http.Request) (channel, token string, bearer bool) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/w"), "/")
	channel, token, _ = strings.Cut(rest, "/")
	token, _, _ = strings.Cut(token, "/") // 防御多余路径段
	if token == "" && r.Method == http.MethodPost {
		return channel, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), true
	}
	return channel, token, false
}

// WHIPSessionRID 取 /w/sessions/{rid} 的会话资源 id（POST 应答 Location，客户端 PATCH/DELETE 打它）。
func WHIPSessionRID(r *http.Request) string {
	return strings.TrimPrefix(r.URL.Path, "/w/sessions/")
}

// WHIPGrantIssuer 可选能力：远端 WHIP 网关的通行证签发。接入层在反代前做完入场判定，
// 把结果签成短时效通行证塞进请求头，网关本地验签即可（与进房凭证同一模型：
// 接入层签、内核验）。实现该接口且 ProxyUpstream 非空时，/w POST 的判定是
// definitive（令牌不存在 404、不许推 403、查询出错 503），不再 fail-open。
type WHIPGrantIssuer interface {
	// IssueWHIPGrant 为一次 WHIP POST 签发通行证，返回请求头名与值；
	// offer 是完整请求体（通行证应与其绑定，防重放挪用）。
	// identity 与 meta 由接入层组好，网关只透传。
	IssueWHIPGrant(ctx context.Context, token, room, identity string, meta Meta, offer []byte) (header, value string, err error)
	// RevokeRemoteSessions 通知远端网关掐断该令牌名下的全部会话（尽力）。
	RevokeRemoteSessions(ctx context.Context, token string) error
}

// WHIPServer 可选能力：进程内处理 WHIP 推流的 IngestProvider（ProxyUpstream 为空时
// 接入层把 /w 请求直接交给它；ProxyUpstream 非空则照常反代，实现可据配置在两者间切换）。
type WHIPServer interface {
	// ServeWHIP 处理一个 WHIP 请求（POST 建会话 / PATCH trickle / DELETE 结束）。
	// token 由接入层解析（POST 另支持 Bearer 头）：POST 时是推流令牌（频道由实现自 URL 取），
	// PATCH/DELETE 时是 POST 应答 Location 里的会话资源 id（/w/sessions/{rid}）。
	ServeWHIP(w http.ResponseWriter, r *http.Request, token string)
	// HasSession 该会话资源 id 是否归本实现。推流中途切换 ingest_provider 时，
	// 接入层据此把收尾请求路由给会话归属方而不是当前选中实现。
	HasSession(rid string) bool
}
