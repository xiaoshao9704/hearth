package portmap

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLivePinhole 对着真实网关跑一遍 v6 pinhole 的「发现 → Open → Close」，默认跳过：
// 结果取决于所在网络，CI 与无 v6 环境都不该因此失败。内容不含任何具体地址/设备信息。
// 手动验收：PORTMAP_LIVE=1 go test -run TestLivePinhole -v ./internal/portmap/
// 需要核对网关放行表时加 PORTMAP_LIVE_HOLD=<秒数>，撤销前会停留这么久。
func TestLivePinhole(t *testing.T) {
	if os.Getenv("PORTMAP_LIVE") != "1" {
		t.Skip("需要 PORTMAP_LIVE=1 和一个支持 v6 pinhole 的网关")
	}
	guas := globalUnicastV6()
	if len(guas) == 0 {
		t.Skip("本机没有全局 IPv6 地址，无从放行")
	}
	gua := guas[0]
	t.Logf("本机有 %d 个 GUA（地址不打印），用第一个做放行", len(guas))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want := Want{Proto: "udp", Port: 47999, Desc: "hearth pinhole live test"}

	var (
		cl  pinholeClient
		h   pinhole
		loc *url.URL
	)
	gw, ok := defaultGateway6()
	if !ok {
		// 默认路由的下一跳多半是链路本地地址，取网关 GUA 只能靠 v6 SSDP。
		var err error
		if loc, gw, err = ssdpSearchV6(ctx); err != nil {
			t.Logf("v6 SSDP 没找到 IGD（%v），两条路都缺网关 GUA", err)
		} else {
			t.Logf("v6 SSDP 发现 IGD：描述 URL 的 host 是可路由 v6 地址（is6=%v）", gw.Is6())
		}
	} else {
		t.Log("默认路由 v6 下一跳就是 GUA，直接用它")
	}

	if gw.IsValid() {
		pc := newPCPPinhole(netip.AddrPortFrom(gw, pcpPort))
		if ph, err := pc.Open(ctx, want.Proto, want.Port, gua, time.Minute); err == nil {
			cl, h = pc, ph
			t.Log("v6 pinhole 成功路径：PCP-v6")
		} else {
			t.Logf("PCP-v6 未成功（%v），退到 UPnP", err)
		}
	}

	if cl == nil {
		uc, err := discoverUPnP6(ctx, loc)
		if err != nil {
			t.Fatalf("v6 pinhole 两条路都不可用: %v", err)
		}
		t.Logf("已发现 UPnP v6 pinhole 途径：%s", uc.Method())
		ph, err := uc.Open(ctx, want.Proto, want.Port, gua, time.Minute)
		if err != nil {
			// 已经连到网关、报文往返正常，只是网关按策略拒绝（v6 pinhole 未授权/被禁）——
			// 这是路由器配置结果，不是代码故障：如实报告，不当失败。
			if errors.Is(err, ErrNotAuthorized) {
				t.Skipf("已连到网关并完成 AddPinhole 往返，但网关拒绝授权 v6 pinhole（%v）——需在网关开启", err)
			}
			t.Fatalf("UPnP6 Open: %v", err)
		}
		cl, h = uc, ph
		t.Logf("v6 pinhole 成功路径：%s", uc.Method())
	}

	if s := os.Getenv("PORTMAP_LIVE_HOLD"); s != "" {
		if d, err := strconv.Atoi(s); err == nil {
			t.Logf("保持放行 %d 秒，便于核对网关放行表", d)
			time.Sleep(time.Duration(d) * time.Second)
		}
	}
	// 撤销另起 ctx：上面的 HOLD 可能已经把 ctx 等超时了。
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	if err := cl.Close(cctx, h); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Log("v6 pinhole 已撤销")
}
