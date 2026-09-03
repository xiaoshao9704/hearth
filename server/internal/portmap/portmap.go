// Package portmap 向默认网关申请端口映射（PCP / NAT-PMP / UPnP IGD），是与内核无关的
// 进程内网络基建：HTTP 端口与各媒体端口一并映射，不进 rtc/。
//
// 设计约束（详见 docs/plan-portmap.md）：
//   - P1 只向本机默认网关申请一层；上游还有一层时给诊断指引，由部署者一次性配置（级联见方案第六节）。
//   - 优先申请 external == internal：双层 NAT 下上游 DMZ 是端口不变透传，端口一变整条链就断。
//   - 判定映射是否有效不看 IP 段：网关返回私网外部地址（上游 DMZ）映射照样有效，
//     只标 DiagUpstreamNAT 提示上游要配转发。
//   - 任何失败都不影响启动，也不得让 /healthz 变成非 200。
package portmap

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

// Want 一条待映射的本机监听端口。
type Want struct {
	Proto string // "tcp" | "udp"
	Port  int    // 本机监听端口，也是首选的外部端口
	Desc  string // 网关映射表里的描述，如 "hearth http"
	// StrictPort 外部端口必须等于内部端口：网关改派别的端口时视为失败（DiagPortConflict），
	// 而不是接受改派后的端口。
	StrictPort bool
}

// Mapping 一条已建立的映射。
type Mapping struct {
	Proto      string
	Internal   int
	External   int
	ExternalIP netip.Addr
	Method     string // "pcp" | "natpmp" | "upnp"
	ExpiresAt  time.Time
}

// Diagnosis 可操作的失败分类，日志与管理后台共用同一套文案（见 Status.Detail）。
type Diagnosis string

const (
	DiagOK                Diagnosis = "ok"
	DiagOff               Diagnosis = "off"                 // portmap_mode=off 或 wants 为空
	DiagNoGateway         Diagnosis = "no_gateway"          // 发现不到任何支持的网关
	DiagDisabledByGateway Diagnosis = "disabled_by_gateway" // 网关主动禁用了端口转发（PCP NOT_AUTHORIZED / UPnP 606）
	DiagUpstreamNAT       Diagnosis = "upstream_nat"        // 映射成功但外部地址是私网：上游还有一层
	DiagPortConflict      Diagnosis = "port_conflict"       // 外部端口被占（UPnP 718 / StrictPort 被改派）
	DiagHostFirewall      Diagnosis = "host_firewall"       // 预留：映射成立但本机防火墙可能拦截（Windows）
	DiagError             Diagnosis = "error"               // 其余失败（网关资源不足、超时等），退避重试中
)

// Status 给日志与管理后台回显的快照。
type Status struct {
	Method    string     // 当前生效的协议，空 = 尚未发现网关
	Gateway   netip.Addr // 默认网关地址（零值 = 未发现）
	Diagnosis Diagnosis
	Detail    string // 人读文案：诊断含义 + 下一步该做什么
	Mappings  []Mapping
	UpdatedAt time.Time
}

// 各协议客户端统一用这组哨兵错误分类网关的返回，Mapper 据此决定重试还是给诊断。
var (
	ErrNotAuthorized = errors.New("网关禁用了端口转发") // PCP NOT_AUTHORIZED(2) / UPnP 606
	ErrConflict      = errors.New("外部端口被占用")   // UPnP 718 ConflictInMappingEntry
	ErrPermanentOnly = errors.New("网关只支持永久映射") // UPnP 725 OnlyPermanentLeasesSupported
	ErrNoResources   = errors.New("网关资源不足")    // PCP NO_RESOURCES(8) / UPnP 501
	ErrUnsupported   = errors.New("网关不支持该协议")  // 发现阶段：无响应或版本不支持
)

// client 单一协议的网关客户端。实现：pcp.go（PCP，含 NAT-PMP 回落）、upnp.go（IGD v1/v2）。
// 各实现只管报文与错误分类，不做重试/续租策略（那是 Mapper 的事）。
type client interface {
	Method() string
	// Map 申请或续租，须幂等（同一映射重发即续租）。external 是期望的外部端口；
	// 网关可能改派，以返回的 Mapping.External 为准。lifetime 是期望租期，
	// 返回的 ExpiresAt 以网关实际授予为准；lifetime 为 0 表示永久（仅 ErrPermanentOnly 回退时使用）。
	Map(ctx context.Context, w Want, external int, lifetime time.Duration) (Mapping, error)
	// Unmap 删除映射；映射已不存在不算错误。
	Unmap(ctx context.Context, w Want, external int) error
}

// 策略常量写死，不开配置项。
const (
	leaseDuration    = time.Hour         // 申请的租期；半程续租
	renewInterval    = leaseDuration / 2 // 续租周期，每轮幂等重发（路由器重启丢映射时自愈）
	discoveryTimeout = 3 * time.Second   // 单个协议的发现/首包超时
	maxBackoff       = 10 * time.Minute  // 发现失败/网关拒绝后的最大重试间隔
	minBackoff       = 30 * time.Second
)
