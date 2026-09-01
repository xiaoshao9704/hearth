// LiveKit 加入令牌签发：grant 带发布/订阅/数据权限，24 小时过期。
package lktoken

import (
	"time"

	"github.com/livekit/protocol/auth"
)

// TTL 只需覆盖"拿到凭证到完成入会"的窗口：入会后连接不受过期影响，
// 断线重连由前端拿新令牌（重新经过禁言/封禁检查）。放长会让被禁言/封禁者
// 用缓存旧令牌绕过服务端权限收回。
const ttl = 10 * time.Minute

// Sign 为 username 签发到 room 的 LiveKit JWT。
// LiveKit 不允许房间内重复 identity(后者顶掉前者),identity 用 用户名-设备标签,
// 同一账号可在不同设备同时在线;同设备重复进房会顶掉旧连接(防僵尸占位)。
// 显示名(Name)仍是用户名。canPublish=false 用于被禁言用户：进房即无发布权限。
func Sign(key, secret, room, username, device string, canPublish bool) (string, error) {
	grant := &auth.VideoGrant{
		RoomJoin:       true,
		Room:           room,
		CanPublish:     boolPtr(canPublish),
		CanSubscribe:   boolPtr(true),
		CanPublishData: boolPtr(true),
	}
	at := auth.NewAccessToken(key, secret)
	at.SetVideoGrant(grant).
		SetIdentity(username + "-" + device).
		SetName(username).
		SetValidFor(ttl)
	return at.ToJWT()
}

func boolPtr(b bool) *bool { return &b }
