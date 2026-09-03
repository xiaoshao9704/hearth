package lite

import (
	"fmt"
	"hash/fnv"
	"net/netip"
	"strconv"
	"strings"
)

// srflxPriority srflx 候选的优先级：RFC 8445 的 (2^24)*类型偏好 + (2^8)*本地偏好 + (256-组件号)，
// 类型偏好取 srflx 惯例的 100（host 是 126，所以外部候选排在 host 之后）。
const srflxPriority = 100<<24 | 65535<<8 | 255

// candLine 一行 a=candidate 里追加 srflx 所需的字段。
type candLine struct {
	foundation string
	component  string
	typ        string
	addr       netip.AddrPort
}

// srflxCand 一条待追加的 srflx 候选：ext 是对端要连的外部地址，related 是它对应的本机 host 地址。
type srflxCand struct{ ext, related netip.AddrPort }

// AppendMappedCandidate 在每个含候选的 m= 段末尾追加一条指向 ext 的 srflx 候选，
// raddr/rport 填 local。端口映射的外部端口与本地监听端口不同时用它宣告：
// pion 的地址改写规则只能改候选的 IP，改不了端口（ice 的 external_ip_mapper 只解析 IP），
// 所以这一行只作为文本插进发给对端的 SDP，不回灌给 pion——它不需要知道。
func AppendMappedCandidate(sdp string, ext, local netip.AddrPort) string {
	return appendSrflx(sdp, func([]candLine) []srflxCand {
		return []srflxCand{{ext: ext, related: local}}
	})
}

// appendSrflx 逐 m= 段调 pick 决定要追加的 srflx 候选。pick 收到该段的 component 1 host 候选
// （顺序即 SDP 中的顺序）；没有候选的 m= 段直接跳过——BUNDLE 下 pion 只把候选放进第一个
// m= 段（实测 webrtc v4.2.19：两个 transceiver 的 offer 里只有 mid:0 有 a=candidate 与
// a=end-of-candidates），逐段处理对「只有一段有候选」和「每段都有」两种形态都成立。
func appendSrflx(sdp string, pick func(hosts []candLine) []srflxCand) string {
	eol := "\n"
	if strings.Contains(sdp, "\r\n") {
		eol = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")

	var out []string
	flush := func(section []string) {
		out = append(out, sectionWithSrflx(section, pick)...)
	}
	start := 0
	for i, l := range lines {
		if strings.HasPrefix(l, "m=") && i > start {
			flush(lines[start:i])
			start = i
		}
	}
	flush(lines[start:])
	return strings.Join(out, eol)
}

// sectionWithSrflx 在一段（会话头或一个 m= 段）的最后一行候选之后插入 pick 给出的 srflx 候选。
// 候选之后紧跟的就是 a=end-of-candidates，所以「最后一行候选之后」即「end-of-candidates 之前」。
func sectionWithSrflx(section []string, pick func(hosts []candLine) []srflxCand) []string {
	var hosts []candLine
	foundations := map[string]bool{}
	last := -1
	for i, l := range section {
		c, ok := parseCandidate(l)
		if !ok {
			continue
		}
		last = i
		foundations[c.foundation] = true
		if c.typ == "host" && c.component == "1" {
			hosts = append(hosts, c)
		}
	}
	if last < 0 {
		return section
	}
	var added []string
	seen := map[netip.AddrPort]bool{}
	for _, c := range pick(hosts) {
		if !c.ext.IsValid() || !c.related.IsValid() || seen[c.ext] {
			continue
		}
		seen[c.ext] = true
		f := freeFoundation(c, foundations)
		foundations[f] = true
		added = append(added, fmt.Sprintf("a=candidate:%s 1 udp %d %s %d typ srflx raddr %s rport %d",
			f, srflxPriority, c.ext.Addr(), c.ext.Port(), c.related.Addr(), c.related.Port()))
	}
	if len(added) == 0 {
		return section
	}
	out := make([]string, 0, len(section)+len(added))
	out = append(out, section[:last+1]...)
	out = append(out, added...)
	return append(out, section[last+1:]...)
}

// freeFoundation 取一个不与该段已有候选冲突的 foundation：同一外部地址每次生成同一个值
// （对端把重协商前后的候选认作同一路径），冲突则递增。
func freeFoundation(c srflxCand, taken map[string]bool) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "srflx|%s|%s", c.ext, c.related)
	n := h.Sum32()
	for {
		f := strconv.FormatUint(uint64(n), 10)
		if !taken[f] {
			return f
		}
		n++
	}
}

// parseCandidate 解析 a=candidate 行：<foundation> <component> <transport> <priority> <ip> <port> typ <type> ...
func parseCandidate(line string) (candLine, bool) {
	rest, ok := strings.CutPrefix(line, "a=candidate:")
	if !ok {
		return candLine{}, false
	}
	f := strings.Fields(rest)
	if len(f) < 8 || f[6] != "typ" {
		return candLine{}, false
	}
	ip, err := netip.ParseAddr(f[4])
	if err != nil {
		return candLine{}, false
	}
	port, err := strconv.ParseUint(f[5], 10, 16)
	if err != nil {
		return candLine{}, false
	}
	return candLine{foundation: f[0], component: f[1], typ: f[7],
		addr: netip.AddrPortFrom(ip, uint16(port))}, true
}
