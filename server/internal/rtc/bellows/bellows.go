// Package bellows 是 rtc.IngestProvider 的进程内 WHIP 直通推流网关：
// OBS/ffmpeg 以 WHIP 推流（POST /w/{channel} + Bearer 或 POST /w/{channel}/{token}），
// 进程内 pion PeerConnection 收 RTP（ICE-Lite + UDP 单端口 mux），零转码原样转发，
// 出口走 rtc.Publisher 抽象（每次发布时从 sink 取当前舞台线实例，注册表切换即生效），
// 对舞台内核中立。
//
// 两种部署形态（同一个 Gateway）：
//   - 进程内：hearth 自己收流，/w 由接入层直接交给 ServeWHIP；
//   - 远端：注册 bellows-remote 实例（bellows_remote_url）后 hearth 不收流，/w 信令反代到远端的 cmd/bellows
//     进程（通常在舞台内核同一局域网），媒体由推流端直达远端，视频不再经过 hearth
//     所在服务器。入场判定在 hearth 反代前做完并签成短时效通行证（grant）塞进请求头，
//     远端只本地验签（见 grant.go），无出站依赖；OBS 必须经 hearth 同源 /w 才有 grant。
package bellows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/lite"
)

// ResolveFunc 的哨兵错误：令牌不存在 → 404；
// 其余错误视为内部故障 → 503，避免瞬时 DB 抖动被误报成「令牌已重置」。
var ErrUnknownKey = errors.New("unknown ingest token")

// ConfigKeys 内建进程内形态的全局配置键（命名空间 bellows_*）。端口改动需重启生效。
func ConfigKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "bellows_udp_port", Env: "BELLOWS_UDP_PORT", Group: "ingress", Default: "47710",
			Label: "WHIP 媒体 UDP 端口", Hint: "单端口 mux，需在防火墙/安全组放行；改动重启生效"},
		{Name: "bellows_public_ip", Env: "BELLOWS_PUBLIC_IP", Group: "ingress",
			Label: "WHIP 公网 IP", Hint: "留空 = 自动宣告全部网卡地址与 STUN 探测到的公网映射；显式设置则只通告该地址（覆盖）"},
		{Name: "bellows_stun_servers", Env: "BELLOWS_STUN_SERVERS", Group: "ingress",
			Label: "STUN 服务器", Hint: "逗号分隔；探测各网卡公网映射用，留空用内置默认（不可达时改填可用地址）"},
	}
}

// RemoteKeys 远端形态（bellows-remote 实例）的注册表单字段，值存实例 params。
func RemoteKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "bellows_remote_url", Env: "BELLOWS_REMOTE_URL", Group: "ingress",
			Label: "远端 Bellows 地址", Hint: "本进程不收流，/w 反代到该地址的 cmd/bellows（如 http://192.168.1.20:8090）"},
		{Name: "bellows_shared_secret", Env: "BELLOWS_SHARED_SECRET", Group: "ingress", Secret: true,
			Label: "远端 Bellows 共享密钥", Hint: "hearth 签发推流通行证、远端验签用的 HMAC 密钥，两边填同一值"},
	}
}

// ResolveFunc 按推流令牌反查归属（房间名 = 频道名；identity 与 meta 由接入层组好，
// 网关只透传，不做任何身份拼装），由接入层注入，仅进程内形态使用。
// 令牌不存在须返回 ErrUnknownKey；其余错误按内部故障处理。
type ResolveFunc func(ctx context.Context, token string) (room, identity string, meta rtc.Meta, err error)

// Gateway 同时实现 rtc.IngestProvider 与 rtc.WHIPServer（进程内处理 /w 请求，不反代）；
// 远端形态下另实现 rtc.WHIPGrantIssuer（hearth 侧签发通行证、通知撤销远端会话）。
type Gateway struct {
	cfg     rtc.ConfigFunc
	resolve ResolveFunc
	// sink 取发布出口（当前舞台线实例的 rtc.Publisher），每次发布时取，注册表切换即生效；
	// 进程内形态必填，nil 或取不到 Publisher 时 Enabled=false。
	sink func(ctx context.Context) rtc.Publisher

	// initMu 只保护 transport 懒初始化；宣告探测在 Announcer 里（最长约 2s），
	// 不拿 mu 顶着，否则首推期间所有会话操作（DELETE/状态回调清理）全被阻塞。
	initMu    sync.Mutex
	transport *lite.Transport
	announcer *lite.Announcer

	mu       sync.Mutex
	sessions map[string]*session // 键 = 会话资源 id（POST 应答 Location /w/sessions/{rid}），非推流令牌
}

