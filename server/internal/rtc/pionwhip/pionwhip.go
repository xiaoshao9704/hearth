// Package pionwhip 是 rtc.IngestProvider 的进程内 WHIP 直通推流网关：
// OBS/ffmpeg 以 WHIP 推流（POST /w + Bearer 或 POST /w/{key}），进程内 pion
// PeerConnection 收 RTP（ICE-Lite + UDP 单端口 mux），零转码原样转发，
// 用 lksdk 以 bot 参与者（identity={user}-obs）PublishTrack 进 LiveKit 房间。
//
// 读 livekit_* 配置键是有意耦合：本实现本质是「发进 LiveKit 房间的网关」，
// 观众侧舞台内核仍是 livekitrtc，二者必须指向同一 LiveKit 部署，复用其凭证。
package pionwhip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"hearth/server/internal/rtc"
)

// ConfigKeys 本实现声明的配置键（命名空间 pionwhip_*）。端口改动需重启生效。
func ConfigKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "pionwhip_udp_port", Env: "PIONWHIP_UDP_PORT", Group: "ingress", Default: "47710",
			Label: "WHIP 媒体 UDP 端口", Hint: "单端口 mux，需在防火墙/安全组放行；改动重启生效"},
		{Name: "pionwhip_public_ip", Env: "PIONWHIP_PUBLIC_IP", Group: "ingress",
			Label: "WHIP 公网 IP", Hint: "留空 = 启动时 HTTP 自动探测（云主机一般留空即可）"},
	}
}

// ResolveFunc 按推流密钥反查归属（房间名 = 频道名，用户名用于 bot identity），由接入层注入。
type ResolveFunc func(ctx context.Context, streamKey string) (room, username string, err error)

// Gateway 同时实现 rtc.IngestProvider 与 rtc.WHIPServer（进程内处理 /w 请求，不反代）。
type Gateway struct {
	cfg     rtc.ConfigFunc
	resolve ResolveFunc

	mu       sync.Mutex
	api      *webrtc.API
	apiErr   error
	sessions map[string]*session
}

func New(cfg rtc.ConfigFunc, resolve ResolveFunc) *Gateway {
	return &Gateway{cfg: cfg, resolve: resolve, sessions: map[string]*session{}}
}

// ---- rtc.IngestProvider ----

func (g *Gateway) Name() string { return "pion" }

// Enabled 推流的前提是能连进 LiveKit 房间（发不进去就没法用）。
func (g *Gateway) Enabled(ctx context.Context) bool {
	return g.cfg(ctx, "livekit_api_key") != "" && g.cfg(ctx, "livekit_api_secret") != ""
}

// CreateEndpoint 端点是纯本地概念：密钥落库（接入层）即生效，内核侧无需预建资源。
func (g *Gateway) CreateEndpoint(context.Context, string, string) (id, streamKey string, err error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	key := hex.EncodeToString(buf)
	return key, key, nil
}

// DeleteEndpoint 密钥由接入层从库中删除（旧 key 此后反查不到归属即失效）；
// 这里只需把还在推的同 key 会话断掉。
func (g *Gateway) DeleteEndpoint(_ context.Context, id string) error {
	g.mu.Lock()
	s := g.sessions[id]
	g.mu.Unlock()
	if s != nil {
		s.close()
	}
	return nil
}

func (g *Gateway) PublicBase(context.Context) string    { return "" } // 同源 /w/
func (g *Gateway) ProxyUpstream(context.Context) string { return "" } // 进程内，无代理

// ---- rtc.WHIPServer ----

