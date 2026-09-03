package portmap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PCP（RFC 6887）与 NAT-PMP（RFC 6886）是同一个网关端口上的两代协议：先按 PCP 发，
// 网关回 UNSUPP_VERSION 就永久改发 NAT-PMP 报文，两者共用同一套重试与错误分类。
//
// 不做 epoch 回退检测：PCP 响应里的 epoch 变小意味着网关重启、映射已丢，但我们本就
// 每轮续租幂等重发，且只在续租轮才与网关通信、不做轮询——检测到也没有比「下一轮重发」
// 更早的修复时机，多写一套状态没有收益。
const (
	pcpPort    = 5351
	pcpVersion = 2
	natpmpVer  = 0

	pcpOpcodeMap = 1
	pcpRespBit   = 0x80 // 响应报文的 opcode 字节最高位

	pcpResultUnsuppVersion = 1
	pcpResultNotAuthorized = 2
	pcpResultNoResources   = 8

	natpmpOpExternal = 0 // 取外部地址（PCP 的 MAP 响应自带外部地址，NAT-PMP 要单独问）
	natpmpOpMapUDP   = 1
	natpmpOpMapTCP   = 2

	natpmpResultNotAuthorized = 2
	natpmpResultNoResources   = 4
)

// errUnsuppVersion 内部信号：网关只认 NAT-PMP，切协议后重发。
var errUnsuppVersion = errors.New("网关不支持 PCP 版本")

type pcpClient struct {
	gw netip.AddrPort
	// clientIP 覆盖请求头里的 client IP，级联申请时用（见 chain.go）：报文经内层 NAT
	// 到达上游时源地址已变成内层路由的 WAN 地址，头里填同一个地址才过得了
	// ADDRESS_MISMATCH 校验。零值 = 用 dial() 取到的本机源地址。
	clientIP netip.Addr

	mu     sync.Mutex
	natpmp bool                // 收到 UNSUPP_VERSION 后永久回落，不再试 PCP
	nonces map[string][12]byte // (proto, 内部端口) → nonce：续租与删除必须复用同一个才认领得到同一条映射
}

func newPCPClient(gw netip.AddrPort) client {
	return newPCPClientAs(gw, netip.Addr{})
}

// newPCPClientAs 同上，但把请求头里的 client IP 固定成 clientIP（级联申请，见 chain.go）。
func newPCPClientAs(gw netip.AddrPort, clientIP netip.Addr) client {
	return &pcpClient{gw: gw, clientIP: clientIP, nonces: make(map[string][12]byte)}
}

func (c *pcpClient) Method() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.natpmp {
		return "natpmp"
	}
	return "pcp"
}

func (c *pcpClient) Map(ctx context.Context, w Want, external int, lifetime time.Duration) (Mapping, error) {
	if lifetime <= 0 {
		// 两个协议里 lifetime 0 都是「删除」，没有永久映射的表达方式。
		// ErrPermanentOnly 只可能来自 UPnP，这里收到 0 属于误用，按最长租期处理。
		lifetime = leaseDuration
	}
	return c.do(ctx, w, external, lifetime)
}

// Unmap 删除 = lifetime 0。两个协议都按 (协议, 内部端口, nonce) 认领映射，
// 删除报文里的建议外部端口一律填 0，入参 external 用不上。
func (c *pcpClient) Unmap(ctx context.Context, w Want, external int) error {
	_, err := c.do(ctx, w, 0, 0)
	return err
}

func (c *pcpClient) do(ctx context.Context, w Want, external int, lifetime time.Duration) (Mapping, error) {
	conn, local, err := c.dial()
	if err != nil {
		return Mapping{}, err
	}
	defer conn.Close()

	if c.clientIP.IsValid() {
		local = c.clientIP
	}

	c.mu.Lock()
	fallen := c.natpmp
	c.mu.Unlock()

	if !fallen {
		mp, err := c.pcpExchange(ctx, conn, local, w, external, lifetime)
		if !errors.Is(err, errUnsuppVersion) {
			return mp, err
		}
		c.mu.Lock()
		c.natpmp = true
		c.mu.Unlock()
	}
	return c.natpmpExchange(ctx, conn, w, external, lifetime)
}

