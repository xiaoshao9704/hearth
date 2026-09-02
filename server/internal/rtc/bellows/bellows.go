// Package bellows 是 rtc.IngestProvider 的进程内 WHIP 直通推流网关：
// OBS/ffmpeg 以 WHIP 推流（POST /w + Bearer 或 POST /w/{key}），进程内 pion
// PeerConnection 收 RTP（ICE-Lite + UDP 单端口 mux），零转码原样转发，
// 用 lksdk 以 bot 参与者（identity={user}-obs）PublishTrack 进 LiveKit 房间。
//
// 读 livekit_* 配置键是有意耦合：本实现本质是「发进 LiveKit 房间的网关」，
// 观众侧舞台内核仍是 livekitrtc，二者必须指向同一 LiveKit 部署，复用其凭证。
//
// 两种部署形态（同一个 Gateway）：
//   - 进程内：hearth 自己收流，/w 由接入层直接交给 ServeWHIP；
//   - 远端：设 bellows_remote_url 后 hearth 不收流，/w 信令反代到远端的 cmd/bellows
//     进程（通常在 LiveKit 同一局域网），媒体由推流端直达远端，视频不再经过 hearth
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

	"github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/lite"
)

// ResolveFunc 的哨兵错误：密钥不存在 → 404；
// 其余错误视为内部故障 → 503，避免瞬时 DB 抖动被误报成「密钥已重置」。
var ErrUnknownKey = errors.New("unknown stream key")

// ConfigKeys 本实现声明的配置键（命名空间 bellows_*）。端口改动需重启生效。
func ConfigKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "bellows_udp_port", Env: "BELLOWS_UDP_PORT", Group: "ingress", Default: "47710",
			Label: "WHIP 媒体 UDP 端口", Hint: "单端口 mux，需在防火墙/安全组放行；改动重启生效"},
		{Name: "bellows_public_ip", Env: "BELLOWS_PUBLIC_IP", Group: "ingress",
			Label: "WHIP 公网 IP", Hint: "留空 = 启动时 HTTP 自动探测（云主机一般留空即可）"},
		{Name: "bellows_remote_url", Env: "BELLOWS_REMOTE_URL", Group: "ingress",
			Label: "远端 Bellows 地址", Hint: "设置后本进程不收流，/w 反代到该地址的 cmd/bellows（如 http://192.168.1.20:8090）；留空 = 进程内收流"},
		{Name: "bellows_shared_secret", Env: "BELLOWS_SHARED_SECRET", Group: "ingress", Secret: true,
			Label: "远端 Bellows 共享密钥", Hint: "hearth 签发推流通行证、远端验签用的 HMAC 密钥，两边填同一值"},
	}
}

// ResolveFunc 按推流密钥反查归属（房间名 = 频道名，用户名用于 bot identity），由接入层注入，
// 仅进程内形态使用。密钥不存在须返回 ErrUnknownKey；其余错误按内部故障处理。
type ResolveFunc func(ctx context.Context, streamKey string) (room, username string, err error)

// Gateway 同时实现 rtc.IngestProvider 与 rtc.WHIPServer（进程内处理 /w 请求，不反代）；
// 远端形态下另实现 rtc.WHIPGrantIssuer（hearth 侧签发通行证、通知撤销远端会话）。
type Gateway struct {
	cfg     rtc.ConfigFunc
	resolve ResolveFunc

	// initMu 只保护 api 懒初始化：公网 IP 探测最长约 10s，不能拿 mu
	// 顶着，否则首推期间所有会话操作（DELETE/状态回调清理）全被阻塞。
	initMu sync.Mutex
	api    *webrtc.API

	mu       sync.Mutex
	sessions map[string]*session // 键 = 会话资源 id（POST 应答 Location /w/{rid}），非推流密钥
}

func New(cfg rtc.ConfigFunc, resolve ResolveFunc) *Gateway {
	return &Gateway{cfg: cfg, resolve: resolve, sessions: map[string]*session{}}
}

