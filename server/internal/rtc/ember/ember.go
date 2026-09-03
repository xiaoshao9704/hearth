// Package ember 是 rtc.Provider 的进程内纯音频 SFU 实现（pion/webrtc）。
// 设计取舍：
//   - 只做 Opus 转发：无 simulcast / 关键帧 / 带宽估计，每参与者一条上行、N-1 条下行拷贝；
//   - 服务器有公网 IP：ICE-Lite + 单端口 UDP mux，客户端不需要 STUN/TURN；
//   - 信令走同源 WebSocket（api 层挂 /providers/ember/voice，鉴权与聊天 WS 同模式），协议自有；
//   - 说话检测：读 ssrc-audio-level RTP 头扩展，服务端聚合后广播 speakers。
package ember

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"hearth/server/internal/rtc"
	"hearth/server/internal/rtc/lite"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// ConfigKeys 本实现声明的配置键（命名空间 ember_*）。端口改动需重启生效。
func ConfigKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "ember_udp_port", Env: "EMBER_UDP_PORT", Group: "voice", Default: "47700",
			Label: "媒体 UDP 端口", Hint: "单端口 mux，需在防火墙/安全组放行；改动重启生效"},
		{Name: "ember_public_ip", Env: "EMBER_PUBLIC_IP", Group: "voice",
			Label: "公网 IP", Hint: "留空 = 自动宣告全部网卡地址与 STUN 探测到的公网映射；显式设置则只通告该地址（覆盖）"},
		{Name: "ember_stun_servers", Env: "EMBER_STUN_SERVERS", Group: "voice",
			Label: "STUN 服务器", Hint: "逗号分隔；探测各网卡公网映射用，留空用内置默认（不可达时改填可用地址）"},
	}
}

// ---- 信令消息 ----

type sigMsg struct {
	Type     string            `json:"type"` // C→S: offer/answer/mute；S→C: welcome/answer/offer/roster/speakers/gag/bye
	SDP      string            `json:"sdp,omitempty"`
	Mids     map[string]string `json:"mids,omitempty"` // S→C offer 附带：mid -> 对端 identity
	Identity string            `json:"identity,omitempty"`
	Self     *peerInfo         `json:"self,omitempty"` // welcome 附带：自己的名册条目（前端本地参与者据此，不解析 identity）
	Peers    []peerInfo        `json:"peers,omitempty"`
	On       bool              `json:"on"` // mute: 闭麦状态；welcome/gag: 禁言状态
	Speakers []string          `json:"speakers,omitempty"`
	Reason   string            `json:"reason,omitempty"`
}

type peerInfo struct {
	Identity string `json:"identity"`
	UID      int64  `json:"uid"` // 归属用户 id（前端据此聚合设备与发管理操作，不解析 identity）
	Name     string `json:"name"`
	Username string `json:"username"`       // 归属用户名（纯展示）
	Kind     string `json:"kind,omitempty"` // 参与者类别（ingest = 推流设备）；ember 是纯语音内核，恒为空
	Tag      string `json:"tag,omitempty"`  // 设备标签
	MicOn    bool   `json:"micOn"`
	Muted    bool   `json:"muted,omitempty"` // 服务端禁言（channel_gags）
}

// ---- 参与者与房间 ----

type participant struct {
	identity string
	uid      int64  // 归属用户 id（管理操作的目标；identity 的主体就是它）
	username string // 归属用户名（纯展示）
	tag      string // 设备标签
	name     string
	joinedAt int64

	conn     *websocket.Conn
	send     chan sigMsg
	pc       *webrtc.PeerConnection
	out      *webrtc.TrackLocalStaticRTP // 该参与者的上行音频，供他人订阅
	micOn    bool
	muted    atomic.Bool // 服务端禁言（channel_gags）：丢弃上行并锁死 mic 状态
	closed   chan struct{}
	closeOne sync.Once

	// 对每个别人 out 轨的 sender（离开时 RemoveTrack 用）；sndMu 保护。
	// 值带 owner：identity 可能因重连被新 incarnation 复用，增删前必须比对是不是同一个人，
	// 不能只按 identity 键判断——否则旧连接的收尾会把新连接刚建立的 sender 一起摘掉，
	// 或者新连接的订阅被旧连接的残留 sender 挡住（见 addPeer/dropPeer 注释）。
	sndMu   sync.Mutex
	senders map[string]*senderEntry

	negMu    sync.Mutex // 串行化重协商：同一时刻最多一个未应答 offer
	answerCh chan string

	// announce SDP 出口的宣告追加（Announcer.Announce 绑住入会 ctx）：
	// renegotiate 是 vroom 的方法，拿不到 Provider
	announce func(sdp string) string

	// 说话检测
	levelMu    sync.Mutex
	lastLoudAt time.Time
}