// dial 连到网关并取本机源地址：PCP 请求头里的 client IP 必须与报文源地址一致，
// 网关据此识别请求是否经过了它不知道的 NAT，不一致会被丢弃。
func (c *pcpClient) dial() (*net.UDPConn, netip.Addr, error) {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(c.gw))
	if err != nil {
		return nil, netip.Addr{}, err
	}
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		conn.Close()
		return nil, netip.Addr{}, errors.New("取本机源地址失败")
	}
	return conn, la.AddrPort().Addr().Unmap(), nil
}

// nonce 按 (协议, 内部端口) 生成一次并留存：续租与删除复用它认领同一条映射。
func (c *pcpClient) nonce(w Want) [12]byte {
	key := strings.ToLower(w.Proto) + "/" + strconv.Itoa(w.Port)
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.nonces[key]; ok {
		return n
	}
	var n [12]byte
	rand.Read(n[:])
	c.nonces[key] = n
	return n
}

func (c *pcpClient) pcpExchange(ctx context.Context, conn *net.UDPConn, local netip.Addr, w Want, external int, lifetime time.Duration) (Mapping, error) {
	proto, err := protoNumber(w.Proto)
	if err != nil {
		return Mapping{}, err
	}
	nonce := c.nonce(w)

	// 24 字节公共头 + 36 字节 MAP 数据。
	req := make([]byte, 60)
	req[0] = pcpVersion
	req[1] = pcpOpcodeMap
	binary.BigEndian.PutUint32(req[4:8], uint32(lifetime/time.Second))
	ip := local.As16() // v4 写成 v4-mapped 的 16 字节
	copy(req[8:24], ip[:])
	copy(req[24:36], nonce[:])
	req[36] = proto
	binary.BigEndian.PutUint16(req[40:42], uint16(w.Port))
	binary.BigEndian.PutUint16(req[42:44], uint16(external))
	// req[44:60] 建议外部地址留全零 = 无偏好，由网关分配。

	resp, err := roundTrip(ctx, conn, req, func(b []byte) bool {
		if len(b) >= 4 && b[0] == natpmpVer {
			return true // 只认 NAT-PMP 的网关会用 v0 报文回 UNSUPP_VERSION
		}
		if len(b) < 24 || b[0] != pcpVersion || b[1] != pcpRespBit|pcpOpcodeMap {
			return false
		}
		if b[3] != 0 {
			return true // 出错响应可能不带完整 MAP 数据，交给解析处理
		}
		return len(b) >= 60 && [12]byte(b[24:36]) == nonce && binary.BigEndian.Uint16(b[40:42]) == uint16(w.Port)
	})
	if err != nil {
		return Mapping{}, err
	}

	if resp[0] == natpmpVer {
		return Mapping{}, errUnsuppVersion
	}
	if err := pcpResultErr(resp[3]); err != nil {
		return Mapping{}, err
	}
	if len(resp) < 60 {
		return Mapping{}, errors.New("pcp 响应长度不足")
	}

	granted := time.Duration(binary.BigEndian.Uint32(resp[4:8])) * time.Second
	mp := Mapping{
		Proto:      w.Proto,
		Internal:   w.Port,
		External:   int(binary.BigEndian.Uint16(resp[42:44])),
		ExternalIP: netip.AddrFrom16([16]byte(resp[44:60])).Unmap(),
		Method:     "pcp",
	}
	if granted > 0 {
		mp.ExpiresAt = time.Now().Add(granted)
	}
	return mp, nil
}

