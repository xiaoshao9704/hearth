package portmap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

// startFakeGW6 与 startFakeGW 同形，但监听回环 IPv6（PCP v6 pinhole 走 udp6）。
func startFakeGW6(t *testing.T, respond func(req []byte) []byte) *fakeGW {
	t.Helper()
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatalf("监听 v6 假网关失败: %v", err)
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
			g.srcs = append(g.srcs, from.AddrPort().Addr().Unmap())
			g.mu.Unlock()
			if resp := respond(req); resp != nil {
				conn.WriteToUDP(resp, from)
			}
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return g
}

// testGUA 文档地址段的占位 GUA（RFC 3849 2001:db8::/32），仅测试用、非真实地址。
var testGUA = netip.MustParseAddr("2001:db8::1234")

func TestPCP6PinholeEncodesGUA(t *testing.T) {
	gw := startFakeGW6(t, func(req []byte) []byte {
		return pcpMapResp(req, 0, 3600, binary.BigEndian.Uint16(req[42:44]), testGUA.String())
	})
	c := newPCPPinhole(gw.addr())

	h, err := c.Open(context.Background(), "udp", 47700, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if h.proto != "udp" || h.port != 47700 || h.gua != testGUA || h.method != "pcp6" {
		t.Fatalf("pinhole 认领信息不对: %+v", h)
	}
	if c.Method() != "pcp6" {
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
	// 请求头 client IP 必须是本机 GUA（不是 dial 取到的回环源地址）。
	if got := netip.AddrFrom16([16]byte(req[8:24])); got != testGUA {
		t.Fatalf("client IP = %v，应为本机 GUA", got)
	}
	// 建议外部地址也必须是本机 GUA（v6 无 NAT，外部地址就是自己）。
	if got := netip.AddrFrom16([16]byte(req[44:60])); got != testGUA {
		t.Fatalf("建议外部地址 = %v，应为本机 GUA", got)
	}
	if req[36] != 17 {
		t.Fatalf("协议号 = %d，udp 应为 17", req[36])
	}
	in := binary.BigEndian.Uint16(req[40:42])
	ext := binary.BigEndian.Uint16(req[42:44])
	if in != 47700 || ext != 47700 {
		t.Fatalf("内部端口 %d / 建议外部端口 %d，pinhole 须内外一致", in, ext)
	}
}

func TestPCP6PinholeCloseReusesNonce(t *testing.T) {
	gw := startFakeGW6(t, func(req []byte) []byte {
		return pcpMapResp(req, 0, binary.BigEndian.Uint32(req[4:8]), binary.BigEndian.Uint16(req[42:44]), testGUA.String())
	})
	c := newPCPPinhole(gw.addr())
	ctx := context.Background()

	h, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 续租（幂等重发）与删除都必须复用同一 nonce 认领同一条放行。
	if _, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour); err != nil {
		t.Fatalf("续租: %v", err)
	}
	if err := c.Close(ctx, h); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reqs := gw.requests()
	if len(reqs) != 3 {
		t.Fatalf("请求数 = %d", len(reqs))
	}
	nonce := reqs[0][24:36]
	for i, req := range reqs[1:] {
		if !bytes.Equal(nonce, req[24:36]) {
			t.Fatalf("第 %d 个请求 nonce 变了，认领不到同一条放行", i+2)
		}
	}
	if got := binary.BigEndian.Uint32(reqs[2][4:8]); got != 0 {
		t.Fatalf("删除报文 lifetime = %d，应为 0", got)
	}
}

// TestPCP6PinholeDistinctNoncePerGUA 一台多 GUA 时，每个 (端口, GUA) 各有独立 nonce。
func TestPCP6PinholeDistinctNoncePerGUA(t *testing.T) {
	gw := startFakeGW6(t, func(req []byte) []byte {
		return pcpMapResp(req, 0, 3600, binary.BigEndian.Uint16(req[42:44]), testGUA.String())
	})
	c := newPCPPinhole(gw.addr())
	ctx := context.Background()

	gua2 := netip.MustParseAddr("2001:db8::5678")
	if _, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour); err != nil {
		t.Fatalf("Open gua1: %v", err)
	}
	if _, err := c.Open(ctx, "udp", 47700, gua2, time.Hour); err != nil {
		t.Fatalf("Open gua2: %v", err)
	}
	reqs := gw.requests()
	if len(reqs) != 2 {
		t.Fatalf("请求数 = %d", len(reqs))
	}
	if bytes.Equal(reqs[0][24:36], reqs[1][24:36]) {
		t.Fatal("不同 GUA 的放行应各有独立 nonce")
	}
}

func TestPCP6PinholeResultCodes(t *testing.T) {
	cases := []struct {
		code byte
		want error
	}{
		{pcpResultNotAuthorized, ErrNotAuthorized},
		{pcpResultNoResources, ErrNoResources},
	}
	for _, tc := range cases {
		gw := startFakeGW6(t, func(req []byte) []byte {
			return pcpMapResp(req, tc.code, 0, 0, testGUA.String())
		})
		_, err := newPCPPinhole(gw.addr()).Open(context.Background(), "udp", 47700, testGUA, time.Hour)
		if !errors.Is(err, tc.want) {
			t.Fatalf("结果码 %d → %v，期望 %v", tc.code, err, tc.want)
		}
	}
}

func TestPCP6PinholeTimeoutIsUnsupported(t *testing.T) {
	gw := startFakeGW6(t, func([]byte) []byte { return nil })
	_, err := newPCPPinhole(gw.addr()).Open(context.Background(), "udp", 47700, testGUA, time.Hour)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("网关不响应应返回 ErrUnsupported，得到 %v", err)
	}
}

// TestPCP6PinholeBindsSourceToGUA 报文必须从被放行的 GUA 发出：secure_mode 的网关按
// 报文源地址判定「只能给自己开洞」。回环里能绑的本机地址只有 ::1，就拿它当这条放行的 GUA。
func TestPCP6PinholeBindsSourceToGUA(t *testing.T) {
	loop := netip.MustParseAddr("::1")
	gw := startFakeGW6(t, func(req []byte) []byte {
		return pcpMapResp(req, 0, 3600, binary.BigEndian.Uint16(req[42:44]), loop.String())
	})
	if _, err := newPCPPinhole(gw.addr()).Open(context.Background(), "udp", 47700, loop, time.Hour); err != nil {
		t.Fatalf("Open: %v", err)
	}
	gw.mu.Lock()
	srcs := append([]netip.Addr(nil), gw.srcs...)
	gw.mu.Unlock()
	if len(srcs) != 1 {
		t.Fatalf("请求数 = %d", len(srcs))
	}
	if srcs[0] != loop {
		t.Fatalf("报文源地址 = %v，应绑定到被放行的 GUA %v", srcs[0], loop)
	}
}