// New mapped 传端口映射结果的查询函数（无映射来源、或远端形态实例传 nil），
// 媒体端口的外部地址据此宣告。
func New(cfg rtc.ConfigFunc, resolve ResolveFunc, sink func(ctx context.Context) rtc.Publisher,
	mapped lite.MappedFunc) *Gateway {

	g := &Gateway{cfg: cfg, resolve: resolve, sink: sink, sessions: map[string]*session{}}
	g.announcer = lite.NewAnnouncer(
		func(ctx context.Context) string { return g.cfg(ctx, "bellows_public_ip") },
		func(ctx context.Context) string { return g.cfg(ctx, "bellows_stun_servers") },
		mapped,
	)
	return g
}

// NewRemote 远端形态（cmd/bellows）：不做归属反查，handlePost 只验 hearth 签发的通行证；
// sink 编译时选定（BELLOWS_SINK），取不到 Publisher 时推流拒绝。
func NewRemote(cfg rtc.ConfigFunc, sink func(ctx context.Context) rtc.Publisher,
	mapped lite.MappedFunc) *Gateway {

	g := &Gateway{cfg: cfg, sink: sink, sessions: map[string]*session{}}
	g.announcer = lite.NewAnnouncer(
		func(ctx context.Context) string { return g.cfg(ctx, "bellows_public_ip") },
		func(ctx context.Context) string { return g.cfg(ctx, "bellows_stun_servers") },
		mapped,
	)
	return g
}

// RefreshAnnounce 周期刷新（或端口映射变化）触发的宣告探测刷新。
// 远端形态（hearth 侧的 bellows-remote
// 实例）不收流，宣告是远端进程自己的事，这里直接 no-op。
func (g *Gateway) RefreshAnnounce(ctx context.Context) (changed bool, externals []string, probedAt time.Time) {
	if g.remoteURL(ctx) != "" {
		return false, nil, time.Time{}
	}
	changed = g.announcer.Refresh(ctx)
	externals, probedAt = g.announceSnapshot()
	return changed, externals, probedAt
}

// AnnounceSnapshot 只读当前会宣告的外部地址，给日志/管理后台回显。
func (g *Gateway) AnnounceSnapshot() (externals []string, probedAt time.Time) {
	return g.announceSnapshot()
}

func (g *Gateway) announceSnapshot() ([]string, time.Time) {
	return g.announcer.Snapshot()
}

func randHex(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ---- rtc.IngestProvider ----

func (g *Gateway) Name() string { return "bellows" }

// remoteURL 远端 cmd/bellows 的基地址；空 = 进程内收流。
func (g *Gateway) remoteURL(ctx context.Context) string {
	return strings.TrimSuffix(strings.TrimSpace(g.cfg(ctx, "bellows_remote_url")), "/")
}

// Enabled 进程内形态的前提是发布出口可用（sink 取得到 Publisher）；远端形态只需远端地址
// 与共享密钥（出口在远端进程手里）。
func (g *Gateway) Enabled(ctx context.Context) bool {
	if g.remoteURL(ctx) != "" {
		return g.cfg(ctx, "bellows_shared_secret") != ""
	}
	return g.sink != nil && g.sink(ctx) != nil
}

// RevokeToken 掐断该令牌名下的全部进行会话（令牌重置时调用；幂等）。
func (g *Gateway) RevokeToken(_ context.Context, token string) error {
	g.closeSessionsByToken(token)
	return nil
}

// EnsureEndpoint Bellows 的实例凭证就是通行证，无上游端点要建（livekit-ingress 才有）。
func (g *Gateway) EnsureEndpoint(context.Context, string, string, rtc.Meta) (id, upstreamKey string, err error) {
	return "", "", nil
}

// BindRoom 无上游端点，空操作。
func (g *Gateway) BindRoom(context.Context, string, string) error { return nil }

// DeleteEndpoint 无上游端点，空操作（进行会话的掐断走 RevokeToken）。
func (g *Gateway) DeleteEndpoint(context.Context, string) error { return nil }

// ProxyUpstream 远端形态下 hearth 的 /w 反代到远端 cmd/bellows；进程内为空，接入层直接交给 ServeWHIP。
func (g *Gateway) ProxyUpstream(ctx context.Context) string { return g.remoteURL(ctx) }

// Handler 独立进程用的 /w 处理器（令牌解析 + ServeWHIP）；进程内形态由接入层做同样的事。
// DELETE /w/revoke/{token} 是 hearth 通知撤销远端会话的端点（验 revoke 通行证），
// 只服务于远端形态；"revoke"/"sessions" 是保留频道名（接入层拒绝创建），不会与真频道冲突。
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/w/revoke/") {
			g.handleRevoke(w, r, strings.TrimPrefix(r.URL.Path, "/w/revoke/"))
			return
		}
		if r.Method == http.MethodPost {
			_, token, _ := rtc.WHIPToken(r)
			g.ServeWHIP(w, r, token)
			return
		}
		g.ServeWHIP(w, r, rtc.WHIPSessionRID(r))
	})
}

