package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"hearth/server/internal/portmap"
	"hearth/server/internal/tlsx"
)

func TestProbeExternalOffUsesPublicBasePort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := (&API{}).probeExternal(context.Background(), tlsx.Config{Mode: "off"}, netcheckResult{}, srv.URL)
	if got.Verdict != "reachable" || got.URL != srv.URL+"/healthz" {
		t.Fatalf("off 模式应按公开地址的实际端口回探: %+v", got)
	}
}

func TestChooseIPCertificateReusesPortmapVerdict(t *testing.T) {
	public := netip.MustParseAddr("203.0.113.10")
	got := chooseIPCertificate(portmap.Status{
		Diagnosis: portmap.DiagOK,
		Mappings:  []portmap.Mapping{{Proto: "tcp", ExternalIP: public, External: 80}},
	}, nil, nil)
	if !got.Available || got.Subject != public.String() {
		t.Fatalf("公网 TCP 映射应允许 IP 证书: %+v", got)
	}

	got = chooseIPCertificate(portmap.Status{
		Diagnosis: portmap.DiagUpstreamNAT,
		Mappings:  []portmap.Mapping{{Proto: "tcp", ExternalIP: public, External: 80}},
	}, nil, nil)
	if got.Available {
		t.Fatalf("上游 NAT 结论不得允许 IP 证书: %+v", got)
	}
}

func TestChooseIPCertificateAllowsDirectPublicIPv6(t *testing.T) {
	public := netip.MustParseAddr("2001:db8::1")
	got := chooseIPCertificate(portmap.Status{Diagnosis: portmap.DiagUpstreamNAT}, nil, []netip.Addr{public})
	if !got.Available || got.Subject != public.String() {
		t.Fatalf("v4 在上游 NAT 内时，网卡直持的公网 IPv6 仍应允许 IP 证书: %+v", got)
	}
	if publicIP(netip.MustParseAddr("100.64.0.1")) {
		t.Fatal("RFC 6598 地址不得当作公网可直连 IP")
	}
}