func (c *pcpClient) natpmpExchange(ctx context.Context, conn *net.UDPConn, w Want, external int, lifetime time.Duration) (Mapping, error) {
	op := byte(natpmpOpMapUDP)
	if strings.EqualFold(w.Proto, "tcp") {
		op = natpmpOpMapTCP
	}

	req := make([]byte, 12)
	req[0] = natpmpVer
	req[1] = op
	binary.BigEndian.PutUint16(req[4:6], uint16(w.Port))
	binary.BigEndian.PutUint16(req[6:8], uint16(external))
	binary.BigEndian.PutUint32(req[8:12], uint32(lifetime/time.Second))

	resp, err := roundTrip(ctx, conn, req, func(b []byte) bool {
		return len(b) >= 8 && b[0] == natpmpVer && b[1] == pcpRespBit|op
	})
	if err != nil {
		return Mapping{}, err
	}
	if err := natpmpResult(binary.BigEndian.Uint16(resp[2:4])); err != nil {
		return Mapping{}, err
	}
	if len(resp) < 16 {
		return Mapping{}, errors.New("natpmp 响应长度不足")
	}

	granted := time.Duration(binary.BigEndian.Uint32(resp[12:16])) * time.Second
	mp := Mapping{
		Proto:    w.Proto,
		Internal: w.Port,
		External: int(binary.BigEndian.Uint16(resp[10:12])),
		Method:   "natpmp",
	}
	if granted > 0 {
		mp.ExpiresAt = time.Now().Add(granted)
	}
	if lifetime == 0 {
		return mp, nil // 删除不必再问外部地址
	}

	// NAT-PMP 的映射响应里没有外部地址字段，要另发一次 op 0。
	mp.ExternalIP, err = c.natpmpExternalIP(ctx, conn)
	if err != nil {
		return Mapping{}, err
	}
	return mp, nil
}

func (c *pcpClient) natpmpExternalIP(ctx context.Context, conn *net.UDPConn) (netip.Addr, error) {
	req := []byte{natpmpVer, natpmpOpExternal}
	resp, err := roundTrip(ctx, conn, req, func(b []byte) bool {
		return len(b) >= 8 && b[0] == natpmpVer && b[1] == pcpRespBit|natpmpOpExternal
	})
	if err != nil {
		return netip.Addr{}, err
	}
	if err := natpmpResult(binary.BigEndian.Uint16(resp[2:4])); err != nil {
		return netip.Addr{}, err
	}
	if len(resp) < 12 {
		return netip.Addr{}, errors.New("natpmp 外部地址响应长度不足")
	}
	return netip.AddrFrom4([4]byte(resp[8:12])), nil
}

// pcpResultErr 把 PCP MAP 响应的结果码映射成哨兵错误；v4 与 v6 pinhole 共用。
func pcpResultErr(code byte) error {
	switch code {
	case 0:
		return nil
	case pcpResultUnsuppVersion:
		return errUnsuppVersion
	case pcpResultNotAuthorized:
		return fmt.Errorf("%w: pcp NOT_AUTHORIZED", ErrNotAuthorized)
	case pcpResultNoResources:
		return fmt.Errorf("%w: pcp NO_RESOURCES", ErrNoResources)
	default:
		return fmt.Errorf("pcp 结果码 %d", code)
	}
}

func natpmpResult(code uint16) error {
	switch code {
	case 0:
		return nil
	case natpmpResultNotAuthorized:
		return fmt.Errorf("%w: natpmp not authorized", ErrNotAuthorized)
	case natpmpResultNoResources:
		return fmt.Errorf("%w: natpmp out of resources", ErrNoResources)
	default:
		return fmt.Errorf("natpmp 结果码 %d", code)
	}
}

func protoNumber(proto string) (byte, error) {
	switch strings.ToLower(proto) {
	case "tcp":
		return 6, nil
	case "udp":
		return 17, nil
	default:
		return 0, fmt.Errorf("未知协议 %q", proto)
	}
}

// pcp6Client 用同一份 PCP MAP 报文做 IPv6 防火墙 pinhole：IPv6 下 MAP 就是「放行入站」
// 而非端口翻译。与 v4 的两处区别——地址族走 udp6，且请求头 client IP 与 MAP 的建议外部
// 地址都填本机 GUA，且报文源地址也绑到它：真机核实过 PCP 反欺骗要求报文源、头里的
// client IP、建议外部地址三者都是本机 GUA，网关才授予租约（发链路本地地址无回应）。
// 没有 NAT-PMP 回落（那是 v4 专属）。
type pcp6Client struct {
	gw netip.AddrPort // 网关的 GUA（不是链路本地），端口 5351

	mu     sync.Mutex
	nonces map[string][12]byte // (协议, 端口, GUA) → nonce：续租与删除复用它认领同一条放行
}

func newPCPPinhole(gw netip.AddrPort) pinholeClient {
	return &pcp6Client{gw: gw, nonces: make(map[string][12]byte)}
}

func (c *pcp6Client) Method() string { return "pcp6" }

