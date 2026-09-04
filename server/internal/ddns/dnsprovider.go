package ddns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	alidnslib "github.com/libdns/alidns"
	cloudflarelib "github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

// DNSProvider 是 CertMagic DNS-01 所需的 libdns 能力子集。
type DNSProvider interface {
	libdns.RecordAppender
	libdns.RecordDeleter
}

// NewDNSProvider 复用 DDNS 的提供方与凭证构造 DNS-01 provider。
// fingerprint 只用于判断配置是否变化，不得写进日志或状态。
func NewDNSProvider(cfg Config) (provider DNSProvider, fingerprint string) {
	switch cfg.Provider {
	case "cloudflare":
		if cfg.CFToken == "" {
			return nil, ""
		}
		provider = &cloudflarelib.Provider{APIToken: cfg.CFToken}
		fingerprint = dnsFingerprint(cfg.Provider, cfg.Zone, cfg.CFToken)
	case "aliyun":
		if cfg.AliyunID == "" || cfg.AliyunSecret == "" {
			return nil, ""
		}
		provider = &alidnslib.Provider{CredentialInfo: alidnslib.CredentialInfo{
			AccessKeyID: cfg.AliyunID, AccessKeySecret: cfg.AliyunSecret,
		}}
		fingerprint = dnsFingerprint(cfg.Provider, cfg.Zone, cfg.AliyunID, cfg.AliyunSecret)
	default:
		return nil, ""
	}
	return &configuredZoneProvider{upstream: provider, zone: normalizeDNSName(cfg.Zone)}, fingerprint
}

func dnsFingerprint(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// configuredZoneProvider 保证 CertMagic 发现的权威 zone 与显式 ddns_zone 一致；
// 留空时允许 CertMagic 按 DNS 权威记录自动发现。
type configuredZoneProvider struct {
	upstream DNSProvider
	zone     string
}

func (p *configuredZoneProvider) AppendRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	if err := p.checkZone(zone); err != nil {
		return nil, err
	}
	result, err := p.upstream.AppendRecords(ctx, zone, records)
	return result, redactURLError(err)
}

func (p *configuredZoneProvider) DeleteRecords(ctx context.Context, zone string, records []libdns.Record) ([]libdns.Record, error) {
	if err := p.checkZone(zone); err != nil {
		return nil, err
	}
	result, err := p.upstream.DeleteRecords(ctx, zone, records)
	return result, redactURLError(err)
}

func (p *configuredZoneProvider) checkZone(zone string) error {
	if p.zone != "" && normalizeDNSName(zone) != p.zone {
		return fmt.Errorf("DNS-01 发现的 zone %s 与 ddns_zone %s 不一致", normalizeDNSName(zone), p.zone)
	}
	return nil
}