func (p *participant) close(reason string) {
	p.closeOne.Do(func() {
		if reason != "" {
			select {
			case p.send <- sigMsg{Type: "bye", Reason: reason}:
			default:
			}
		}
		close(p.closed)
		// 给 bye 一点送达时间再关
		go func() {
			time.Sleep(150 * time.Millisecond)
			p.conn.Close(websocket.StatusNormalClosure, reason)
			if p.pc != nil {
				p.pc.Close()
			}
		}()
	})
}

// senderEntry 是 participant.senders 的值：owner 记录这个 sender 是订阅了谁的 out 轨，
// 供 identity 复用场景下精确匹配（见 participant.senders 字段注释）。
type senderEntry struct {
	owner  *participant
	sender *webrtc.RTPSender
}

type vroom struct {
	name  string
	mu    sync.Mutex
	parts map[string]*participant

	speakers   map[string]bool
	tickerStop chan struct{}
}

// ---- Provider ----

type Provider struct {
	cfg rtc.ConfigFunc

	mu    sync.Mutex
	rooms map[string]*vroom

	// initMu 只保护 transport 懒初始化；宣告探测在 Announcer 里，不顶着 mu
	// 让首个入会把名册/人数等房间操作一起卡住
	initMu    sync.Mutex
	transport *lite.Transport
	announcer *lite.Announcer
}

// New mapped 传端口映射结果的查询函数（无映射来源传 nil），媒体端口的外部地址据此宣告。
func New(cfg rtc.ConfigFunc, mapped lite.MappedFunc) *Provider {
	p := &Provider{cfg: cfg, rooms: make(map[string]*vroom)}
	p.announcer = lite.NewAnnouncer(
		func(ctx context.Context) string { return p.cfg(ctx, "ember_public_ip") },
		func(ctx context.Context) string { return p.cfg(ctx, "ember_stun_servers") },
		mapped,
	)
	return p
}

// RefreshAnnounce 周期刷新（或端口映射变化）触发的宣告探测刷新。
func (p *Provider) RefreshAnnounce(ctx context.Context) (changed bool, externals []string, probedAt time.Time) {
	changed = p.announcer.Refresh(ctx)
	externals, probedAt = p.AnnounceSnapshot()
	return changed, externals, probedAt
}

// AnnounceSnapshot 只读当前会宣告的外部地址，给日志/管理后台回显。
func (p *Provider) AnnounceSnapshot() (externals []string, probedAt time.Time) {
	return p.announcer.Snapshot()
}

func (p *Provider) Name() string { return "ember" }

func (p *Provider) JoinCredentials(context.Context, string, rtc.Meta, bool) (rtc.Credentials, error) {
	// URL/Token 留空：api 层推导同源 /providers/ember/voice 信令地址并透传会话 token；
	// canPublish 此处在凭证层无处安放，禁言由 api 层在信令入会时（HandleJoin muted 参数）生效
	return rtc.Credentials{Engine: "ember"}, nil
}

func (p *Provider) RoomCounts(context.Context) (map[string]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.rooms))
	for name, r := range p.rooms {
		r.mu.Lock()
		if n := len(r.parts); n > 0 {
			out[name] = n
		}
		r.mu.Unlock()
	}
	return out, nil
}

func (p *Provider) ListParticipants(_ context.Context, room string) ([]rtc.Participant, error) {
	p.mu.Lock()
	r := p.rooms[room]
	p.mu.Unlock()
	if r == nil {
		return []rtc.Participant{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]rtc.Participant, 0, len(r.parts))
	for _, pt := range r.parts {
		out = append(out, rtc.Participant{Identity: pt.identity, UID: pt.uid, Username: pt.username,
			Name: pt.name, JoinedAt: pt.joinedAt, Tag: pt.tag})
	}
	return out, nil
}

