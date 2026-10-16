package livekitrtc

import (
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
)

// RewriteDownstream 上游 → 客户端的 binary 帧：只在 Join/Reconnect 上改 IceServers。
// 解析失败或其它消息一律原样返回——信令绝不因解析问题断连。
// 实现 rtc.SignalRewriter，接入层按接口调用，不接触 LiveKit 报文类型。
func (p *Provider) RewriteDownstream(data []byte, clientSTUN string) []byte {
	return rewriteSignalFrame(data, clientSTUN)
}

// rewriteSignalFrame 供本包单测直接调用。
func rewriteSignalFrame(data []byte, stunCfg string) []byte {
	var msg livekit.SignalResponse
	if err := proto.Unmarshal(data, &msg); err != nil {
		return data
	}
	switch {
	case msg.GetJoin() != nil:
		j := msg.GetJoin()
		j.IceServers = clientICEServers(j.GetIceServers(), stunCfg)
	case msg.GetReconnect() != nil:
		rc := msg.GetReconnect()
		rc.IceServers = clientICEServers(rc.GetIceServers(), stunCfg)
	default:
		return data
	}
	out, err := proto.Marshal(&msg)
	if err != nil {
		return data
	}
	return out
}
