//go:build !linux && !darwin && !windows

package portmap

import (
	"fmt"
	"net/netip"
	"runtime"
)

func defaultGateway() (netip.Addr, error) {
	return netip.Addr{}, fmt.Errorf("%w：%s 未实现网关发现", errNoGateway, runtime.GOOS)
}