func (g *Gateway) ServeWHIP(w http.ResponseWriter, r *http.Request, streamKey string) {
	switch r.Method {
	case http.MethodPost:
		g.handlePost(w, r, streamKey)
	case http.MethodPatch:
		// trickle 空适配：answer 等 gathering complete 后已带全部候选
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		g.mu.Lock()
		s := g.sessions[streamKey]
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
	room, username, err := g.resolve(r.Context(), streamKey)
	if err != nil {
		http.Error(w, "推流密钥无效或已重置", http.StatusNotFound)
		return
	}
	api, err := g.ensureAPI(r.Context())
	if err != nil {
		http.Error(w, "推流网关不可用: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	s := &session{gw: g, key: streamKey, room: room, user: username, pc: pc}
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
	g.mu.Lock()
	if old := g.sessions[streamKey]; old != nil {
		old.close()
	}
	g.sessions[streamKey] = s
	g.mu.Unlock()

	body := []byte(pc.LocalDescription().SDP)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/w/"+streamKey)
	// ffmpeg 的 WHIP muxer 读不了 chunked 响应，必须显式 Content-Length
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusCreated)
	w.Write(body)
	log.Printf("pionwhip 会话建立: 用户=%s 房间=%s", username, room)
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

// ---- WebRTC API（懒初始化：UDP mux + ICE-Lite + 公网 IP 通告）----

func probePublicIP() string {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, u := range []string{"http://ip.3322.net", "https://api.ipify.org"} {
		if resp, err := client.Get(u); err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
			resp.Body.Close()
			ip := strings.TrimSpace(string(b))
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	return ""
}

func (g *Gateway) ensureAPI(ctx context.Context) (*webrtc.API, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.api != nil || g.apiErr != nil {
		return g.api, g.apiErr
	}
	port, err := strconv.Atoi(g.cfg(ctx, "pionwhip_udp_port"))
	if err != nil || port <= 0 || port > 65535 {
		port = 47710
	}
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		g.apiErr = fmt.Errorf("WHIP 媒体端口 %d 监听失败: %w", port, err)
		return nil, g.apiErr
	}
	ip := g.cfg(ctx, "pionwhip_public_ip")
	if ip == "" {
		ip = probePublicIP()
	}

	se := webrtc.SettingEngine{}
	se.SetLite(true) // 服务器公网直达：lite 模式由推流端做连通性检查
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpConn))
	if ip != "" {
		se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost)
	}

	videoFB := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	m := &webrtc.MediaEngine{}
	for _, c := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		}, PayloadType: 111},
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: videoFB,
		}, PayloadType: 127},
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f",
			RTCPFeedback: videoFB,
		}, PayloadType: 108},
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH265, ClockRate: 90000, RTCPFeedback: videoFB,
		}, PayloadType: 116},
		{RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeAV1, ClockRate: 90000, RTCPFeedback: videoFB,
		}, PayloadType: 45},
	} {
		kind := webrtc.RTPCodecTypeVideo
		if strings.HasPrefix(c.MimeType, "audio/") {
			kind = webrtc.RTPCodecTypeAudio
		}
		if err := m.RegisterCodec(c, kind); err != nil {
			g.apiErr = err
			return nil, err
		}
	}

	g.api = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	log.Printf("pionwhip 就绪: udp=%d 公网IP=%q", port, ip)
	return g.api, nil
}

func (g *Gateway) removeSession(s *session) {
	g.mu.Lock()
	if g.sessions[s.key] == s {
		delete(g.sessions, s.key)
	}
	g.mu.Unlock()
}

// ---- 会话 ----

type session struct {
	gw   *Gateway
	key  string
	room string // 频道名 = LiveKit 房间名
	user string
	pc   *webrtc.PeerConnection

	mu     sync.Mutex
	lkRoom *lksdk.Room

	closeOnce sync.Once
}

// handleTrack 首个 track 到达时懒连 LiveKit 房间，随后每条 track 直通发布。
func (s *session) handleTrack(tr *webrtc.TrackRemote) {
	room, err := s.joinRoom()
	if err != nil {
		log.Printf("pionwhip 进 LiveKit 房间失败: %v", err)
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
		log.Printf("pionwhip 创建发布轨失败: %v", err)
		s.close()
		return
	}
	if _, err := room.LocalParticipant.PublishTrack(lt, &lksdk.TrackPublicationOptions{}); err != nil {
		log.Printf("pionwhip 发布 %s 轨失败: %v", codec.MimeType, err)
		s.close()
		return
	}
	log.Printf("pionwhip 发布 %s 轨: 用户=%s 房间=%s", codec.MimeType, s.user, s.room)
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		if err := lt.WriteRTP(pkt, nil); err != nil && err != io.ErrClosedPipe {
			return
		}
	}
}

// joinRoom 懒连接：bot 参与者 identity={user}-obs（与既有归属约定一致，
// 房主侧的 MuteUserAudio/RemoveParticipantsOf 对它天然生效）。
func (s *session) joinRoom() (*lksdk.Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lkRoom != nil {
		return s.lkRoom, nil
	}
	// 会话生命周期独立于 WHIP 请求（请求上下文在应答后即取消）
	ctx := context.Background()
	cb := lksdk.NewRoomCallback()
	cb.OnDisconnected = func() { s.close() }
	room, err := lksdk.ConnectToRoom(wsURL(s.gw.cfg(ctx, "livekit_api_url")), lksdk.ConnectInfo{
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
		s.gw.removeSession(s)
		s.pc.Close()
		s.mu.Lock()
		room := s.lkRoom
		s.mu.Unlock()
		if room != nil {
			room.Disconnect()
		}
		log.Printf("pionwhip 会话结束: 用户=%s 房间=%s", s.user, s.room)
	})
}

// wsURL lksdk 要 ws:// 地址：livekit_api_url 是 Twirp API 的 http(s) 地址，按 scheme 对换。
func wsURL(apiURL string) string {
	u := strings.TrimSuffix(apiURL, "/")
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	return "ws://" + strings.TrimPrefix(u, "http://")
}
