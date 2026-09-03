package portmap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeGW 回环上的假网关：记录收到的请求，按用例回固定字节（返回 nil 表示不应答）。
type fakeGW struct {
	conn *net.UDPConn
	mu   sync.Mutex
	reqs [][]byte
}

func startFakeGW(t *testing.T, respond func(req []byte) []byte) *fakeGW {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听假网关失败: %v", err)
	}
	g := &fakeGW{conn: conn}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := bytes.Clone(buf[:n])
			g.mu.Lock()
			g.reqs = append(g.reqs, req)
			g.mu.Unlock()
			if resp := respond(req); resp != nil {
				conn.WriteToUDP(resp, from)
			}
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return g
}

func (g *fakeGW) addr() netip.AddrPort {
	ap := g.conn.LocalAddr().(*net.UDPAddr).AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func (g *fakeGW) requests() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]byte(nil), g.reqs...)
}

// pcpMapResp 按请求拼一条 PCP MAP 响应（回显 nonce / 协议 / 内部端口）。
func pcpMapResp(req []byte, result byte, lifetime uint32, assigned uint16, ext string) []byte {
	resp := make([]byte, 60)
	resp[0] = pcpVersion
	resp[1] = pcpRespBit | pcpOpcodeMap
	resp[3] = result
	binary.BigEndian.PutUint32(resp[4:8], lifetime)
	binary.BigEndian.PutUint32(resp[8:12], 4242) // epoch
	copy(resp[24:36], req[24:36])
	resp[36] = req[36]
	copy(resp[40:42], req[40:42])
	binary.BigEndian.PutUint16(resp[42:44], assigned)
	ip := netip.MustParseAddr(ext).As16()
	copy(resp[44:60], ip[:])
	return resp
}

func natpmpMapResp(req []byte, result uint16, assigned uint16, lifetime uint32) []byte {
	resp := make([]byte, 16)
	resp[0] = natpmpVer
	resp[1] = pcpRespBit | req[1]
	binary.BigEndian.PutUint16(resp[2:4], result)
	copy(resp[8:10], req[4:6])
	binary.BigEndian.PutUint16(resp[10:12], assigned)
	binary.BigEndian.PutUint32(resp[12:16], lifetime)
	return resp
}

func natpmpExternalResp(ext string) []byte {
	resp := make([]byte, 12)
	resp[0] = natpmpVer
	resp[1] = pcpRespBit | natpmpOpExternal
	ip := netip.MustParseAddr(ext).As4()
	copy(resp[8:12], ip[:])
	return resp
}

var udpWant = Want{Proto: "udp", Port: 47700, Desc: "test"}

func TestPCPMapEncodesRequestAndParsesResponse(t *testing.T) {
	gw := startFakeGW(t, func(req []byte) []byte {
		return pcpMapResp(req, 0, 3600, 47700, "198.51.100.9")
	})
	c := newPCPClient(gw.addr())

	mp, err := c.Map(context.Background(), udpWant, udpWant.Port, time.Hour)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if mp.External != 47700 || mp.ExternalIP.String() != "198.51.100.9" || mp.Method != "pcp" {
		t.Fatalf("映射结果不对: %+v", mp)
	}
	if mp.ExpiresAt.IsZero() {
		t.Fatal("应按网关授予的租期算出 ExpiresAt")
	}
	if c.Method() != "pcp" {
		t.Fatalf("Method = %q", c.Method())
	}

	reqs := gw.requests()
	if len(reqs) != 1 {
		t.Fatalf("请求数 = %d", len(reqs))
	}
	req := reqs[0]
	if len(req) != 60 {
		t.Fatalf("PCP MAP 请求应为 60 字节，实际 %d", len(req))
	}
	if req[0] != pcpVersion || req[1] != pcpOpcodeMap {
		t.Fatalf("版本/opcode = %d/%d", req[0], req[1])
	}
	if got := binary.BigEndian.Uint32(req[4:8]); got != 3600 {
		t.Fatalf("请求租期 = %d", got)
	}
	// 头里的 client IP 必须是 socket 的真实源地址（这里是回环）。
	if got := netip.AddrFrom16([16]byte(req[8:24])).Unmap(); !got.IsLoopback() {
		t.Fatalf("client IP = %v，应为 socket 源地址", got)
	}
	if req[36] != 17 {
		t.Fatalf("协议号 = %d，udp 应为 17", req[36])
	}
	if got := binary.BigEndian.Uint16(req[40:42]); got != 47700 {
		t.Fatalf("内部端口 = %d", got)
	}
	if got := binary.BigEndian.Uint16(req[42:44]); got != 47700 {
		t.Fatalf("建议外部端口 = %d，应优先请求同端口", got)
	}
}

func TestPCPAcceptsReassignedPort(t *testing.T) {
	gw := startFakeGW(t, func(req []byte) []byte {
		return pcpMapResp(req, 0, 1800, 51000, "198.51.100.9")
	})
	c := newPCPClient(gw.addr())

	mp, err := c.Map(context.Background(), udpWant, 0, time.Hour)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if mp.External != 51000 {
		t.Fatalf("应以网关改派的端口为准，得到 %d", mp.External)
	}
}

