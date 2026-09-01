// LiveKit RoomService 管理：走 LiveKit server 的 Twirp HTTP API 移除房间参与者。
// 鉴权：每个请求注入带 RoomAdmin grant 的短时效 JWT（模式同 internal/lkingress）。
package lkroom

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"hearth/server/internal/rtc"
)

type Client struct {
	api    livekit.RoomService
	key    string
	secret string
	room   string // 当前操作的房间(LiveKit 要求 RoomAdmin grant 的 Room 与请求房间一致)
}

// NewClient apiURL 是 LiveKit server 的 Twirp 地址（与 Ingress 管理同一个）。
func NewClient(apiURL, key, secret string) *Client {
	c := &Client{key: key, secret: secret}
	c.api = livekit.NewRoomServiceJSONClient(apiURL, &http.Client{
		Timeout:   10 * time.Second,
		Transport: c, // 逐请求注入鉴权头
	})
	return c
}

// RoundTrip 为 Twirp 请求注入 RoomAdmin+RoomList JWT（5 分钟时效，随用随签）。
// LiveKit 校验管理员 grant 时要求 Room 与请求的房间一致，随请求体里的 room 签发。
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	at := auth.NewAccessToken(c.key, c.secret)
	at.SetVideoGrant(&auth.VideoGrant{RoomAdmin: true, RoomList: true, Room: c.room}).SetValidFor(5 * time.Minute)
	tok, err := at.ToJWT()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return http.DefaultTransport.RoundTrip(req)
}

// RoomCounts 返回当前有人的房间与在房人数（房间名 -> 人数）。
func (c *Client) RoomCounts(ctx context.Context) (map[string]int, error) {
	resp, err := c.api.ListRooms(ctx, &livekit.ListRoomsRequest{})
	if err != nil {
		return nil, fmt.Errorf("列出房间失败: %w", err)
	}
	out := make(map[string]int, len(resp.Rooms))
	for _, r := range resp.Rooms {
		out[r.Name] = int(r.NumParticipants)
	}
	return out, nil
}

// Participant 房间参与者的精简信息（identity 规则 {用户名}-{设备标签}）。
type Participant struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	JoinedAt int64  `json:"joined_at"` // Unix 秒
}

// ListParticipants 列出房间当前参与者。
func (c *Client) ListParticipants(ctx context.Context, room string) ([]Participant, error) {
	c.room = room
	defer func() { c.room = "" }()
	resp, err := c.api.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		return nil, fmt.Errorf("列出参与者失败: %w", err)
	}
	out := make([]Participant, 0, len(resp.Participants))
	for _, p := range resp.Participants {
		out = append(out, Participant{Identity: p.Identity, Name: p.Name, JoinedAt: p.JoinedAt})
	}
	return out, nil
}

// RemoveParticipantsOf 把 room 里 identity 属于 username 的参与者全部移除
// （identity 规则：{用户名}-{设备标签} 或 {用户名}-obs），返回移除数量。
func (c *Client) RemoveParticipantsOf(ctx context.Context, room, username string) (int, error) {
	c.room = room
	defer func() { c.room = "" }()
	resp, err := c.api.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		return 0, fmt.Errorf("列出参与者失败: %w", err)
	}
	n := 0
	for _, p := range resp.Participants {
		if p.Identity == username || strings.HasPrefix(p.Identity, username+"-") {
			if _, err := c.api.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
				Room:     room,
				Identity: p.Identity,
			}); err != nil {
				return n, fmt.Errorf("移除参与者 %s 失败: %w", p.Identity, err)
			}
			n++
		}
	}
	return n, nil
}

// MuteUserAudio 服务端禁言/解禁 room 里 identity 属于 username 的参与者
// （identity 规则同 RemoveParticipantsOf，对全部设备生效）。
// 通过 UpdateParticipant 改写发布权限实现（CanPublish=false）：LiveKit 服务端会下架
// 其全部已发布轨道，且客户端无法自行重新发布（区别于仅静音轨道，后者客户端可自行取消）。
// 权限是整体替换语义（见 auth.VideoGrant.UpdateFromPermission），故从参与者当前权限
// （ParticipantInfo.Permission）出发只翻转 CanPublish，避免误清 CanSubscribe/CanPublishData 等。
// 该用户没有任何参与者在房间时返回 rtc.ErrNoParticipant。
func (c *Client) MuteUserAudio(ctx context.Context, room, username string, muted bool) error {
	c.room = room
	defer func() { c.room = "" }()
	resp, err := c.api.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		return fmt.Errorf("列出参与者失败: %w", err)
	}
	found := false
	for _, p := range resp.Participants {
		if p.Identity != username && !strings.HasPrefix(p.Identity, username+"-") {
			continue
		}
		found = true
		// p.Permission 是本响应新解码的对象，原地翻转安全；
		// nil 兜底与进房令牌（lktoken.Sign）的默认授权一致。
		perm := p.Permission
		if perm == nil {
			perm = &livekit.ParticipantPermission{CanSubscribe: true, CanPublish: true, CanPublishData: true}
		}
		perm.CanPublish = !muted
		if _, err := c.api.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
			Room:       room,
			Identity:   p.Identity,
			Permission: perm,
		}); err != nil {
			return fmt.Errorf("更新参与者 %s 权限失败: %w", p.Identity, err)
		}
	}
	if !found {
		return rtc.ErrNoParticipant
	}
	return nil
}