// NewRemote 远端形态（cmd/bellows）：不做归属反查，handlePost 只验 hearth 签发的通行证。
func NewRemote(cfg rtc.ConfigFunc) *Gateway {
	return &Gateway{cfg: cfg, sessions: map[string]*session{}}
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

// Enabled 进程内形态的前提是能连进 LiveKit 房间；远端形态只需远端地址与共享密钥
// （LiveKit 凭证在远端进程手里）。
func (g *Gateway) Enabled(ctx context.Context) bool {
	if g.remoteURL(ctx) != "" {
		return g.cfg(ctx, "bellows_shared_secret") != ""
	}
	return g.cfg(ctx, "livekit_api_key") != "" && g.cfg(ctx, "livekit_api_secret") != ""
}

// CreateEndpoint 端点是纯本地概念：密钥落库（接入层）即生效，内核侧无需预建资源。
func (g *Gateway) CreateEndpoint(context.Context, string, string) (id, streamKey string, err error) {
	key, err := randHex(16)
	if err != nil {
		return "", "", err
	}
	return key, key, nil
}

// DeleteEndpoint 密钥由接入层从库中删除（旧 key 此后反查不到归属即失效）；
// 这里只需把还在推的同 key 会话断掉。id 即 streamKey（CreateEndpoint 的约定）。
func (g *Gateway) DeleteEndpoint(_ context.Context, id string) error {
	g.closeSessionsByKey(id)
	return nil
}

// PublicBase 恒为空（同源 /w/）：纯通行证模型下 OBS 必须经 hearth 反代才有 grant，
// 直连远端的形态不存在。
func (g *Gateway) PublicBase(context.Context) string { return "" }

// ProxyUpstream 远端形态下 hearth 的 /w 反代到远端 cmd/bellows；进程内为空，接入层直接交给 ServeWHIP。
func (g *Gateway) ProxyUpstream(ctx context.Context) string { return g.remoteURL(ctx) }

// Handler 独立进程用的 /w 处理器（令牌解析 + ServeWHIP）；进程内形态由接入层做同样的事。
// DELETE /w/sessions/{key} 是 hearth 通知撤销远端会话的端点（验 revoke 通行证），
// 只服务于远端形态；会话资源 id 是随机 hex，不会与 "sessions" 前缀冲突。
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/w/sessions/") {
			g.handleRevoke(w, r, strings.TrimPrefix(r.URL.Path, "/w/sessions/"))
			return
		}
		token, _ := rtc.WHIPToken(r)
		g.ServeWHIP(w, r, token)
	})
}

// handleRevoke 验 revoke 通行证后掐断该推流密钥名下的全部会话；幂等（无会话也 204）。
func (g *Gateway) handleRevoke(w http.ResponseWriter, r *http.Request, streamKey string) {
	p, err := verifyGrant(g.cfg(r.Context(), "bellows_shared_secret"), r.Header.Get(GrantHeader))
	if err != nil || p.Op != "revoke" || p.Key != streamKey {
		http.Error(w, errInvalidGrant.Error(), http.StatusUnauthorized)
		return
	}
	g.closeSessionsByKey(streamKey)
	w.WriteHeader(http.StatusNoContent)
}