func (c *pcp6Client) Open(ctx context.Context, proto string, port int, gua netip.Addr, lifetime time.Duration) (pinhole, error) {
	if lifetime <= 0 {
		lifetime = leaseDuration // 0 是「删除」的表达，Open 收到属误用，按最长租期处理
	}
	if err := c.exchange(ctx, proto, port, gua, lifetime); err != nil {
		return pinhole{}, err
	}
	return pinhole{proto: proto, port: port, gua: gua, method: "pcp6"}, nil
}

// Close 删除 = lifetime 0，复用同一 nonce 认领。
func (c *pcp6Client) Close(ctx context.Context, h pinhole) error {
	return c.exchange(ctx, h.proto, h.port, h.gua, 0)
}

func (c *pcp6Client) nonce(proto string, port int, gua netip.Addr) [12]byte {
	key := pinNonceKey(proto, port, gua)
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.nonces[key]; ok {
		return n
	}
	var n [12]byte
	rand.Read(n[:])
	c.nonces[key] = n
	return n
}

func (c *pcp6Client) exchange(ctx context.Context, proto string, port int, gua netip.Addr, lifetime time.Duration) error {
	protoNum, err := protoNumber(proto)
	if err != nil {
		return err
	}
	// 源地址绑到 gua：secure_mode 的网关按报文源地址判定「只能给自己开洞」，多 GUA
	// （临时/隐私地址）时不绑定会由内核任选源地址，开出来的洞就不是这一条。绑不上
	// （地址刚被系统撤销）退回不绑定，报文头里仍填 gua，授权与否交给网关裁决。
	conn, err := net.DialUDP("udp6", &net.UDPAddr{IP: gua.AsSlice()}, net.UDPAddrFromAddrPort(c.gw))
	if err != nil {
		if conn, err = net.DialUDP("udp6", nil, net.UDPAddrFromAddrPort(c.gw)); err != nil {
			return err
		}
	}
	defer conn.Close()

	nonce := c.nonce(proto, port, gua)
	ip := gua.As16()

	// 24 字节公共头 + 36 字节 MAP 数据，与 v4 逐字段相同，只是地址填本机 GUA。
	req := make([]byte, 60)
	req[0] = pcpVersion
	req[1] = pcpOpcodeMap
	binary.BigEndian.PutUint32(req[4:8], uint32(lifetime/time.Second))
	copy(req[8:24], ip[:]) // 请求头 client IP = 本机 GUA
	copy(req[24:36], nonce[:])
	req[36] = protoNum
	binary.BigEndian.PutUint16(req[40:42], uint16(port)) // 内部端口
	binary.BigEndian.PutUint16(req[42:44], uint16(port)) // 建议外部端口 == 内部端口
	copy(req[44:60], ip[:])                              // 建议外部地址 = 本机 GUA

	resp, err := roundTrip(ctx, conn, req, func(b []byte) bool {
		if len(b) < 24 || b[0] != pcpVersion || b[1] != pcpRespBit|pcpOpcodeMap {
			return false
		}
		if b[3] != 0 {
			return true // 出错响应可能不带完整 MAP 数据，交给结果码处理
		}
		return len(b) >= 60 && [12]byte(b[24:36]) == nonce && binary.BigEndian.Uint16(b[40:42]) == uint16(port)
	})
	if err != nil {
		return err
	}
	return pcpResultErr(resp[3])
}

// roundTrip 发请求等第一个匹配的响应：250ms 起指数退避、最多 3 次（约 1.75s 见分晓）。
// RFC 要求退到 9 次，但我们后面还有 UPnP 兜底，等太久只会拖慢启动后的首次映射。
// 全部超时返回 ErrUnsupported —— Mapper 据此换下一个协议。
func roundTrip(ctx context.Context, conn *net.UDPConn, req []byte, match func([]byte) bool) ([]byte, error) {
	buf := make([]byte, 1500)
	delay := 250 * time.Millisecond
	for range 3 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(delay)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break // 读超时（或连接出错）：重发
			}
			if match(buf[:n]) {
				return bytes.Clone(buf[:n]), nil
			}
			// 不匹配的包（网关重发的旧应答、别的 opcode）丢弃，继续等本轮剩余时间
		}
		delay *= 2
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}