func (p *Provider) RemoveParticipantsOf(_ context.Context, room string, userID int64, device string) (int, error) {
	p.mu.Lock()
	r := p.rooms[room]
	p.mu.Unlock()
	if r == nil {
		return 0, nil
	}
	r.mu.Lock()
	var targets []*participant
	for _, pt := range r.parts {
		if rtc.MatchesUser(pt.identity, userID) && (device == "" || pt.identity == device) {
			targets = append(targets, pt)
		}
	}
	r.mu.Unlock()
	for _, pt := range targets {
		pt.close("kicked")
	}
	return len(targets), nil
}

// MuteUserAudio 服务端禁言/解禁某用户全部设备：丢弃其上行音频并锁死 mic 状态。
// 契约见 rtc.Provider（禁言=禁全部媒体发布）：本内核只承载音频，丢弃全部上行即等效。
// 用户不在房间时返回 ErrNoParticipant。
func (p *Provider) MuteUserAudio(_ context.Context, room string, userID int64, muted bool) error {
	p.mu.Lock()
	r := p.rooms[room]
	p.mu.Unlock()
	if r == nil {
		return rtc.ErrNoParticipant
	}
	r.mu.Lock()
	found := false
	var targets []*participant
	for _, pt := range r.parts {
		if rtc.MatchesUser(pt.identity, userID) {
			pt.muted.Store(muted)
			if muted {
				pt.micOn = false // roster() 在 r.mu 下读
			}
			targets = append(targets, pt)
			found = true
		}
	}
	r.mu.Unlock()
	if !found {
		return rtc.ErrNoParticipant
	}
	for _, pt := range targets { // 告知被禁言者本人（前端自我提示 + 收成闭麦）
		select {
		case pt.send <- sigMsg{Type: "gag", On: muted}:
		default:
		}
	}
	r.broadcastRoster()
	return nil
}

func (p *Provider) SignalProxyUpstream(context.Context) string { return "" } // 进程内，无代理

// ---- WebRTC Transport（懒初始化，传输基建见 rtc/lite）----

