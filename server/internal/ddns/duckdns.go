// DuckDNS：只要 token 的免费子域服务，最适合没有域名的人。
// 更新接口是一个 GET：参数带 token、子域名（不含 .duckdns.org）与 v4/v6 地址，
// 响应体是纯文本 "OK"/"KO"。
package ddns

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// DuckDNS 提供方。Token 是 duckdns.org 账户页的 token。
type DuckDNS struct {
	Token string
}

func (d *DuckDNS) Name() string { return "duckdns" }

func (d *DuckDNS) Update(ctx context.Context, host string, v4, v6 netip.Addr) error {
	// domains 参数只收子域名部分（不含 .duckdns.org）
	sub := strings.TrimSuffix(strings.ToLower(host), ".duckdns.org")
	if sub == host || sub == "" || strings.Contains(sub, ".") {
		return apiErr(d.Name(), "DuckDNS 只支持 <子域名>.duckdns.org 形式的主机名")
	}
	q := url.Values{"domains": {sub}, "token": {d.Token}}
	if v4.IsValid() {
		q.Set("ip", v4.String())
	}
	if v6.IsValid() {
		q.Set("ipv6", v6.String())
	}
	u := "https://www.duckdns.org/update?" + q.Encode()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if strings.TrimSpace(string(body)) != "OK" {
		return apiErr(d.Name(), fmt.Sprintf("更新被拒绝（%s），检查 token 与子域名", strings.TrimSpace(string(body))))
	}
	return nil
}
