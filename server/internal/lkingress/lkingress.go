// LiveKit Ingress 管理：走 LiveKit server 的 Twirp HTTP API 创建/删除 WHIP ingress。
// 鉴权：每个请求注入带 IngressAdmin grant 的短时效 JWT。
package lkingress

import (
	"context"
	"fmt"
	"net/http"
	"time"

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

// Create 为 username 在 room 创建 WHIP ingress（enable_transcoding=false，bypass 转码省服务端 CPU）。
// 返回 ingressID 与 streamKey。
func (c *Client) Create(ctx context.Context, room, username string) (string, string, error) {
	identity := username + "-obs"
	info, err := c.api.CreateIngress(ctx, &livekit.CreateIngressRequest{
		InputType:           livekit.IngressInput_WHIP_INPUT,
		Name:                identity,
		RoomName:            room,
		ParticipantIdentity: identity,
		ParticipantName:     username + "(OBS)",
		EnableTranscoding:   boolPtr(false),
	})
	if err != nil {
		return "", "", fmt.Errorf("创建 ingress 失败: %w", err)
	}
	return info.IngressId, info.StreamKey, nil
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