// ensureTransport 失败不缓存：端口被瞬时占用时下一次入会重试，不把内核永久卡在错误态。
func (p *Provider) ensureTransport(ctx context.Context) (*lite.Transport, error) {
	p.initMu.Lock()
	defer p.initMu.Unlock()
	if p.transport != nil {
		return p.transport, nil
	}
	port, err := strconv.Atoi(p.cfg(ctx, "ember_udp_port"))
	if err != nil || port <= 0 || port > 65535 {
		port = 47700
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
	// 说话检测靠这个头扩展
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{
		URI: "urn:ietf:params:rtp-hdrext:ssrc-audio-level",
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	t, err := lite.NewTransport(port, m)
	if err != nil {
		return nil, fmt.Errorf("语音%w", err)
	}
	p.transport = t
	log.Printf("ember 就绪: udp=%d", port)
	return t, nil
}

// ---- 入会（由 api 层完成鉴权后调用，阻塞至连接结束）----

func (p *Provider) HandleJoin(ctx context.Context, roomName string, meta rtc.Meta, muted bool, conn *websocket.Conn) {
	identity, name := rtc.Identity(meta.UID, meta.Tag), meta.Username
	t, err := p.ensureTransport(ctx)
	if err != nil {
		wsjson.Write(ctx, conn, sigMsg{Type: "bye", Reason: err.Error()})
		conn.Close(websocket.StatusInternalError, "voice unavailable")
		return
	}
	// 每次入会用当时的宣告规则组装 API：规则刷新只影响新连接，在途会话不受打扰
	api, err := t.NewAPI(p.announcer.Rules(ctx))
	if err != nil {
		wsjson.Write(ctx, conn, sigMsg{Type: "bye", Reason: err.Error()})
		conn.Close(websocket.StatusInternalError, "voice unavailable")
		return
	}

	p.mu.Lock()
	r := p.rooms[roomName]
	if r == nil {
		r = &vroom{name: roomName, parts: make(map[string]*participant), speakers: make(map[string]bool), tickerStop: make(chan struct{})}
		p.rooms[roomName] = r
		go r.speakerLoop()
	}
	p.mu.Unlock()

	out, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", identity)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "track init failed")
		return
	}

	part := &participant{
		identity: identity, uid: meta.UID, username: meta.Username, tag: meta.Tag,
		name: name, joinedAt: time.Now().Unix(),
		conn: conn, send: make(chan sigMsg, 32), out: out,
		closed: make(chan struct{}), senders: make(map[string]*senderEntry),
		answerCh: make(chan string, 1),
		// 出口宣告绑在参与者上：renegotiate 是 vroom 的方法，拿不到 Provider 与入会 ctx
		announce: func(sdp string) string { return p.announcer.Announce(ctx, sdp) },
	}
	part.muted.Store(muted) // 禁言状态随入会生效（api 层据 channel_gags 判定）

	r.mu.Lock()
	if old := r.parts[identity]; old != nil {
		old.close("duplicate")
		delete(r.parts, identity)
	}
	r.parts[identity] = part
	r.mu.Unlock()

	go part.writeLoop(ctx)
	self := peerInfo{Identity: identity, UID: meta.UID, Name: name, Username: meta.Username,
		Tag: meta.Tag, MicOn: false, Muted: muted}
	// On = 自己是否被禁言
	part.send <- sigMsg{Type: "welcome", Identity: identity, Self: &self, Peers: r.roster(identity), On: muted}
	r.broadcastRoster()

	p.readLoop(ctx, api, r, part) // 阻塞

	// 收尾：identity 可能已被同 identity 的新连接顶替（重入会判重复时踢的就是这里的 part）。
	// 顶替时新 incarnation 正在走自己的入会流程，这里不能再碰它——
	// 对它 renegotiate 会跟它自己初始握手抢同一个 pc（negMu 保护的是同一把锁，但这里的 offer
	// 本身就是多余的，不该发）；dropPeer 也只应摘掉属于这一代（旧 part）的 sender。
	r.mu.Lock()
	replaced := r.parts[identity] // 顶替时是新 part；正常退出时会等于 part，随之被摘掉
	if replaced == part {
		delete(r.parts, identity)
		replaced = nil
	}
	empty := len(r.parts) == 0
	others := make([]*participant, 0, len(r.parts)) // 锁内手工收集：snapshot() 会重复抢 r.mu
	for _, o := range r.parts {
		others = append(others, o)
	}
	r.mu.Unlock()
	part.close("")
	for _, o := range others {
		if o == replaced {
			continue
		}
		o.dropPeer(part)
		r.renegotiate(o)
	}
	r.broadcastRoster()
	if empty {
		p.mu.Lock()
		if rr := p.rooms[roomName]; rr == r {
			delete(p.rooms, roomName)
			close(r.tickerStop)
		}
		p.mu.Unlock()
	}
}

func (pt *participant) writeLoop(ctx context.Context) {
	for {
		select {
		case m := <-pt.send:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := wsjson.Write(wctx, pt.conn, m)
			cancel()
			if err != nil {
				return
			}
		case <-pt.closed:
			return
		}
	}
}

func (p *Provider) readLoop(ctx context.Context, api *webrtc.API, r *vroom, part *participant) {
	for {
		var m sigMsg
		if err := wsjson.Read(ctx, part.conn, &m); err != nil {
			return
		}
		switch m.Type {
		case "offer": // 仅入会时一次：客户端带 sendonly 麦克风收发器
			if part.pc != nil {
				continue
			}
			pc, err := api.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				return
			}
			// negMu 与 renegotiate 共用：初始握手完成前，任何并发的重协商都必须排在后面，
			// 不能抢着往同一个 pc 塞 offer——那会把 signaling state 撞坏，客户端要么收到
			// 意外的 offer、要么等 answer 永远等不到（见 renegotiate 注释）。part.pc 的赋值
			// 也纳入锁内，避免 renegotiate 那头的 nil 检查与这里的赋值出现数据竞争。
			part.negMu.Lock()
			part.pc = pc
			pc.OnTrack(func(tr *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
				go part.forward(tr, recv)
			})
			pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
				if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
					part.close("")
				}
			})
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}); err != nil {
				part.negMu.Unlock()
				log.Printf("ember SetRemoteDescription: %v", err)
				return
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				part.negMu.Unlock()
				return
			}
			done := webrtc.GatheringCompletePromise(pc)
			if err := pc.SetLocalDescription(answer); err != nil {
				part.negMu.Unlock()
				return
			}
			<-done
			localSDP := pc.LocalDescription().SDP
			part.negMu.Unlock()
			part.send <- sigMsg{Type: "answer", SDP: part.announce(localSDP)}
			// 把既有参与者的音轨喂给新人，把新人的音轨喂给既有参与者
			for _, other := range r.snapshot() {
				if other == part {
					continue
				}
				part.addPeer(other)
				other.addPeer(part)
			}
			r.renegotiate(part)
			for _, other := range r.snapshot() {
				if other != part {
					r.renegotiate(other)
				}
			}
		case "answer":
			select {
			case part.answerCh <- m.SDP:
			default:
			}
		case "mute":
			r.mu.Lock()
			part.micOn = !m.On && !part.muted.Load() // 被禁言者不允许自行开麦；roster() 在 r.mu 下读
			r.mu.Unlock()
			if !m.On && part.muted.Load() {
				// 被禁言者尝试开麦：补发 gag 通知（禁言时的通知可能因 send 满被丢弃过）
				select {
				case part.send <- sigMsg{Type: "gag", On: true}:
				default:
				}
			}
			r.broadcastRoster()
		}
	}
}

