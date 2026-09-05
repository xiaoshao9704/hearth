package lktoken

import (
	"testing"

	"github.com/livekit/protocol/auth"

	"hearth/server/internal/rtc"
)

// 禁言 = 禁全部发布，数据通道也在内：聊天与文件走数据通道，只掐媒体等于没禁。
func TestSignDataPublishFollowsCanPublish(t *testing.T) {
	const key, secret = "devkey", "devsecretdevsecretdevsecret"
	meta := rtc.Meta{UID: 7, Username: "alice", Tag: "web"}

	for _, tc := range []struct{ canPublish bool }{{true}, {false}} {
		jwt, err := Sign(key, secret, "general", meta, tc.canPublish)
		if err != nil {
			t.Fatalf("签发失败: %v", err)
		}
		v, err := auth.ParseAPIToken(jwt)
		if err != nil {
			t.Fatalf("解析令牌失败: %v", err)
		}
		_, grants, err := v.Verify(secret)
		if err != nil {
			t.Fatalf("校验令牌失败: %v", err)
		}
		g := grants.Video
		if g.CanPublish == nil || *g.CanPublish != tc.canPublish {
			t.Fatalf("canPublish=%t 时 CanPublish 不符: %+v", tc.canPublish, g.CanPublish)
		}
		if g.CanPublishData == nil || *g.CanPublishData != tc.canPublish {
			t.Fatalf("canPublish=%t 时 CanPublishData 应同源: %+v", tc.canPublish, g.CanPublishData)
		}
		if g.CanSubscribe == nil || !*g.CanSubscribe {
			t.Fatalf("被禁言也应能订阅: %+v", g.CanSubscribe)
		}
	}
}
