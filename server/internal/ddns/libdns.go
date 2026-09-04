package ddns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	alidnslib "github.com/libdns/alidns"
	cloudflarelib "github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

const ddnsTTL = 2 * time.Minute

type recordSetter interface {
	SetRecords(context.Context, string, []libdns.Record) ([]libdns.Record, error)
}

// libDNSProvider 保留项目自己的 Provider 契约，把具体 libdns 实现封在适配层内。
type libDNSProvider struct {
	name   string
	zone   string
	setter recordSetter
}

func newCloudflare(token, zone string) Provider {
	return &libDNSProvider{
		name:   "cloudflare",
		zone:   zone,
		setter: &cloudflarelib.Provider{APIToken: token},
	}
}

func newAliyun(id, secret, zone string) Provider {
	provider := &alidnslib.Provider{CredentialInfo: alidnslib.CredentialInfo{
		AccessKeyID: id, AccessKeySecret: secret,
	}}
	return &libDNSProvider{
		name:   "aliyun",
		zone:   zone,
		setter: &aliDNSSetter{provider: provider},
	}
}

// aliDNSSetter 先补齐已有记录 ID，规避上游按新值查旧记录时误创建重复记录。
type aliDNSSetter struct {
	provider *alidnslib.Provider
}

func (s *aliDNSSetter) SetRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	existing, err := s.provider.GetRecords(ctx, zone)
	if err != nil {
		return nil, err
	}
	pending, unchanged := prepareAliRecords(existing, records)
	if len(pending) == 0 {
		return unchanged, nil
	}
	updated, err := s.provider.SetRecords(ctx, zone, pending)
	return append(unchanged, updated...), err
}

func prepareAliRecords(existing, desired []libdns.Record) (pending, unchanged []libdns.Record) {
	for _, want := range desired {
		wantRR := want.RR()
		found := false
		for _, current := range existing {
			currentRecord, ok := current.(alidnslib.DomainRecord)
			if !ok || !strings.EqualFold(currentRecord.Name, wantRR.Name) || currentRecord.Type != wantRR.Type {
				continue
			}
			found = true
			if currentRecord.Value == wantRR.Data {
				unchanged = append(unchanged, current)
			} else {
				currentRecord.Value = wantRR.Data
				currentRecord.TTL = uint32(wantRR.TTL.Seconds())
				pending = append(pending, currentRecord)
			}
			break
		}
		if !found {
			pending = append(pending, want)
		}
	}
	return pending, unchanged
}

func (p *libDNSProvider) Name() string { return p.name }

func (p *libDNSProvider) Update(ctx context.Context, host string, v4, v6 netip.Addr) error {
	host = normalizeDNSName(host)
	if host == "" || !strings.Contains(host, ".") {
		return apiErr(p.name, "主机名格式不对")
	}
	if zone := normalizeDNSName(p.zone); zone != "" {
		return p.updateZone(ctx, host, zone, v4, v6)
	}

	labels := strings.Split(host, ".")
	var errs []error
	for n := 2; n <= len(labels); n++ {
		zone := strings.Join(labels[len(labels)-n:], ".")
		if err := p.updateZone(ctx, host, zone, v4, v6); err == nil {
			return nil
		} else {
			errs = append(errs, err)
		}
	}
	return fmt.Errorf("%s: 逐级尝试后仍找不到 %s 对应的 zone: %w", p.name, host, errors.Join(errs...))
}

func (p *libDNSProvider) updateZone(ctx context.Context, host, zone string, v4, v6 netip.Addr) error {
	name, ok := relativeHost(host, zone)
	if !ok {
		return apiErr(p.name, fmt.Sprintf("主机名 %s 不属于 zone %s", host, zone))
	}
	records := make([]libdns.Record, 0, 2)
	if v4.IsValid() {
		records = append(records, libdns.Address{Name: name, TTL: ddnsTTL, IP: v4})
	}
	if v6.IsValid() {
		records = append(records, libdns.Address{Name: name, TTL: ddnsTTL, IP: v6})
	}
	if len(records) == 0 {
		return nil
	}
	if _, err := p.setter.SetRecords(ctx, zone, records); err != nil {
		return fmt.Errorf("%s: 更新 zone %s: %w", p.name, zone, err)
	}
	return nil
}