// forward 上行 RTP → 自己的 out 轨（pion 对每个订阅端绑定各自 SSRC/序列透传）
func (pt *participant) forward(tr *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	// 解析协商到的 audio-level 扩展 id
	levelID := -1
	for _, ext := range recv.GetParameters().HeaderExtensions {
		if ext.URI == "urn:ietf:params:rtp-hdrext:ssrc-audio-level" {
			levelID = int(ext.ID)
		}
	}
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		if pt.muted.Load() {
			continue // 服务端禁言：直接丢弃上行，不等客户端自觉
		}
		if levelID > 0 {
			if raw := pkt.Header.GetExtension(uint8(levelID)); len(raw) > 0 {
				// level = -dBov，0 最响 127 静音；阈值放在 55dBov
				if raw[0]&0x7F < 55 {
					pt.levelMu.Lock()
					pt.lastLoudAt = time.Now()
					pt.levelMu.Unlock()
				}
			}
		}
		if err := pt.out.WriteRTP(pkt); err != nil && err != io.ErrClosedPipe {
			return
		}
	}
}

// addPeer 订阅对方的 out 轨（sender 存起来供离开时移除）。
// identity 可能因重连被复用：已有 sender 但 owner 不是这次的 other，说明是旧 incarnation
// 的残留（它的收尾还没轮到），不能当成"已订阅"直接跳过——否则新 incarnation 的下行轨永远建不起来。
func (pt *participant) addPeer(other *participant) {
	pt.sndMu.Lock()
	if pt.pc == nil {
		pt.sndMu.Unlock()
		return
	}
	old := pt.senders[other.identity]
	if old != nil {
		if old.owner == other {
			pt.sndMu.Unlock()
			return
		}
		delete(pt.senders, other.identity)
	}
	pt.sndMu.Unlock()
	if old != nil {
		pt.pc.RemoveTrack(old.sender)
	}

	sender, err := pt.pc.AddTrack(other.out)
	if err != nil {
		return
	}
	pt.sndMu.Lock()
	pt.senders[other.identity] = &senderEntry{owner: other, sender: sender}
	pt.sndMu.Unlock()
	go func() { // 排空 RTCP，释放 interceptor
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
}

// dropPeer 摘掉 owner 在自己身上的 sender；按参与者对象而不是仅按 identity 精确匹配——
// identity 可能被重连的新 incarnation 复用，按 identity 摘会连新连接刚建立的 sender 一起删掉。
func (pt *participant) dropPeer(owner *participant) {
	pt.sndMu.Lock()
	s := pt.senders[owner.identity]
	if s == nil || s.owner != owner {
		pt.sndMu.Unlock()
		return
	}
	delete(pt.senders, owner.identity)
	pt.sndMu.Unlock()
	if pt.pc != nil {
		pt.pc.RemoveTrack(s.sender)
	}
}

// renegotiate 服务端发 offer（入会后只有服务端主动协商，天然无 glare）。
// pt.pc 的读取与初始握手（"offer" 分支）里的赋值共用 negMu：新连接的初始握手
// （SetRemoteDescription→CreateAnswer→SetLocalDescription→发 answer）也在 negMu 保护下进行，
// 避免这里抢在它把 answer 发出去之前就往同一个 pc 塞 offer——那会把 pc 的 signaling state 撞坏，
// 客户端要么收到意外的 offer、要么等 answer 永远等不到。
func (r *vroom) renegotiate(pt *participant) {
	go func() {
		pt.negMu.Lock()
		defer pt.negMu.Unlock()
		if pt.pc == nil {
			return
		}
		select {
		case <-pt.closed:
			return
		default:
		}
		offer, err := pt.pc.CreateOffer(nil)
		if err != nil {
			return
		}
		done := webrtc.GatheringCompletePromise(pt.pc)
		if err := pt.pc.SetLocalDescription(offer); err != nil {
			return
		}
		<-done
		// mid → 对端 identity（客户端 ontrack 用它认人）
		mids := map[string]string{}
		pt.sndMu.Lock()
		for _, tx := range pt.pc.GetTransceivers() {
			if s := tx.Sender(); s != nil && s.Track() != nil && tx.Mid() != "" {
				for id, e := range pt.senders {
					if e.sender == s {
						mids[tx.Mid()] = id
					}
				}
			}
		}
		pt.sndMu.Unlock()
		select {
		case pt.send <- sigMsg{Type: "offer", SDP: pt.announce(pt.pc.LocalDescription().SDP), Mids: mids}:
		case <-pt.closed:
			return
		}
		select {
		case sdp := <-pt.answerCh:
			pt.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
		case <-time.After(10 * time.Second):
			// 迟迟不答不代表连接已死（可能只是后台 tab 被节流、一时丢包）：这里只是这次
			// 追加协商没追完，连接死活交给 pc.OnConnectionStateChange 判断，不在这里主动
			// close——否则会把可自愈的抖动升级成整条连接被踢、客户端重连。
		case <-pt.closed:
		}
	}()
}

// ---- 房间广播 ----

func (r *vroom) snapshot() []*participant {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*participant, 0, len(r.parts))
	for _, pt := range r.parts {
		out = append(out, pt)
	}
	return out
}

