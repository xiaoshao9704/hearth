// DNSPod（腾讯云）：ID+Token 认证（login_token=ID,Token），dnsapi.cn 表单 POST。
// Record.List 查记录、Record.DDNS 更新（值不变也幂等成功）、Record.Create 创建。
// 主域与子域的拆分靠 Record.List 逐级试探：先从最短主域（最后两段）试起，
// 「域名不存在」类错误就加长主域重试，直到试通或穷尽。
package ddns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// DNSPod 提供方。ID/Token 在 DNSPod 控制台「密钥管理」创建。
type DNSPod struct {
	ID    string
	Token string
}

func (d *DNSPod) Name() string { return "dnspod" }

type dpResponse struct {
	Status struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
	Records []struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"records"`
}

// call 调 dnsapi.cn 的一个动作；login_token/format 由这里统一补。
// 返回错误带提供方前缀；status.code != 1 视为 API 拒绝。
func (d *DNSPod) call(ctx context.Context, action string, params url.Values) (*dpResponse, error) {
	params.Set("login_token", d.ID+","+d.Token)
	params.Set("format", "json")
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://dnsapi.cn/"+action, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out dpResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status.Code != "1" {
		return &out, apiErr(d.Name(), out.Status.Message)
	}
	return &out, nil
}

// splitHost 主机名拆（主域, 子域）：labels 从尾部取 n 段做主域。
func splitHost(host string, n int) (domain, sub string) {
	labels := strings.Split(host, ".")
	if n >= len(labels) {
		return host, ""
	}
	return strings.Join(labels[len(labels)-n:], "."), strings.Join(labels[:len(labels)-n], ".")
}

// upsert 查/改/建一条记录。sub 为主机名去掉主域的部分（主域本身用 @）。
func (d *DNSPod) upsert(ctx context.Context, domain, sub, rtype string, addr netip.Addr) error {
	if sub == "" {
		sub = "@"
	}
	list, listErr := d.call(ctx, "Record.List", url.Values{
		"domain": {domain}, "sub_domain": {sub}, "record_type": {rtype},
	})
	if listErr != nil {
		return listErr
	}
	value := addr.String()
	if len(list.Records) > 0 {
		if list.Records[0].Value == value {
			return nil // 值没变，省一次写
		}
		_, err := d.call(ctx, "Record.DDNS", url.Values{
			"domain": {domain}, "record_id": {list.Records[0].ID},
			"record_line": {"默认"}, "value": {value},
		})
		return err
	}
	_, err := d.call(ctx, "Record.Create", url.Values{
		"domain": {domain}, "sub_domain": {sub}, "record_type": {rtype},
		"record_line": {"默认"}, "value": {value},
	})
	return err
}

// updateOne 对一种记录类型做主域拆分试探：主域从最短（两段）逐级加长，
// 「域名不存在」就再试；试通或穷尽后返回首个非域名类错误。
func (d *DNSPod) updateOne(ctx context.Context, host, rtype string, addr netip.Addr) error {
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return apiErr(d.Name(), "主机名格式不对")
	}
	var lastErr error
	for n := 2; n < len(labels); n++ {
		domain, sub := splitHost(host, n)
		err := d.upsert(ctx, domain, sub, rtype, addr)
		if err == nil {
			return nil
		}
		if !isDomainNotFound(err) {
			return err // 凭证/权限类错误重试别的拆分没意义
		}
		lastErr = err
	}
	if lastErr != nil {
		return apiErr(d.Name(), fmt.Sprintf("账户下没有匹配 %s 的域名", host))
	}
	return apiErr(d.Name(), "主机名格式不对")
}

// isDomainNotFound 判断「域名不存在」类错误（拆分试探的依据）：DNSPod 文案随语言变，
// 按关键词宽松匹配。
func isDomainNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "域名") ||
		strings.Contains(strings.ToLower(msg), "domain") ||
		strings.Contains(msg, "no exist")
}

func (d *DNSPod) Update(ctx context.Context, host string, v4, v6 netip.Addr) error {
	host = strings.ToLower(host)
	if v4.IsValid() {
		if err := d.updateOne(ctx, host, "A", v4); err != nil {
			return err
		}
	}
	if v6.IsValid() {
		if err := d.updateOne(ctx, host, "AAAA", v6); err != nil {
			return err
		}
	}
	return nil
}
