// Cloudflare：API Token 认证。zone 由主机名自动推——列 /zones 按后缀匹配
// （host 以 zone 名结尾即归属该 zone）；记录存在则 PUT 更新、不存在则 POST 创建，
// A 与 AAAA 分开处理。记录一律不代理（proxied=false：语音流量要直连）。
package ddns

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Cloudflare 提供方。Token 需要有 Zone.DNS 编辑权限的 API Token。
type Cloudflare struct {
	Token string
}

func (c *Cloudflare) Name() string { return "cloudflare" }

type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body any, out *cfResponse) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	if !out.Success {
		msg := resp.Status
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return apiErr(c.Name(), msg)
	}
	return nil
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// zone 按主机名后缀匹配归属 zone（如 voice.example.com → example.com）。
func (c *Cloudflare) zone(ctx context.Context, host string) (cfZone, error) {
	var out cfResponse
	if err := c.call(ctx, http.MethodGet, "/zones?per_page=50", nil, &out); err != nil {
		return cfZone{}, err
	}
	var zones []cfZone
	if err := json.Unmarshal(out.Result, &zones); err != nil {
		return cfZone{}, err
	}
	best := cfZone{}
	for _, z := range zones {
		if host == z.Name || strings.HasSuffix(host, "."+z.Name) {
			if len(z.Name) > len(best.Name) { // 最长后缀优先
				best = z
			}
		}
	}
	if best.ID == "" {
		return cfZone{}, apiErr(c.Name(), "该 token 下没有匹配 "+host+" 的 zone")
	}
	return best, nil
}

type cfRecord struct {
	ID string `json:"id"`
}

// upsert 一条 A/AAAA 记录：存在则更新，不存在则创建。
func (c *Cloudflare) upsert(ctx context.Context, zoneID, host, rtype string, addr netip.Addr) error {
	q := url.Values{"type": {rtype}, "name": {host}}
	var out cfResponse
	if err := c.call(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	var recs []cfRecord
	if err := json.Unmarshal(out.Result, &recs); err != nil {
		return err
	}
	body := map[string]any{"type": rtype, "name": host, "content": addr.String(), "ttl": 120, "proxied": false}
	if len(recs) > 0 {
		return c.call(ctx, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+recs[0].ID, body, &out)
	}
	return c.call(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, &out)
}

func (c *Cloudflare) Update(ctx context.Context, host string, v4, v6 netip.Addr) error {
	z, err := c.zone(ctx, host)
	if err != nil {
		return err
	}
	if v4.IsValid() {
		if err := c.upsert(ctx, z.ID, host, "A", v4); err != nil {
			return err
		}
	}
	if v6.IsValid() {
		if err := c.upsert(ctx, z.ID, host, "AAAA", v6); err != nil {
			return err
		}
	}
	return nil
}
