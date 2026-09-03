package portmap

import "errors"

// errNoGateway 路由表里没有可用的 IPv4 默认路由（含容器 bridge 网络里网关是 docker0 的情况——
// 那种情况下地址本身能读到，只是申请必然失败，由 Mapper 的诊断文案兜底）。
var errNoGateway = errors.New("未找到 IPv4 默认网关")

// defaultGateway 返回默认路由（0.0.0.0/0）的下一跳地址，各平台走系统原生途径：
// linux 读 /proc/net/route、darwin 读路由表 RIB、windows 调 GetAdaptersAddresses。
// 见 gateway_linux.go / gateway_darwin.go / gateway_windows.go / gateway_other.go。
