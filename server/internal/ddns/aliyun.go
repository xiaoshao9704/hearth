// 阿里云解析（alidns）：AccessKey ID+Secret，RPC 风格 GET + HMAC-SHA1 签名。
// DescribeDomainRecords 查记录（同时用来试探主域拆分：主域不在账户下会报
// InvalidDomainName.NoExist）→ 存在 UpdateDomainRecord / 不存在 AddDomainRecord。
package ddns

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Aliyun 提供方。ID/Secret 是 RAM 子账户的 AccessKey（只需 AliyunDNSFullAccess）。
type Aliyun struct {
	ID     string
	Secret string
}

func (a *Aliyun) Name() string { return "aliyun" }

const aliEndpoint = "https://alidns.aliyuncs.com/"

// aliPercentEncode 阿里云规定的百分号编码：RFC3986（+ → %20、* → %2A、~ 不编码）。
func aliPercentEncode(s string) string {
	r := url.QueryEscape(s)
	r = strings.ReplaceAll(r, "+", "%20")
	r = strings.ReplaceAll(r, "*", "%2A")
	r = strings.ReplaceAll(r, "%7E", "~")
	return r
}

// aliSign 对参数集算签名（公共参数含时间戳/nonce，由调用方先填好，离线可测）。
func aliSign(secret string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(aliPercentEncode(k))
		sb.WriteByte('=')
		sb.WriteString(aliPercentEncode(params[k]))
	}
	stringToSign := "GET&%2F&" + aliPercentEncode(sb.String())
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type aliResponse struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
	Record  struct {
		Records []struct {
			RecordID string `json:"RecordId"`
			RR       string `json:"RR"`
			Value    string `json:"Value"`
		} `json:"Record"`
	} `json:"DomainRecords"`
}

// call 调一个 alidns 动作；公共参数与签名由这里统一补。
// HTTP 200 也可能带业务错误码，Code 非空即视为拒绝。
func (a *Aliyun) call(ctx context.Context, action string, extra map[string]string) (*aliResponse, error) {
	params := map[string]string{
		"Format":           "JSON",
		"Version":          "2015-01-09",
		"AccessKeyId":      a.ID,
		"SignatureMethod":  "HMAC-SHA1",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"SignatureVersion": "1.0",
		"SignatureNonce":   randHex(16),
		"Action":           action,
	}
	for k, v := range extra {
		params[k] = v
	}
	params["Signature"] = aliSign(a.Secret, params)

	q := make(url.Values, len(params))
	for k, v := range params {
		q.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aliEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out aliResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Code != "" {
		return &out, apiErr(a.Name(), out.Code+": "+out.Message)
	}
	return &out, nil
}

// upsert 查/改/建一条记录（RR 为主机名去掉主域的部分，主域本身用 @）。
func (a *Aliyun) upsert(ctx context.Context, domain, sub, rtype string, addr netip.Addr) error {
	if sub == "" {
		sub = "@"
	}
	list, err := a.call(ctx, "DescribeDomainRecords", map[string]string{
		"DomainName": domain, "RRKeyWord": sub, "TypeKeyWord": rtype, "PageSize": "100",
	})
	if err != nil {
		return err
	}
	value := addr.String()
	for _, rec := range list.Record.Records {
		if rec.RR != sub {
			continue
		}
		if rec.Value == value {
			return nil // 值没变，省一次写
		}
		_, err := a.call(ctx, "UpdateDomainRecord", map[string]string{
			"RecordId": rec.RecordID, "RR": sub, "Type": rtype, "Value": value,
		})
		return err
	}
	_, err = a.call(ctx, "AddDomainRecord", map[string]string{
		"DomainName": domain, "RR": sub, "Type": rtype, "Value": value,
	})
	return err
}

func (a *Aliyun) updateOne(ctx context.Context, host, rtype string, addr netip.Addr) error {
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return apiErr(a.Name(), "主机名格式不对")
	}
	var lastErr error
	for n := 2; n <= len(labels); n++ {
		domain, sub := splitHost(host, n)
		err := a.upsert(ctx, domain, sub, rtype, addr)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "InvalidDomainName.NoExist") {
			return err
		}
		lastErr = err
	}
	if lastErr != nil {
		return apiErr(a.Name(), "账户下没有匹配 "+host+" 的域名")
	}
	return apiErr(a.Name(), "主机名格式不对")
}

func (a *Aliyun) Update(ctx context.Context, host string, v4, v6 netip.Addr) error {
	host = strings.ToLower(host)
	if v4.IsValid() {
		if err := a.updateOne(ctx, host, "A", v4); err != nil {
			return err
		}
	}
	if v6.IsValid() {
		if err := a.updateOne(ctx, host, "AAAA", v6); err != nil {
			return err
		}
	}
	return nil
}

// randHex n 字节随机 hex（SignatureNonce 用；本包自研，不引外部依赖）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "x"
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, c := range b {
		out[i*2] = hexd[c>>4]
		out[i*2+1] = hexd[c&0xf]
	}
	return string(out)
}