// handleRevoke 验 revoke 通行证后掐断该令牌名下的全部会话；幂等（无会话也 204）。
func (g *Gateway) handleRevoke(w http.ResponseWriter, r *http.Request, token string) {
	p, err := verifyGrant(g.cfg(r.Context(), "bellows_shared_secret"), r.Header.Get(GrantHeader))
	if err != nil || p.Op != "revoke" || p.Token != token {
		http.Error(w, errInvalidGrant.Error(), http.StatusUnauthorized)
		return
	}
	g.closeSessionsByToken(token)
	w.WriteHeader(http.StatusNoContent)
}

// closeSessionsByToken 摘除并关闭该令牌名下的所有会话（同一令牌新 POST 顶掉旧会话：
// 一台设备同时只推一个房间）。
// close 内部会再抢 mu（removeSession），必须先出锁再关。
func (g *Gateway) closeSessionsByToken(token string) {
	g.mu.Lock()
	var victims []*session
	for rid, s := range g.sessions {
		if s.token == token {
			delete(g.sessions, rid)
			victims = append(victims, s)
		}
	}
	g.mu.Unlock()
	for _, s := range victims {
		s.close()
	}
}

// ---- rtc.WHIPServer ----

// HasSession 该资源 id 是否是本网关的活动会话（接入层据此把 PATCH/DELETE 路由给会话归属方）。
func (g *Gateway) HasSession(rid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessions[rid] != nil
}

