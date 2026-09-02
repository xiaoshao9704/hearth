// WHIP 通行证（grant）：hearth 接入层做完入场判定后签发，随反代请求头带给远端
// Bellows，远端本地验签即可，不再需要回调 hearth。与 LiveKit join token 同一模型
// （hearth 签、内核用同一 secret 验）；签名密钥复用 bellows_shared_secret（对称 HMAC），
// v 字段保留换算法（如 Ed25519）的余地。
package bellows

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// GrantHeader 反代时携带通行证的请求头。
const GrantHeader = "X-Bellows-Grant"

// grantTTL 通行证有效期（短时效入场券，与 join token 同思路）。
const grantTTL = 60 * time.Second

// grantClockSkew 验签容忍的两端时钟偏差（NTP 正常时远小于此，只为减少误拒）。
const grantClockSkew = 30 * time.Second

var errInvalidGrant = errors.New("通行证无效或已过期")

// grantPayload 通行证内容。op=publish 时带 room/user/offer（offer = 请求体 SHA256 hex，
// 绑定 SDP 防重放挪用）；op=revoke 只需 key。
type grantPayload struct {
	V     int    `json:"v"`
	Op    string `json:"op"` // publish / revoke
	Key   string `json:"key"`
	Room  string `json:"room,omitempty"`
	User  string `json:"user,omitempty"`
	Offer string `json:"offer,omitempty"`
	Exp   int64  `json:"exp"`
}

// signGrant 签发通行证：base64url(payload JSON) + "." + base64url(HMAC-SHA256)。
func signGrant(secret string, p grantPayload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// verifyGrant 验签并解析：常量时间比对签名 → exp 未过（容忍时钟偏差）。op/key/offer
// 与请求的匹配由调用方按场景检查。
func verifyGrant(secret, header string) (*grantPayload, error) {
	body, sig, ok := strings.Cut(header, ".")
	if !ok || secret == "" {
		return nil, errInvalidGrant
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || subtle.ConstantTimeCompare(got, mac.Sum(nil)) != 1 {
		return nil, errInvalidGrant
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errInvalidGrant
	}
	var p grantPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.V != 1 {
		return nil, errInvalidGrant
	}
	if time.Now().Unix() > p.Exp+int64(grantClockSkew/time.Second) {
		return nil, errInvalidGrant
	}
	return &p, nil
}

func offerHash(offer []byte) string {
	sum := sha256.Sum256(offer)
	return hex.EncodeToString(sum[:])
}

// ---- rtc.WHIPGrantIssuer（hearth 侧，远端形态下由接入层调用）----

// IssueWHIPGrant 入场判定通过后为一次 WHIP POST 签发通行证（绑定推流密钥与 offer 哈希）。
func (g *Gateway) IssueWHIPGrant(ctx context.Context, streamKey, room, username string, offer []byte) (header, value string, err error) {
	v, err := signGrant(g.cfg(ctx, "bellows_shared_secret"), grantPayload{
		V: 1, Op: "publish", Key: streamKey, Room: room, User: username,
		Offer: offerHash(offer), Exp: time.Now().Add(grantTTL).Unix(),
	})
	if err != nil {
		return "", "", err
	}
	return GrantHeader, v, nil
}

// revokeClient 撤销远端会话用；调用方一般用 ctx 控制更短的超时。
var revokeClient = &http.Client{Timeout: 10 * time.Second}

// RevokeRemoteSessions 通知远端 Bellows 掐断该推流密钥名下的全部会话（尽力）：
// 签 revoke 通行证后 DELETE {remote_url}/w/sessions/{key}。进程内形态无远端可调，直接返回。
func (g *Gateway) RevokeRemoteSessions(ctx context.Context, streamKey string) error {
	base := g.remoteURL(ctx)
	if base == "" {
		return nil
	}
	v, err := signGrant(g.cfg(ctx, "bellows_shared_secret"), grantPayload{
		V: 1, Op: "revoke", Key: streamKey, Exp: time.Now().Add(grantTTL).Unix(),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/w/sessions/"+streamKey, nil)
	if err != nil {
		return err
	}
	req.Header.Set(GrantHeader, v)
	resp, err := revokeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("远端撤销会话返回 %d", resp.StatusCode)
	}
	return nil
}
