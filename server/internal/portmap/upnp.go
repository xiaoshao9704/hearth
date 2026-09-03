package portmap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
)

// upnpConn 是 IGDv2/v1/PPP 三种 WANConnection SOAP 服务的公共子集：三者的
// AddPortMapping/DeletePortMapping/GetExternalIPAddress 方法签名逐字相同，
// 收敛成一个接口后 Map/Unmap 不必按版本分叉。GetServiceClient 用来取回
// 发现时记录的设备描述 URL（算内部客户端地址要用）。
type upnpConn interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	GetServiceClient() *goupnp.ServiceClient
}

// upnpClient 实现 client 接口。v2 非空时说明发现到的是 IGDv2，
// 才有 AddAnyPortMapping 这个「网关任选端口」动作。
type upnpClient struct {
	conn upnpConn
	v2   *internetgateway2.WANIPConnection2
}

// discoverUPnP 依次尝试 IGDv2 → IGDv1 → WANPPPConnection1，取第一个发现到的。
// IGDv1 是主路径而非回退：不少消费级路由器（如 miniupnpd 的 force_igd_desc_v1）
// 默认以 v1 身份宣告。三路发现共用调用方传入的 ctx，总耗时由它的 deadline 控制。
func discoverUPnP(ctx context.Context) (client, error) {
	if clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0], v2: clients[0]}, nil
	}
	if clients, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0]}, nil
	}
	if clients, _, err := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return &upnpClient{conn: clients[0]}, nil
	}
	return nil, ErrUnsupported
}

func (u *upnpClient) Method() string { return "upnp" }

func (u *upnpClient) Map(ctx context.Context, w Want, external int, lifetime time.Duration) (Mapping, error) {
	localAddr, err := localAddrFor(u.conn.GetServiceClient().Location)
	if err != nil {
		return Mapping{}, fmt.Errorf("确定本机出口地址失败: %w", err)
	}

	proto := strings.ToUpper(w.Proto)
	seconds := uint32(lifetime / time.Second)

	gotExternal := external
	if external == 0 {
		// v1/PPP 没有「网关任选端口」的动作；方案里 v1 的处理方式是提示换端口。
		if u.v2 == nil {
			return Mapping{}, ErrConflict
		}
		// 首选端口仍传内部端口：IGDv2 不允许外部端口通配（716），被占时由网关改派并回传。
		reserved, err := u.v2.AddAnyPortMappingCtx(ctx, "", uint16(w.Port), proto, uint16(w.Port), localAddr.String(), true, w.Desc, seconds)
		if err != nil {
			return Mapping{}, classifyUPnPError(err)
		}
		gotExternal = int(reserved)
	} else {
		if err := u.conn.AddPortMappingCtx(ctx, "", uint16(external), proto, uint16(w.Port), localAddr.String(), true, w.Desc, seconds); err != nil {
			return Mapping{}, classifyUPnPError(err)
		}
	}

	extIPStr, err := u.conn.GetExternalIPAddressCtx(ctx)
	if err != nil {
		return Mapping{}, classifyUPnPError(err)
	}
	extIP, err := netip.ParseAddr(extIPStr)
	if err != nil {
		return Mapping{}, fmt.Errorf("网关返回的外部地址无法解析: %w", err)
	}

	var expiresAt time.Time
	if lifetime > 0 {
		expiresAt = time.Now().Add(lifetime)
	}

	return Mapping{
		Proto:      w.Proto,
		Internal:   w.Port,
		External:   gotExternal,
		ExternalIP: extIP,
		Method:     "upnp",
		ExpiresAt:  expiresAt,
	}, nil
}

func (u *upnpClient) Unmap(ctx context.Context, w Want, external int) error {
	err := u.conn.DeletePortMappingCtx(ctx, "", uint16(external), strings.ToUpper(w.Proto))
	if err == nil {
		return nil
	}
	var fault *soap.SOAPFaultError
	if errors.As(err, &fault) && fault.Detail.UPnPError.Errorcode == 714 {
		// NoSuchEntryInArray：映射本就不存在，视为已删除。
		return nil
	}
	return classifyUPnPError(err)
}

// localAddrFor 取本机面向设备的出口地址：向设备描述 URL 的 host:port 拨一个
// UDP 连接（不实际发包）取内核选定的 LocalAddr，而不是取本机第一块网卡的地址——
// 多网卡/多路由表时两者可能不是同一个。
func localAddrFor(loc *url.URL) (netip.Addr, error) {
	port := loc.Port()
	if port == "" {
		port = "80"
	}
	conn, err := net.Dial("udp", net.JoinHostPort(loc.Hostname(), port))
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(host)
}

// classifyUPnPError 把 SOAP 错误码映射成 portmap 的哨兵错误；不认识的错误码
// 原样包装并保留错误码与描述文字，供日志诊断。
func classifyUPnPError(err error) error {
	var fault *soap.SOAPFaultError
	if !errors.As(err, &fault) {
		return err
	}
	code := fault.Detail.UPnPError.Errorcode
	desc := fault.Detail.UPnPError.ErrorDescription
	switch code {
	case 606: // NotAuthorized
		return fmt.Errorf("%w: %s", ErrNotAuthorized, desc)
	case 718, 729: // ConflictInMappingEntry / ConflictWithOtherMechanisms
		return fmt.Errorf("%w: %s", ErrConflict, desc)
	case 725: // OnlyPermanentLeasesSupported
		return fmt.Errorf("%w: %s", ErrPermanentOnly, desc)
	case 501: // ActionFailed（网关资源不足/后端故障）
		return fmt.Errorf("%w: %s", ErrNoResources, desc)
	default:
		return fmt.Errorf("upnp 错误码 %d: %s", code, desc)
	}
}
