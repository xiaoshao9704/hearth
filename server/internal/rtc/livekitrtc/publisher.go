// rtc.Publisher 的 LiveKit 实现：WHIP 收来的远端轨经 lksdk 以 bot 参与者直通发布进房间
// （零转码 RTP 直通）。原 rtc/bellows 内的发布代码整体搬到这里，bellows 对舞台内核中立。
package livekitrtc

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"hearth/server/internal/rtc"
)

// pubRoom 同（房间, identity）的多条轨共享的房间连接（audio+video 属于同一 bot 参与者，
// 重复 identity 连房会互踢）；refs 归零时断开。
type pubRoom struct {
	room *lksdk.Room
	refs int
	// lost 房间连接断开时回执给发布方拆会话（rtc.WithPublishLost 注入）。
	// 一个（房间, identity）至多对应一个推流会话，故只需存一份。
	lost func()
}

// PublishRemote 把一条远端轨直通发布进 LiveKit 房间。identity/name/meta 由接入层组好经
// bellows 透传（identity 见 rtc.Identity，房主侧的
// MuteUserAudio/RemoveParticipantsOf 对它天然生效）；meta 序列化为 ParticipantMetadata
// JSON 下发给观众端（见 rtc.Meta）。
// 返回的 unpublish 幂等：末条轨收回时断开房间连接。
func (p *Provider) PublishRemote(ctx context.Context, room, identity, name string, meta rtc.Meta, tr *webrtc.TrackRemote) (func(), error) {
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	key := room + "\x00" + identity

	p.pubMu.Lock()
	pr := p.pubRooms[key]
	if pr == nil {
		pr = &pubRoom{lost: rtc.PublishLost(ctx)}
		lkRoom, err := p.joinPublishRoom(room, identity, name, string(rawMeta), key, pr)
		if err != nil {
			p.pubMu.Unlock()
			return nil, err
		}
		pr.room = lkRoom
		p.pubRooms[key] = pr
	}
	pr.refs++
	p.pubMu.Unlock()

	codec := tr.Codec().RTPCodecCapability
	lt, err := lksdk.NewLocalTrack(codec, lksdk.WithRTCPHandler(func(pkt rtcp.Packet) {
		// PLI/FIR 桥接：观众关键帧请求经 SFU 到这里，回执给 WHIP 会话转成对推流端 SSRC 的 PLI；
		// NACK 各段自理（pion 侧收、lksdk 侧发，互不桥接）
		switch pkt.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			if relay := rtc.KeyframeRelay(ctx); relay != nil {
				relay(uint32(tr.SSRC()))
			}
		}
	}))
	if err != nil {
		p.releasePubRoom(key, pr)
		return nil, err
	}
	// OBS/WHIP 推流按投屏对待（非摄像头语义）：视频轨标 SCREEN_SHARE，
	// 让前端归入投屏而非摄像头分类（LIVE 角标/聚焦优先级/统计角标据此判断）
	pubOpts := lksdk.TrackPublicationOptions{}
	if tr.Kind() == webrtc.RTPCodecTypeVideo {
		pubOpts.Source = livekit.TrackSource_SCREEN_SHARE
	}
	if _, err := pr.room.LocalParticipant.PublishTrack(lt, &pubOpts); err != nil {
		p.releasePubRoom(key, pr)
		return nil, err
	}
	log.Printf("livekit 直通发布 %s 轨: identity=%s 房间=%s", codec.MimeType, identity, room)

	// 热路径复用缓冲与包结构：ReadRTP 每包新分配 MTU 切片 + Packet，8Mbps 视频约 1000 包/秒，
	// 会持续制造 GC 压力；lksdk 的 WriteRTP 同步写完即返回，不持有引用，可安全复用
	go func() {
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
	}()

	var once sync.Once
	return func() { once.Do(func() { p.releasePubRoom(key, pr) }) }, nil
}

// joinPublishRoom 连接发布房间（持 pubMu 调用；连房是低频操作，串行化防同 identity 并发重连互踢）。
func (p *Provider) joinPublishRoom(room, identity, name, meta, key string, pr *pubRoom) (*lksdk.Room, error) {
	cb := lksdk.NewRoomCallback()
	// 断线后摘除条目（仅当条目仍指向该房间），让下一次发布重新连房；同时回执给发布方拆会话——
	// 已建立的推流会话不会再产生新轨，光摘条目的话轨会一直写进死连接，
	// 表现为 OBS 显示推流正常而观众永久黑屏。回执必须在放开 pubMu 之后发（拆会话会回调 unpublish）
	cb.OnDisconnected = func() {
		p.dropPubRoom(key, pr)
		if pr.lost != nil {
			pr.lost()
		}
	}
	// livekit_api_url 原值直传：lksdk 内部会把 http(s) 规范成 ws(s)，
	// ws(s) 原样通过（signalling.ToWebsocketURL），自己转换反而破坏 wss:// 输入
	ctx := context.Background() // 房间生命周期独立于单次发布调用
	lkRoom, err := lksdk.ConnectToRoom(p.cfg(ctx, "livekit_api_url"), lksdk.ConnectInfo{
		APIKey:              p.cfg(ctx, "livekit_api_key"),
		APISecret:           p.cfg(ctx, "livekit_api_secret"),
		RoomName:            room,
		ParticipantIdentity: identity,
		ParticipantName:     name,
		ParticipantMetadata: meta,
	}, cb)
	if err != nil {
		return nil, err
	}
	return lkRoom, nil
}

// releasePubRoom 引用计数减一，归零时断房（unpublish 的唯一出口）。
func (p *Provider) releasePubRoom(key string, pr *pubRoom) {
	p.pubMu.Lock()
	pr.refs--
	last := pr.refs <= 0
	if last && p.pubRooms[key] == pr {
		delete(p.pubRooms, key)
	}
	p.pubMu.Unlock()
	if last {
		pr.room.Disconnect()
	}
}

// dropPubRoom 断线回调：条目仍指向该房间时摘除（refs 不动，unpublish 仍会 Disconnect 一次，幂等）。
func (p *Provider) dropPubRoom(key string, pr *pubRoom) {
	p.pubMu.Lock()
	if p.pubRooms[key] == pr {
		delete(p.pubRooms, key)
	}
	p.pubMu.Unlock()
}
