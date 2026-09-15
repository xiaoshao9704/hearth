// 桌面端「浏览器跳转登录」：壳内网页的 origin 是 tauri://localhost，WKWebView 不对普通应用
// 开放 WebAuthn，通行密钥在壳里用不了。替代路径是壳把系统浏览器指向服务器真实域名上的授权页，
// 用户在浏览器里正常登录（密码或通行密钥）并显式点「允许」，服务端签一个一次性短时码，
// 浏览器经 hearth:// 深链唤回应用，应用拿码 + PKCE verifier 换会话。
//
// 深链里只有码，没有会话 token：深链会经过系统 URL 分发，能被别的应用抢注同名 scheme，
// 所以码单独拿还不够——必须配上只在发起方进程里的 verifier 才能换到会话。
//
// 码表放进程内、不建表：它是两分钟有效、用一次即废的握手凭证（同 passkey 的 ceremony），
// 不是会话；进程重启丢掉只意味着用户重走一次授权。
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

// deviceCodeTTL 一次性码的寿命：够用户从浏览器跳回应用，短到丢了也不值钱。变量而非常量，测试要改。
var deviceCodeTTL = 2 * time.Minute

const deviceExchangePerMin = 20 // 每个来源 IP 每分钟允许的换取次数

// deviceGrant 一次已批准但还没换走的授权。
type deviceGrant struct {
	UID       int64
	Challenge string // base64url(sha256(verifier))
	Expires   time.Time
}

// deviceAuthState 进程内的码表与换取限频。零值可用（map 懒建）。
type deviceAuthState struct {
	mu     sync.Mutex
	grants map[string]deviceGrant
	rates  map[string]clientLogRate
}

// deviceChallengeOK 校验 challenge 形如 base64url 无填充的 32 字节摘要。
func deviceChallengeOK(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

// putDeviceGrant 存一次授权，返回一次性码；顺手清掉过期项（不起后台协程）。
func (a *API) putDeviceGrant(g deviceGrant) string {
	code := randHex(16)
	now := time.Now()
	g.Expires = now.Add(deviceCodeTTL)

	a.deviceAuth.mu.Lock()
	defer a.deviceAuth.mu.Unlock()
	if a.deviceAuth.grants == nil {
		a.deviceAuth.grants = make(map[string]deviceGrant)
	}
	for k, e := range a.deviceAuth.grants {
		if now.After(e.Expires) {
			delete(a.deviceAuth.grants, k)
		}
	}
	a.deviceAuth.grants[code] = g
	return code
}

// takeDeviceGrant 取出并删除一次授权（一次性：同一个码重放必然落空）。
func (a *API) takeDeviceGrant(code string) (deviceGrant, bool) {
	a.deviceAuth.mu.Lock()
	defer a.deviceAuth.mu.Unlock()
	g, ok := a.deviceAuth.grants[code]
	if !ok {
		return deviceGrant{}, false
	}
	delete(a.deviceAuth.grants, code)
	if time.Now().After(g.Expires) {
		return deviceGrant{}, false
	}
	return g, true
}

// allowDeviceExchange 换取接口的按 IP 限频（与通行密钥登录同一套滑动窗口口径）。
func (a *API) allowDeviceExchange(ip string, now time.Time) bool {
	a.deviceAuth.mu.Lock()
	defer a.deviceAuth.mu.Unlock()
	if a.deviceAuth.rates == nil {
		a.deviceAuth.rates = make(map[string]clientLogRate)
	}
	rate := a.deviceAuth.rates[ip]
	if rate.started.IsZero() || now.Sub(rate.started) >= time.Minute {
		rate = clientLogRate{started: now}
	}
	if rate.count >= deviceExchangePerMin {
		return false
	}
	rate.count++
	a.deviceAuth.rates[ip] = rate
	if len(a.deviceAuth.rates) > 1024 {
		for k, e := range a.deviceAuth.rates {
			if now.Sub(e.started) >= 2*time.Minute {
				delete(a.deviceAuth.rates, k)
			}
		}
	}
	return true
}

// deviceApprove 浏览器里的授权页在用户点「允许」之后调：为当前登录用户签一个一次性码。
// 需登录——批准的是「谁」由会话说了算，页面不传用户名。
func (a *API) deviceApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Challenge string `json:"challenge"`
	}
	if !decode(w, r, &req) {
		return
	}
	ch := strings.TrimSpace(req.Challenge)
	if !deviceChallengeOK(ch) {
		writeErr(w, http.StatusBadRequest, "授权请求不合法")
		return
	}
	code := a.putDeviceGrant(deviceGrant{UID: userFrom(r).ID, Challenge: ch})
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

// deviceExchange 桌面壳拿深链里的码 + 自己手上的 verifier 换会话。无鉴权（换的就是会话），
// 按来源 IP 限频。失败一律同一句话、同一个状态码：码不存在、过期、verifier 不对
// 分开回报等于给猜码的人指路。
func (a *API) deviceExchange(w http.ResponseWriter, r *http.Request) {
	if !a.allowDeviceExchange(requestIP(r), time.Now()) {
		writeErr(w, http.StatusTooManyRequests, "操作太频繁，稍后再试")
		return
	}
	var req struct {
		Code     string `json:"code"`
		Verifier string `json:"verifier"`
	}
	if !decode(w, r, &req) {
		return
	}
	const denied = "授权码无效或已过期"
	g, ok := a.takeDeviceGrant(strings.TrimSpace(req.Code))
	if !ok {
		writeErr(w, http.StatusForbidden, denied)
		return
	}
	sum := sha256.Sum256([]byte(req.Verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(g.Challenge)) != 1 {
		writeErr(w, http.StatusForbidden, denied)
		return
	}
	u, err := a.st.UserByID(r.Context(), g.UID)
	if err != nil || u == nil || u.Disabled {
		writeErr(w, http.StatusForbidden, denied)
		return
	}
	a.issueSession(w, r, u)
}
