package api

import (
	"slices"
	"testing"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
)

// 上游没配 stun/turn 时 LiveKit 会回落到内置的境外默认表，改写要把它剔掉；
// 含 turn 的项（含混合项）必须原样保留，凭证不能被我们碰掉。
func TestClientICEServers(t *testing.T) {
	upstream := func() []*livekit.ICEServer {
		return []*livekit.ICEServer{
			{Urls: []string{"stun:global.stun.example.net:3478", "stun:stun1.example.net:19302"}},
			{Urls: []string{"turn:turn.example.com:443?transport=tcp"}, Username: "u", Credential: "c"},
			{Urls: []string{"stun:mix.example.org:3478", "turns:mix.example.org:5349"}},
		}
	}
	cases := []struct {
		name string
		in   []*livekit.ICEServer
		cfg  string
		want [][]string
	}{
		{"留空取默认列表", upstream(), "",
			[][]string{
				{"turn:turn.example.com:443?transport=tcp"},
				{"stun:mix.example.org:3478", "turns:mix.example.org:5349"},
				{"stun:stun.miwifi.com:3478", "stun:stun.l.google.com:19302"},
			}},
		{"none 只剔除不追加", upstream(), "none",
			[][]string{
				{"turn:turn.example.com:443?transport=tcp"},
				{"stun:mix.example.org:3478", "turns:mix.example.org:5349"},
			}},
		{"自定义列表：去空项、已带前缀不重复加", upstream(), " a.example.com:3478 , ,stun:b.example.com:3478 ",
			[][]string{
				{"turn:turn.example.com:443?transport=tcp"},
				{"stun:mix.example.org:3478", "turns:mix.example.org:5349"},
				{"stun:a.example.com:3478", "stun:b.example.com:3478"},
			}},
		{"上游为空也照常追加", nil, "a.example.com:3478",
			[][]string{{"stun:a.example.com:3478"}}},
		{"上游为空且 none 则一条不下发", nil, "none", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clientICEServers(c.in, c.cfg)
			if len(got) != len(c.want) {
				t.Fatalf("项数=%d, want %d: %v", len(got), len(c.want), got)
			}
			for i := range got {
				if !slices.Equal(got[i].GetUrls(), c.want[i]) {
					t.Fatalf("第 %d 项 urls=%v, want %v", i, got[i].GetUrls(), c.want[i])
				}
			}
			// 保留下来的 turn 项凭证不得被改动
			if len(c.in) > 0 && len(got) > 0 && got[0].GetCredential() != "c" {
				t.Fatalf("turn 凭证被改动: %q", got[0].GetCredential())
			}
		})
	}
}

// 非 Join/Reconnect 的帧与非 protobuf 的字节都必须原样透传。
func TestRewriteSignalFramePassesThrough(t *testing.T) {
	if got := rewriteSignalFrame([]byte{0xff, 0xfe, 0xfd}, "none"); string(got) != "\xff\xfe\xfd" {
		t.Fatalf("非 protobuf 字节被改动: %v", got)
	}
	other, err := proto.Marshal(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{Leave: &livekit.LeaveRequest{Reason: livekit.DisconnectReason_CLIENT_INITIATED}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rewriteSignalFrame(other, "a.example.com:3478"); string(got) != string(other) {
		t.Fatalf("Leave 帧被改动")
	}
}

func TestRewriteSignalFrameRewritesJoinAndReconnect(t *testing.T) {
	def := []*livekit.ICEServer{{Urls: []string{"stun:stun.l.google.com:19302"}}}
	join, err := proto.Marshal(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{Join: &livekit.JoinResponse{IceServers: def}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := proto.Marshal(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Reconnect{Reconnect: &livekit.ReconnectResponse{IceServers: def}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"join": join, "reconnect": rec} {
		var msg livekit.SignalResponse
		if err := proto.Unmarshal(rewriteSignalFrame(raw, "a.example.com:3478"), &msg); err != nil {
			t.Fatalf("%s 改写后解析失败: %v", name, err)
		}
		servers := msg.GetJoin().GetIceServers()
		if name == "reconnect" {
			servers = msg.GetReconnect().GetIceServers()
		}
		if len(servers) != 1 || !slices.Equal(servers[0].GetUrls(), []string{"stun:a.example.com:3478"}) {
			t.Fatalf("%s 改写结果=%v", name, servers)
		}
	}
}
