// LiveKit Ingress 管理：走 LiveKit server 的 Twirp HTTP API 创建/删除 WHIP ingress。
// 鉴权：每个请求注入带 IngressAdmin grant 的短时效 JWT。
package lkingress

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"hearth/server/internal/rtc"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

type Client struct {
	api    livekit.Ingress
	key    string
	secret string
}

// NewClient apiURL 是 LiveKit server 的 Twirp 地址（如 http://livekit:7880）。
func NewClient(apiURL, key, secret string) *Client {
	c := &Client{key: key, secret: secret}
	c.api = livekit.NewIngressJSONClient(apiURL, &http.Client{
		Timeout:   10 * time.Second,
		Transport: c, // 逐请求注入鉴权头
	})
	return c
}

// RoundTrip 为 Twirp 请求注入 IngressAdmin JWT（5 分钟时效，随用随签）。
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	at := auth.NewAccessToken(c.key, c.secret)
	at.SetVideoGrant(&auth.VideoGrant{IngressAdmin: true}).SetValidFor(5 * time.Minute)
	tok, err := at.ToJWT()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return http.DefaultTransport.RoundTrip(req)
}

// Create 为该发布身份创建 WHIP ingress（enable_transcoding=false，bypass 转码省服务端 CPU）。
// 房间是端点的可更新属性：创建时以 identity 占位，接入层随即用 UpdateRoom 改成真实房间。
// meta 序列化为参与者元数据 JSON（见 rtc.Meta）。返回 ingressID 与 streamKey。
func (c *Client) Create(ctx context.Context, identity, name string, meta rtc.Meta) (string, string, error) {
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		return "", "", err
	}
	info, err := c.api.CreateIngress(ctx, &livekit.CreateIngressRequest{
		InputType:           livekit.IngressInput_WHIP_INPUT,
		Name:                identity,
		RoomName:            identity, // 占位，创建后由 UpdateRoom 改写
		ParticipantIdentity: identity,
		ParticipantName:     name,
		ParticipantMetadata: string(rawMeta),
		EnableTranscoding:   boolPtr(false),
	})
	if err != nil {
		return "", "", fmt.Errorf("创建 ingress 失败: %w", err)
	}
	return info.IngressId, info.StreamKey, nil
}

// UpdateRoom 把 ingress 的目标房间改为 room（UpdateIngress.room_name；稳态推流零控制面调用，
// 只在 bound_room 与 URL 频道不一致时由接入层触发）。
func (c *Client) UpdateRoom(ctx context.Context, ingressID, room string) error {
	_, err := c.api.UpdateIngress(ctx, &livekit.UpdateIngressRequest{
		IngressId: ingressID,
		RoomName:  room,
	})
	if err != nil {
		return fmt.Errorf("更新 ingress 房间失败: %w", err)
	}
	return nil
}

// Delete 按 ingressID 删除 ingress。
func (c *Client) Delete(ctx context.Context, ingressID string) error {
	_, err := c.api.DeleteIngress(ctx, &livekit.DeleteIngressRequest{IngressId: ingressID})
	if err != nil {
		return fmt.Errorf("删除 ingress 失败: %w", err)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }
