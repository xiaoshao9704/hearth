// LiveKit 加入令牌签发：grant 带发布/订阅/数据权限，24 小时过期。
package lktoken

import (
	"time"

	"github.com/livekit/protocol/auth"
)

const ttl = 24 * time.Hour

// Sign 为 username 签发到 room 的 LiveKit JWT。
// LiveKit 不允许房间内重复 identity(后者顶掉前者),identity 用 用户名-设备标签,
// 同一账号可在不同设备同时在线;同设备重复进房会顶掉旧连接(防僵尸占位)。
// 显示名(Name)仍是用户名。
func Sign(key, secret, room, username, device string) (string, error) {
	grant := &auth.VideoGrant{
		RoomJoin:       true,
		Room:           room,
		CanPublish:     boolPtr(true),
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
