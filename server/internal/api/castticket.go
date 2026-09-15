// 桌面端原生投屏的设备票：短时效、无状态签名的一次入场券，取代借用账号级推流令牌。
// 与推流令牌的差别只在「谁来签、绑不绑设备」——判定仍全在 admission.go：
// 签发时判一次 admitUser，兑换（admitIngest）时再判一次，撤权立即生效。
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"hearth/server/internal/store"
)

const (
	castTicketPrefix = "ct1." // 版本化前缀：admitIngest 据此与推流令牌分流
	castTicketTTL    = 10 * time.Minute
	castTagPrefix    = "cast-" // 设备标签前缀，前端据此把参与者显示成桌面投屏而非 OBS
)

// castTicket 票载荷。字段全是判定要用的，不放展示信息（用户名等仍由服务端现查）。
type castTicket struct {
	UID       int64  `json:"uid"`
	ChannelID int64  `json:"cid"`
	Tag       string `json:"tag"`
	Exp       int64  `json:"exp"`
}

// castSecretMu 串行化首次生成：并发签发下两次生成会互相覆盖，让先签出去的票当场失效。
var castSecretMu sync.Mutex

// castSecret 取签名密钥，留空时生成一把落库（随数据库备份）。
// 不读环境变量：它不是部署要调的旋钮，只是进程要有一把稳定的密钥。
func (a *API) castSecret(ctx context.Context) (string, error) {
	castSecretMu.Lock()
	defer castSecretMu.Unlock()
	v, err := a.st.GetSetting(ctx, "cfg_cast_secret")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if v = strings.TrimSpace(v); v != "" {
		return v, nil
	}
	s := randHex(32)
	if err := a.st.SetSetting(ctx, "cfg_cast_secret", s); err != nil {
		return "", err
	}
	return s, nil
}

func castSign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signCastTicket 组票：ct1.{载荷}.{签名}，两段都是 base64url（可直接做 WHIP 的路径段）。
func signCastTicket(secret string, t castTicket) (string, error) {
	payload, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return castTicketPrefix + body + "." + castSign(secret, payload), nil
}

// parseCastTicket 验签 + 验期，返回载荷。任何不合格都返回错误，调用方一律回同一句文案。
func parseCastTicket(secret, tok string) (castTicket, error) {
	bad := errors.New("设备票无效")
	rest, ok := strings.CutPrefix(tok, castTicketPrefix)
	if !ok {
		return castTicket{}, bad
	}
	body, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return castTicket{}, bad
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return castTicket{}, bad
	}
	if !hmac.Equal([]byte(sig), []byte(castSign(secret, payload))) {
		return castTicket{}, bad
	}
	var t castTicket
	if err := json.Unmarshal(payload, &t); err != nil {
		return castTicket{}, bad
	}
	if t.UID <= 0 || t.ChannelID <= 0 || !strings.HasPrefix(t.Tag, castTagPrefix) {
		return castTicket{}, bad
	}
	if time.Now().Unix() >= t.Exp {
		return castTicket{}, bad
	}
	return t, nil
}

// POST /api/channels/{channel}/cast-ticket {device_id}：桌面端点投屏时现取的设备票。
// 标签绑设备（cast-{device_id}），与 OBS 的 identity 因此不会互相顶替，
// 也不要求推流令牌处于启用态。
func (a *API) castTicketIssue(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	c := channelFrom(r)
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	dev := deviceIDRe.FindString(req.DeviceID)
	if dev == "" {
		writeErr(w, http.StatusBadRequest, "设备标识无效")
		return
	}
	ctx := r.Context()
	adm, ok, reason, err := a.admitUser(ctx, c, u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return
	}
	if !adm.CanPublish {
		writeErr(w, http.StatusForbidden, "你已被禁言，无法投屏")
		return
	}
	alias, ip := a.ingestInstance(ctx)
	if ip == nil {
		writeErr(w, http.StatusServiceUnavailable, "当前没有可用的推流入口")
		return
	}
	secret, err := a.castSecret(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	ticket, err := signCastTicket(secret, castTicket{
		UID: adm.UID, ChannelID: c.ID, Tag: castTagPrefix + dev,
		Exp: time.Now().Add(castTicketTTL).Unix(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// base 与 /api/ingest/token 一致：同源 /providers/{alias}/w/ 绝对地址，拼上频道即完整端点
	base := (&neturl.URL{Scheme: requestScheme(r), Host: r.Host, Path: "/providers/" + alias + "/w/"}).String()
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket": ticket, "base": base, "expires_in": int(castTicketTTL.Seconds()),
	})
}