func (r *vroom) roster(exclude string) []peerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []peerInfo{}
	for _, pt := range r.parts {
		if pt.identity == exclude {
			continue
		}
		out = append(out, peerInfo{Identity: pt.identity, UID: pt.uid, Name: pt.name, Username: pt.username,
			Tag: pt.tag, MicOn: pt.micOn, Muted: pt.muted.Load()})
	}
	return out
}

func (r *vroom) broadcastRoster() {
	for _, pt := range r.snapshot() {
		select {
		case pt.send <- sigMsg{Type: "roster", Peers: r.roster(pt.identity)}:
		default:
		}
	}
}

// speakerLoop 每 250ms 聚合一次说话集合，变化才广播
func (r *vroom) speakerLoop() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.tickerStop:
			return
		case <-t.C:
			now := time.Now()
			cur := map[string]bool{}
			var list []string
			for _, pt := range r.snapshot() {
				pt.levelMu.Lock()
				loud := now.Sub(pt.lastLoudAt) < 400*time.Millisecond
				pt.levelMu.Unlock()
				if loud {
					cur[pt.identity] = true
					list = append(list, pt.identity)
				}
			}
			r.mu.Lock()
			same := len(cur) == len(r.speakers)
			if same {
				for id := range cur {
					if !r.speakers[id] {
						same = false
						break
					}
				}
			}
			r.speakers = cur
			r.mu.Unlock()
			if !same {
				if list == nil {
					list = []string{}
				}
				for _, pt := range r.snapshot() {
					select {
					case pt.send <- sigMsg{Type: "speakers", Speakers: list}:
					default:
					}
				}
			}
		}
	}
}