func TestPCPFallsBackToNATPMP(t *testing.T) {
	gw := startFakeGW(t, func(req []byte) []byte {
		if req[0] == pcpVersion {
			// 只支持 NAT-PMP 的网关用 v0 报文回 UNSUPP_VERSION。
			resp := make([]byte, 8)
			resp[1] = pcpRespBit
			binary.BigEndian.PutUint16(resp[2:4], 1)
			return resp
		}
		if req[1] == natpmpOpExternal {
			return natpmpExternalResp("198.51.100.9")
		}
		return natpmpMapResp(req, 0, 47700, 3600)
	})
	c := newPCPClient(gw.addr())

	mp, err := c.Map(context.Background(), udpWant, udpWant.Port, time.Hour)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if mp.Method != "natpmp" || mp.External != 47700 || mp.ExternalIP.String() != "198.51.100.9" {
		t.Fatalf("回落结果不对: %+v", mp)
	}
	if c.Method() != "natpmp" {
		t.Fatalf("Method = %q，回落后应为 natpmp", c.Method())
	}

	// 回落是永久的：第二次不该再发 PCP 报文。
	if _, err := c.Map(context.Background(), udpWant, udpWant.Port, time.Hour); err != nil {
		t.Fatalf("第二次 Map: %v", err)
	}
	var pcpReqs int
	for _, req := range gw.requests() {
		if req[0] == pcpVersion {
			pcpReqs++
		}
	}
	if pcpReqs != 1 {
		t.Fatalf("PCP 请求发了 %d 次，回落后不应再试 PCP", pcpReqs)
	}
}

func TestPCPResultCodes(t *testing.T) {
	cases := []struct {
		code byte
		want error
	}{
		{pcpResultNotAuthorized, ErrNotAuthorized},
		{pcpResultNoResources, ErrNoResources},
	}
	for _, tc := range cases {
		gw := startFakeGW(t, func(req []byte) []byte {
			return pcpMapResp(req, tc.code, 0, 0, "0.0.0.0")
		})
		_, err := newPCPClient(gw.addr()).Map(context.Background(), udpWant, udpWant.Port, time.Hour)
		if !errors.Is(err, tc.want) {
			t.Fatalf("结果码 %d → %v，期望 %v", tc.code, err, tc.want)
		}
	}

	// 其余非零码是普通错误，不该被当成哨兵之一（否则 Mapper 的处置会走偏）。
	gw := startFakeGW(t, func(req []byte) []byte { return pcpMapResp(req, 3, 0, 0, "0.0.0.0") })
	_, err := newPCPClient(gw.addr()).Map(context.Background(), udpWant, udpWant.Port, time.Hour)
	if err == nil || errors.Is(err, ErrNotAuthorized) || errors.Is(err, ErrNoResources) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("MALFORMED_REQUEST 应是普通错误，得到 %v", err)
	}
}

func TestNATPMPResultCodes(t *testing.T) {
	if err := natpmpResult(natpmpResultNotAuthorized); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("natpmp 2 → %v", err)
	}
	if err := natpmpResult(natpmpResultNoResources); !errors.Is(err, ErrNoResources) {
		t.Fatalf("natpmp 4 → %v", err)
	}
	if err := natpmpResult(0); err != nil {
		t.Fatalf("natpmp 0 → %v", err)
	}
}

func TestPCPUnmapReusesNonceAndZeroLifetime(t *testing.T) {
	gw := startFakeGW(t, func(req []byte) []byte {
		return pcpMapResp(req, 0, binary.BigEndian.Uint32(req[4:8]), binary.BigEndian.Uint16(req[42:44]), "198.51.100.9")
	})
	c := newPCPClient(gw.addr())
	ctx := context.Background()

	if _, err := c.Map(ctx, udpWant, udpWant.Port, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}
	// 续租是同一条映射的幂等重发，nonce 必须一致。
	if _, err := c.Map(ctx, udpWant, udpWant.Port, time.Hour); err != nil {
		t.Fatalf("续租: %v", err)
	}
	if err := c.Unmap(ctx, udpWant, udpWant.Port); err != nil {
		t.Fatalf("Unmap: %v", err)
	}

	reqs := gw.requests()
	if len(reqs) != 3 {
		t.Fatalf("请求数 = %d", len(reqs))
	}
	nonce := reqs[0][24:36]
	for i, req := range reqs[1:] {
		if !bytes.Equal(nonce, req[24:36]) {
			t.Fatalf("第 %d 个请求的 nonce 变了，续租/删除认领不到同一条映射", i+2)
		}
	}
	del := reqs[2]
	if got := binary.BigEndian.Uint32(del[4:8]); got != 0 {
		t.Fatalf("删除报文 lifetime = %d，应为 0", got)
	}
	if got := binary.BigEndian.Uint16(del[42:44]); got != 0 {
		t.Fatalf("删除报文的建议外部端口 = %d，应为 0", got)
	}
}

func TestPCPTimeoutIsUnsupported(t *testing.T) {
	gw := startFakeGW(t, func([]byte) []byte { return nil })
	_, err := newPCPClient(gw.addr()).Map(context.Background(), udpWant, udpWant.Port, time.Hour)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("网关不响应应返回 ErrUnsupported，得到 %v", err)
	}
	if n := len(gw.requests()); n != 3 {
		t.Fatalf("重试次数 = %d，应为 3", n)
	}
}

func TestPCPIgnoresUnrelatedResponses(t *testing.T) {
	// 网关先回一包对不上号的（别的 nonce），客户端应丢弃并继续等本轮的正确应答。
	var once sync.Once
	gw := startFakeGW(t, func(req []byte) []byte {
		var junk []byte
		once.Do(func() {
			junk = pcpMapResp(req, 0, 3600, 47700, "198.51.100.9")
			for i := range junk[24:36] {
				junk[24+i] ^= 0xff
			}
		})
		if junk != nil {
			return junk
		}
		return pcpMapResp(req, 0, 3600, 47700, "198.51.100.9")
	})
	c := newPCPClient(gw.addr())
	if _, err := c.Map(context.Background(), udpWant, udpWant.Port, time.Hour); err != nil {
		t.Fatalf("Map: %v", err)
	}
}