func (g *Gateway) ServeWHIP(w http.ResponseWriter, r *http.Request, token string) {
	switch r.Method {
	case http.MethodPost:
		g.handlePost(w, r, token)
	case http.MethodPatch:
		// trickle 空适配：answer 等 gathering complete 后已带全部候选
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		// token 是 POST 应答 Location 里的会话资源 id
		g.mu.Lock()
		s := g.sessions[token]
		if s != nil {
			delete(g.sessions, token)
		}
		g.mu.Unlock()
		if s != nil {
			s.close()
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (g *Gateway) handlePost(w http.ResponseWriter, r *http.Request, token string) {
	if token == "" {
		http.Error(w, "缺少推流令牌", http.StatusBadRequest)
		return
	}
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		http.Error(w, "读取 SDP offer 失败", http.StatusBadRequest)
		return
	}
	// 白名单校验：pion 对不匹配的 m-line 只会静默回绝（answer 端口 0），
	// 会把 VP8 视频流降级成「纯音频会话」而不报错，所以在这里显式拒绝
	if !offerWithinWhitelist(string(offer)) {
		http.Error(w, "编码不受支持（仅 H264/H265/AV1 视频与 Opus 音频）", http.StatusBadRequest)
		return
	}
	// 归属来源二选一：进程内形态反查接入层注入的 resolve；远端形态验 hearth 签发的
	// 通行证（绑定令牌与 offer 哈希，判定已在 hearth 侧做完，这里不再问任何人）
	var room, identity string
	var meta rtc.Meta
	if g.resolve != nil {
		room, identity, meta, err = g.resolve(r.Context(), token)
		if errors.Is(err, ErrUnknownKey) {
			http.Error(w, "推流令牌无效或已重置", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "推流网关暂时不可用，请稍后再试", http.StatusServiceUnavailable)
			return
		}
	} else {
		p, verr := verifyGrant(g.cfg(r.Context(), "bellows_shared_secret"), r.Header.Get(GrantHeader))
		if verr != nil || p.Op != "publish" || p.Token != token || p.Identity == "" || p.Offer != offerHash(offer) {
			http.Error(w, errInvalidGrant.Error(), http.StatusUnauthorized)
			return
		}
		room, identity, meta = p.Room, p.Identity, p.Meta
	}
	pub := g.publishSink(r.Context())
	if pub == nil {
		http.Error(w, "推流出口不可用（舞台内核未配置发布能力）", http.StatusServiceUnavailable)
		return
	}
	t, err := g.ensureTransport(r.Context())
	if err != nil {
		http.Error(w, "推流网关不可用: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// 每个会话用当时的宣告规则组装 API：规则刷新只影响新会话，在途会话不受打扰
	api, err := t.NewAPI(g.announcer.Rules(r.Context()))
	if err != nil {
		http.Error(w, "推流网关不可用: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	rid, err := randHex(16)
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	s := &session{gw: g, rid: rid, token: token, room: room, identity: identity, meta: meta, pc: pc}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go s.handleTrack(tr)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.close()
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: string(offer),
	}); err != nil {
		pc.Close()
		http.Error(w, "SDP 协商失败（仅支持 H264/H265/AV1 视频与 Opus 音频）: "+err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	<-done // ICE-Lite + 单端口 mux 只有 host 候选，gathering 立即完成

	// 同令牌重推（OBS 重连/换房间）先顶掉旧会话
	g.closeSessionsByToken(token)
	g.mu.Lock()
	// 状态回调注册在入表之前，极端情况下会话可能在此前已被关闭：
	// 关掉的会话不入表，避免留下无法清理的死条目
	if !s.closed.Load() {
		g.sessions[rid] = s
	}
	g.mu.Unlock()

	body := []byte(g.announcer.Announce(r.Context(), pc.LocalDescription().SDP))
	w.Header().Set("Content-Type", "application/sdp")
	// 资源地址用一次性会话 id：bearer 模式的令牌不能经 Location 回流进 URL/访问日志
	w.Header().Set("Location", "/w/sessions/"+rid)
	// ffmpeg 的 WHIP muxer 读不了 chunked 响应，必须显式 Content-Length
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusCreated)
	w.Write(body)
	log.Printf("bellows 会话建立: identity=%s 房间=%s", identity, room)
}

// publishSink 取当前发布出口；sink 未注入或取不到 Publisher 时返回 nil。
func (g *Gateway) publishSink(ctx context.Context) rtc.Publisher {
	if g.sink == nil {
		return nil
	}
	return g.sink(ctx)
}

// offerWithinWhitelist offer 里每条启用的音频/视频 m-line 都必须至少有一个白名单编码
// （opus / h264 / h265 / av1）；解析失败时放行，交给后续协商统一报错。
func offerWithinWhitelist(offer string) bool {
	whitelist := map[string]bool{"OPUS": true, "H264": true, "H265": true, "AV1": true}
	sd := &sdp.SessionDescription{}
	if err := sd.Unmarshal([]byte(offer)); err != nil {
		return true
	}
	for _, md := range sd.MediaDescriptions {
		if md.MediaName.Port.Value == 0 {
			continue
		}
		if media := md.MediaName.Media; media != "audio" && media != "video" {
			continue
		}
		ok := false
		for _, a := range md.Attributes {
			if a.Key != "rtpmap" {
				continue
			}
			// 形如 "96 H264/90000"
			if f := strings.Fields(a.Value); len(f) == 2 {
				if name, _, _ := strings.Cut(f[1], "/"); whitelist[strings.ToUpper(name)] {
					ok = true
					break
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// ---- WebRTC Transport（懒初始化，传输基建见 rtc/lite）----

// ensureTransport 失败不缓存：端口被瞬时占用（如重启重叠期）下一次推流会重试，
// 而不是把网关永久卡在错误态直到进程重启。
func (g *Gateway) ensureTransport(ctx context.Context) (*lite.Transport, error) {
	g.initMu.Lock()
	defer g.initMu.Unlock()
	if g.transport != nil {
		return g.transport, nil
	}
	port, err := strconv.Atoi(g.cfg(ctx, "bellows_udp_port"))
	if err != nil || port <= 0 || port > 65535 {
		port = 47710
	}
	m, err := whipMediaEngine()
	if err != nil {
		return nil, err
	}
	t, err := lite.NewTransport(port, m)
	if err != nil {
		return nil, fmt.Errorf("WHIP %w", err)
	}
	g.transport = t
	log.Printf("bellows 就绪: udp=%d", port)
	return t, nil
}

func whipMediaEngine() (*webrtc.MediaEngine, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	// 视频只收 packetization-mode=1 的 H264：OBS/ffmpeg 都发 pm=1，
	// 直通进舞台内核后浏览器订阅端也按 pm=1 解包，收 pm=0 只会造出解不出的流
	videoFB := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	for _, c := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: videoFB,
		}, PayloadType: 127},
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH265, ClockRate: 90000, RTCPFeedback: videoFB,
		}, PayloadType: 116},
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeAV1, ClockRate: 90000, RTCPFeedback: videoFB,
		}, PayloadType: 45},
	} {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (g *Gateway) removeSession(s *session) {
	g.mu.Lock()
	if g.sessions[s.rid] == s {
		delete(g.sessions, s.rid)
	}
	g.mu.Unlock()
}

// ---- 会话 ----

type session struct {
	gw       *Gateway
	rid      string   // 会话资源 id（Location /w/sessions/{rid}）
	token    string   // 推流令牌（每用户一把）
	room     string   // 频道名 = 舞台内核房间名
	identity string   // 发布参与者 identity（接入层组好，见 rtc.Identity）
	meta     rtc.Meta // 参与者元数据（接入层组好，网关只透传）
	pc       *webrtc.PeerConnection

	closed atomic.Bool // close 一开始就置位：handleTrack 据此拒绝为已关闭会话发布

	mu          sync.Mutex
	unpublishes []func() // 各轨的 unpublish（close 时逐个调用）

	closeOnce sync.Once
}

// handleTrack 每条到达的轨交给 Publisher 直通发布（track 转换与读取循环在 Publisher 内部，
// 这里只透传 *webrtc.TrackRemote 与参与者身份）。
func (s *session) handleTrack(tr *webrtc.TrackRemote) {
	// close 可能赶在发布前完成（ICE 失败与首 track 竞态）：此后发布的轨
	// 没人会再收回，必须在这里拒绝，否则留下永不退房的幽灵参与者
	if s.closed.Load() {
		return
	}
	pub := s.gw.publishSink(context.Background())
	if pub == nil {
		log.Printf("bellows 推流出口不可用: identity=%s 房间=%s", s.identity, s.room)
		s.close()
		return
	}
	// 会话生命周期独立于 WHIP 请求（请求上下文在应答后即取消）；
	// 关键帧回执通道经 ctx 注入（TrackRemote 不暴露所属连接，无法走接口参数）
	ctx := rtc.WithKeyframeRelay(context.Background(), func(ssrc uint32) {
		// 观众关键帧请求（PLI/FIR）经 Publisher 到这里，转成对推流端 SSRC 的 PLI
		s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
	})
	// 发布出口断了就拆会话：推流端据此重推（重推会走完整的建会话流程，重新连房）。
	// 不拆的话轨只会一直写进死连接，永远等不到自愈
	ctx = rtc.WithPublishLost(ctx, s.close)
	unpublish, err := pub.PublishRemote(ctx, s.room, s.identity, s.meta.Username, s.meta, tr)
	if err != nil {
		log.Printf("bellows 发布轨失败: identity=%s 房间=%s: %v", s.identity, s.room, err)
		s.close()
		return
	}
	s.mu.Lock()
	// 与 close 串行化：发布完成前会话已关闭则立即收回，不入列表
	if s.closed.Load() {
		s.mu.Unlock()
		unpublish()
		return
	}
	s.unpublishes = append(s.unpublishes, unpublish)
	s.mu.Unlock()
	log.Printf("bellows 发布轨: identity=%s 房间=%s", s.identity, s.room)
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.gw.removeSession(s)
		s.pc.Close()
		s.mu.Lock()
		unpublishes := s.unpublishes
		s.unpublishes = nil
		s.mu.Unlock()
		for _, unpublish := range unpublishes {
			unpublish()
		}
		log.Printf("bellows 会话结束: identity=%s 房间=%s", s.identity, s.room)
	})
}