// closeSessionsByKey 摘除并关闭该推流密钥名下的所有会话。
// close 内部会再抢 mu（removeSession），必须先出锁再关。
func (g *Gateway) closeSessionsByKey(streamKey string) {
	g.mu.Lock()
	var victims []*session
	for rid, s := range g.sessions {
		if s.key == streamKey {
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

func (g *Gateway) handlePost(w http.ResponseWriter, r *http.Request, streamKey string) {
	if streamKey == "" {
		http.Error(w, "缺少推流密钥", http.StatusBadRequest)
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
	// 通行证（绑定密钥与 offer 哈希，判定已在 hearth 侧做完，这里不再问任何人）
	var room, username string
	if g.resolve != nil {
		room, username, err = g.resolve(r.Context(), streamKey)
		if errors.Is(err, ErrUnknownKey) {
			http.Error(w, "推流密钥无效或已重置", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "推流网关暂时不可用，请稍后再试", http.StatusServiceUnavailable)
			return
		}
	} else {
		p, verr := verifyGrant(g.cfg(r.Context(), "bellows_shared_secret"), r.Header.Get(GrantHeader))
		if verr != nil || p.Op != "publish" || p.Key != streamKey || p.Offer != offerHash(offer) {
			http.Error(w, errInvalidGrant.Error(), http.StatusUnauthorized)
			return
		}
		room, username = p.Room, p.User
	}
	api, err := g.ensureAPI(r.Context())
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
	s := &session{gw: g, rid: rid, key: streamKey, room: room, user: username, pc: pc}
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

	// 同 key 重推（OBS 重连）先顶掉旧会话
	g.closeSessionsByKey(streamKey)
	g.mu.Lock()
	// 状态回调注册在入表之前，极端情况下会话可能在此前已被关闭：
	// 关掉的会话不入表，避免留下无法清理的死条目
	if !s.closed.Load() {
		g.sessions[rid] = s
	}
	g.mu.Unlock()

	body := []byte(pc.LocalDescription().SDP)
	w.Header().Set("Content-Type", "application/sdp")
	// 资源地址用一次性会话 id：bearer 模式的密钥不能经 Location 回流进 URL/访问日志
	w.Header().Set("Location", "/w/"+rid)
	// ffmpeg 的 WHIP muxer 读不了 chunked 响应，必须显式 Content-Length
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusCreated)
	w.Write(body)
	log.Printf("bellows 会话建立: 用户=%s 房间=%s", username, room)
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

// ---- WebRTC API（懒初始化，传输基建见 rtc/lite）----

// ensureAPI 失败不缓存：端口被瞬时占用（如重启重叠期）下一次推流会重试，
// 而不是把网关永久卡在错误态直到进程重启。
func (g *Gateway) ensureAPI(ctx context.Context) (*webrtc.API, error) {
	g.initMu.Lock()
	defer g.initMu.Unlock()
	if g.api != nil {
		return g.api, nil
	}
	port, err := strconv.Atoi(g.cfg(ctx, "bellows_udp_port"))
	if err != nil || port <= 0 || port > 65535 {
		port = 47710
	}
	ip := g.cfg(ctx, "bellows_public_ip")
	if ip == "" {
		ip = lite.ProbePublicIP()
	}

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
	// 直通进 LiveKit 后浏览器订阅端也按 pm=1 解包，收 pm=0 只会造出解不出的流
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
	api, err := lite.NewAPI(port, ip, m)
	if err != nil {
		return nil, fmt.Errorf("WHIP %w", err)
	}
	g.api = api
	log.Printf("bellows 就绪: udp=%d 公网IP=%q", port, ip)
	return g.api, nil
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
	gw   *Gateway
	rid  string // 会话资源 id（Location /w/{rid}）
	key  string // 推流密钥
	room string // 频道名 = LiveKit 房间名
	user string
	pc   *webrtc.PeerConnection

	closed atomic.Bool // close 一开始就置位：joinRoom 据此拒绝为已关闭会话连房

	mu     sync.Mutex
	lkRoom *lksdk.Room

	closeOnce sync.Once
}

// handleTrack 首个 track 到达时懒连 LiveKit 房间，随后每条 track 直通发布。
func (s *session) handleTrack(tr *webrtc.TrackRemote) {
	room, err := s.joinRoom()
	if err != nil {
		log.Printf("bellows 进 LiveKit 房间失败: %v", err)
		s.close()
		return
	}
	codec := tr.Codec().RTPCodecCapability
	lt, err := lksdk.NewLocalTrack(codec, lksdk.WithRTCPHandler(func(pkt rtcp.Packet) {
		// PLI/FIR 桥接：观众关键帧请求经 SFU 到这里，转成对 OBS 侧 SSRC 的 PLI；
		// NACK 各段自理（pion 侧收、lksdk 侧发，互不桥接）
		switch pkt.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(tr.SSRC())}})
		}
	}))
	if err != nil {
		log.Printf("bellows 创建发布轨失败: %v", err)
		s.close()
		return
	}
	if _, err := room.LocalParticipant.PublishTrack(lt, &lksdk.TrackPublicationOptions{}); err != nil {
		log.Printf("bellows 发布 %s 轨失败: %v", codec.MimeType, err)
		s.close()
		return
	}
	log.Printf("bellows 发布 %s 轨: 用户=%s 房间=%s", codec.MimeType, s.user, s.room)
	// 热路径复用缓冲与包结构：ReadRTP 每包新分配 MTU 切片 + Packet，8Mbps 视频约 1000 包/秒，
	// 会持续制造 GC 压力；lksdk 的 WriteRTP 同步写完即返回，不持有引用，可安全复用
	buf := make([]byte, 1500)
	var pkt rtp.Packet
	for {
		n, _, err := tr.Read(buf)
		if err != nil {
			return
		}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if err := lt.WriteRTP(&pkt, nil); err != nil && err != io.ErrClosedPipe {
			return
		}
	}
}

// joinRoom 懒连接：bot 参与者 identity={user}-obs（与既有归属约定一致，
// 房主侧的 MuteUserAudio/RemoveParticipantsOf 对它天然生效）。
func (s *session) joinRoom() (*lksdk.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// close 可能赶在连房前完成（ICE 失败与首 track 竞态）：此后连上的房间
	// 没人会再断开，必须在这里拒绝，否则留下永不退房的幽灵参与者
	if s.closed.Load() {
		return nil, errors.New("会话已关闭")
	}
	if s.lkRoom != nil {
		return s.lkRoom, nil
	}
	// 会话生命周期独立于 WHIP 请求（请求上下文在应答后即取消）
	ctx := context.Background()
	cb := lksdk.NewRoomCallback()
	cb.OnDisconnected = func() { s.close() }
	// livekit_api_url 原值直传：lksdk 内部会把 http(s) 规范成 ws(s)，
	// ws(s) 原样通过（signalling.ToWebsocketURL），自己转换反而破坏 wss:// 输入
	room, err := lksdk.ConnectToRoom(s.gw.cfg(ctx, "livekit_api_url"), lksdk.ConnectInfo{
		APIKey:              s.gw.cfg(ctx, "livekit_api_key"),
		APISecret:           s.gw.cfg(ctx, "livekit_api_secret"),
		RoomName:            s.room,
		ParticipantIdentity: s.user + "-obs",
		ParticipantName:     s.user + "(OBS)",
	}, cb)
	if err != nil {
		return nil, err
	}
	s.lkRoom = room
	return room, nil
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.gw.removeSession(s)
		s.pc.Close()
		// 与 joinRoom 串行化：joinRoom 连房中则等它写完 lkRoom 再断开
		s.mu.Lock()
		room := s.lkRoom
		s.mu.Unlock()
		if room != nil {
			room.Disconnect()
		}
		log.Printf("bellows 会话结束: 用户=%s 房间=%s", s.user, s.room)
	})
}
