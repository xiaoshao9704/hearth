// 通行密钥（Passkey / WebAuthn）：可发现凭证的一键登录 + 账号里的凭证管理。
//
// 三条约束值得先说清楚：
//   - RP ID 与允许的 origin 按请求现算（两个 dyncfg 键留空时从 Host / X-Forwarded-Proto 推导），
//     所以 *webauthn.WebAuthn 不是进程单例，而是按 (rpID, origins) 缓存的一组实例。
//   - ceremony 状态（挑战）只在内存里活 2 分钟且取用即删：它是一次性的握手上下文，
//     不是会话，进程重启丢掉只意味着用户重来一次。
//   - 登录 begin/finish 是公开接口，按来源 IP 限频；账号侧的四个接口要登录且非访客
//     （访客账号会过期、绑浏览器，给它挂长期凭证没有意义）。
//
// 凭证 ID 与公钥不进日志、不进响应（store.Passkey 的 json 标签已经把它们标成 "-"）。
package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hearth/server/internal/perm"
	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	// 登录页加载时就会静默发起一次 conditional 登录，用户在页面上停留多久这次 ceremony 就要活多久；
	// 太短会让 autofill 里选通行密钥的人撞上「已失效」。
	passkeyCeremonyTTL = 10 * time.Minute
	passkeyLoginPerMin = 20 // 每个来源 IP 每分钟允许的登录 begin/finish 次数
	passkeyNameMax     = 40 // 凭证名字长度上限（rune）
	passkeyBodyLimit   = 16 << 10
)

// passkeyCeremony 一次进行中的注册/登录握手。UserID 仅注册时有值（登录是可发现凭证，
// 开始时还不知道是谁）；Kind 用来防止把登录的挑战拿去完成注册。
type passkeyCeremony struct {
	Session webauthn.SessionData
	UserID  int64
	Kind    string // "login" | "register"
	Expires time.Time
}

// passkeyState 进程内的握手表、RP 实例缓存与登录限频计数。零值可用（map 懒建）。
type passkeyState struct {
	mu         sync.Mutex
	ceremonies map[string]passkeyCeremony
	rps        map[string]*webauthn.WebAuthn
	rates      map[string]clientLogRate
}

// ---- RP 配置 ----

// passkeyHost 取请求的主机名（去端口）。RP ID 必须是域名，不能带端口。
func passkeyHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// passkeyRP 按当前请求取（或建）WebAuthn 实例。
// passkey_rp_id 留空 = 请求 Host 去端口；passkey_origins 留空 = 只允许当前请求的 origin
// （scheme 尊重 X-Forwarded-Proto，所以 nginx 终止 TLS 的部署也能推导出 https://…）。
func (a *API) passkeyRP(r *http.Request) (*webauthn.WebAuthn, error) {
	ctx := r.Context()
	rpID := strings.TrimSpace(a.dynVal(ctx, "passkey_rp_id"))
	if rpID == "" {
		rpID = passkeyHost(r.Host)
	}
	var origins []string
	for _, o := range strings.Split(a.dynVal(ctx, "passkey_origins"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	if len(origins) == 0 {
		origins = []string{requestScheme(r) + "://" + r.Host}
	}
	name := a.cfg.SiteName
	if strings.TrimSpace(name) == "" {
		name = "Hearth"
	}
	key := rpID + "\x00" + name + "\x00" + strings.Join(origins, ",")

	a.passkey.mu.Lock()
	defer a.passkey.mu.Unlock()
	if w, ok := a.passkey.rps[key]; ok {
		return w, nil
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  rpID,
		RPDisplayName:         name,
		RPOrigins:             origins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
	})
	if err != nil {
		// 走到这里几乎只有一种原因：RP ID 不是合法域名（裸 IP 访问，或后台填错）。
		// 请求侧已限频，直接打日志给管理员看，不额外做去重。
		log.Printf("通行密钥配置无效: rp_id=%q origins=%v: %v", rpID, origins, err)
		return nil, err
	}
	if a.passkey.rps == nil {
		a.passkey.rps = make(map[string]*webauthn.WebAuthn)
	}
	// 配置项可改，缓存不该无限长；键数量本就是「域名 × origin 组合」的个位数，超了整桶重来
	if len(a.passkey.rps) > 16 {
		a.passkey.rps = make(map[string]*webauthn.WebAuthn)
	}
	a.passkey.rps[key] = wa
	return wa, nil
}

// ---- ceremony 表 ----

// putCeremony 存一次握手，返回一次性的 ceremony_id；顺手清掉过期项（不起后台协程）。
func (a *API) putCeremony(c passkeyCeremony) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	c.Expires = now.Add(passkeyCeremonyTTL)

	a.passkey.mu.Lock()
	defer a.passkey.mu.Unlock()
	if a.passkey.ceremonies == nil {
		a.passkey.ceremonies = make(map[string]passkeyCeremony)
	}
	for k, e := range a.passkey.ceremonies {
		if now.After(e.Expires) {
			delete(a.passkey.ceremonies, k)
		}
	}
	a.passkey.ceremonies[id] = c
	return id
}

// takeCeremony 取出并删除一次握手（一次性：重放同一个 ceremony_id 必然落空）。
func (a *API) takeCeremony(id, kind string) (passkeyCeremony, bool) {
	a.passkey.mu.Lock()
	defer a.passkey.mu.Unlock()
	c, ok := a.passkey.ceremonies[id]
	if !ok {
		return passkeyCeremony{}, false
	}
	delete(a.passkey.ceremonies, id)
	if c.Kind != kind || time.Now().After(c.Expires) {
		return passkeyCeremony{}, false
	}
	return c, true
}

// allowPasskeyLogin 登录 begin/finish 的按 IP 限频（与 clientLog 同一套滑动窗口口径）。
func (a *API) allowPasskeyLogin(ip string, now time.Time) bool {
	a.passkey.mu.Lock()
	defer a.passkey.mu.Unlock()
	if a.passkey.rates == nil {
		a.passkey.rates = make(map[string]clientLogRate)
	}
	rate := a.passkey.rates[ip]
	if rate.started.IsZero() || now.Sub(rate.started) >= time.Minute {
		rate = clientLogRate{started: now}
	}
	if rate.count >= passkeyLoginPerMin {
		return false
	}
	rate.count++
	a.passkey.rates[ip] = rate
	if len(a.passkey.rates) > 1024 {
		for k, e := range a.passkey.rates {
			if now.Sub(e.started) >= 2*time.Minute {
				delete(a.passkey.rates, k)
			}
		}
	}
	return true
}

func requestIP(r *http.Request) string {
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return ip
	}
	return r.RemoteAddr
}

// ---- webauthn.User 适配 ----

// passkeyUser 把 hearth 的用户 + 它的凭证喂给 go-webauthn。
// WebAuthnID 是 user_id 的 8 字节大端：稳定、不含用户名（用户名可改，绝不能进身份键）。
type passkeyUser struct {
	u     *store.User
	creds []webauthn.Credential
}

func passkeyUserHandle(userID int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(userID))
	return b[:]
}

func (p *passkeyUser) WebAuthnID() []byte                         { return passkeyUserHandle(p.u.ID) }
func (p *passkeyUser) WebAuthnName() string                       { return p.u.Username }
func (p *passkeyUser) WebAuthnDisplayName() string                { return p.u.Username }
func (p *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return p.creds }

// credentialOf 把库里的一行还原成 go-webauthn 的凭证记录。
// 校验只用到 ID / PublicKey / Flags / SignCount，attestation 原文不留（不做 MDS 校验）。
func credentialOf(p store.Passkey) (webauthn.Credential, error) {
	id, err := base64.RawURLEncoding.DecodeString(p.CredentialID)
	if err != nil {
		return webauthn.Credential{}, err
	}
	pub, err := base64.RawURLEncoding.DecodeString(p.PublicKey)
	if err != nil {
		return webauthn.Credential{}, err
	}
	aaguid, _ := base64.RawURLEncoding.DecodeString(p.AAGUID)
	var flags protocol.AuthenticatorFlags = protocol.FlagUserPresent
	if p.UserVerified {
		flags |= protocol.FlagUserVerified
	}
	if p.BackupEligible {
		flags |= protocol.FlagBackupEligible
	}
	if p.BackupState {
		flags |= protocol.FlagBackupState
	}
	var transports []protocol.AuthenticatorTransport
	for _, t := range strings.Split(p.Transports, ",") {
		if t = strings.TrimSpace(t); t != "" {
			transports = append(transports, protocol.AuthenticatorTransport(t))
		}
	}
	return webauthn.Credential{
		ID:            id,
		PublicKey:     pub,
		Transport:     transports,
		Flags:         webauthn.NewCredentialFlags(flags),
		Authenticator: webauthn.Authenticator{AAGUID: aaguid, SignCount: p.SignCount},
	}, nil
}

// passkeyUserOf 取用户与它的全部凭证。
func (a *API) passkeyUserOf(r *http.Request, u *store.User) (*passkeyUser, error) {
	rows, err := a.st.ListPasskeys(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	pu := &passkeyUser{u: u}
	for _, row := range rows {
		c, err := credentialOf(row)
		if err != nil {
			continue // 单行坏了不该让整个账号登不进来；那一枚删掉重加即可
		}
		pu.creds = append(pu.creds, c)
	}
	return pu, nil
}

// ---- 登录 ----

type passkeyFinishReq struct {
	CeremonyID string          `json:"ceremony_id"`
	Credential json.RawMessage `json:"credential"`
	Name       string          `json:"name,omitempty"`
}

func (a *API) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !a.allowPasskeyLogin(requestIP(r), time.Now()) {
		writeErr(w, http.StatusTooManyRequests, "尝试过于频繁，稍后再试")
		return
	}
	wa, err := a.passkeyRP(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "通行密钥不可用：当前站点地址不能作为 RP ID（需要域名或 localhost，不能是裸 IP），或后台的 passkey_rp_id 填错了")
		return
	}
	assertion, session, err := wa.BeginDiscoverableLogin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法发起通行密钥登录")
		return
	}
	id := a.putCeremony(passkeyCeremony{Session: *session, Kind: "login"})
	if id == "" {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony_id": id, "options": assertion.Response})
}

func (a *API) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !a.allowPasskeyLogin(requestIP(r), time.Now()) {
		writeErr(w, http.StatusTooManyRequests, "尝试过于频繁，稍后再试")
		return
	}
	req, ok := decodePasskeyFinish(w, r)
	if !ok {
		return
	}
	wa, err := a.passkeyRP(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "通行密钥不可用：当前站点地址不能作为 RP ID（需要域名或 localhost，不能是裸 IP），或后台的 passkey_rp_id 填错了")
		return
	}
	c, ok := a.takeCeremony(req.CeremonyID, "login")
	if !ok {
		writeErr(w, http.StatusBadRequest, "本次登录已失效，请重新发起")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(req.Credential)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "通行密钥响应无法解析")
		return
	}

	// 由 rawID 反查凭证与它的主人：可发现凭证的整条登录路径都靠这一步定身份，
	// 用户名从不参与（见 CLAUDE.md 的 user_id 铁律）。
	var matched store.Passkey
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		row, err := a.st.PasskeyByCredentialID(r.Context(), base64.RawURLEncoding.EncodeToString(rawID))
		if err != nil {
			return nil, err
		}
		u, err := a.st.UserByID(r.Context(), row.UserID)
		if err != nil {
			return nil, err
		}
		pu, err := a.passkeyUserOf(r, u)
		if err != nil {
			return nil, err
		}
		matched = *row
		return pu, nil
	}

	cred, err := wa.ValidateDiscoverableLogin(handler, c.Session, parsed)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "通行密钥校验失败，请重试")
		return
	}
	// sign_count 回退（新值 ≤ 旧值且不都为 0）= 疑似克隆/重放：拒登并留审计。
	if cred.Authenticator.CloneWarning {
		a.audit(r.Context(), matched.UserID, store.AuditPasskeyReplay, matched.UserID, 0,
			"通行密钥「"+matched.Name+"」签名计数回退，登录已拒绝")
		writeErr(w, http.StatusUnauthorized, "这枚通行密钥的状态异常，已拒绝登录，请删除后重新添加")
		return
	}
	u, err := a.st.UserByID(r.Context(), matched.UserID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "通行密钥校验失败，请重试")
		return
	}
	if u.Disabled {
		writeErr(w, http.StatusForbidden, "账号已被停用，请联系管理员")
		return
	}
	if u.ExpiresAt != nil && !time.Now().Before(*u.ExpiresAt) {
		writeErr(w, http.StatusUnauthorized, "访客身份已到期")
		return
	}
	_ = a.st.UpdatePasskeySignCount(r.Context(), matched.ID, cred.Authenticator.SignCount,
		cred.Flags.BackupState, cred.Flags.UserVerified)
	_ = a.st.UpdatePasskeyLastUsed(r.Context(), matched.ID)
	a.issueSession(w, r, u)
}

// ---- 账号：凭证管理 ----

func (a *API) listPasskeys(w http.ResponseWriter, r *http.Request) {
	rows, err := a.st.ListPasskeys(r.Context(), userFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": rows})
}

func (a *API) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	wa, err := a.passkeyRP(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "通行密钥不可用：当前站点地址不能作为 RP ID（需要域名或 localhost，不能是裸 IP），或后台的 passkey_rp_id 填错了")
		return
	}
	pu, err := a.passkeyUserOf(r, u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	creation, session, err := wa.BeginRegistration(pu,
		webauthn.WithExclusions(webauthn.Credentials(pu.creds).CredentialDescriptors()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法发起通行密钥注册")
		return
	}
	id := a.putCeremony(passkeyCeremony{Session: *session, UserID: u.ID, Kind: "register"})
	if id == "" {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremony_id": id, "options": creation.Response})
}

func (a *API) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	req, ok := decodePasskeyFinish(w, r)
	if !ok {
		return
	}
	wa, err := a.passkeyRP(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "通行密钥不可用：当前站点地址不能作为 RP ID（需要域名或 localhost，不能是裸 IP），或后台的 passkey_rp_id 填错了")
		return
	}
	c, ok := a.takeCeremony(req.CeremonyID, "register")
	if !ok || c.UserID != u.ID {
		writeErr(w, http.StatusBadRequest, "本次添加已失效，请重新发起")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "通行密钥响应无法解析")
		return
	}
	pu, err := a.passkeyUserOf(r, u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	cred, err := wa.CreateCredential(pu, c.Session, parsed)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "通行密钥校验失败，请重试")
		return
	}
	var transports []string
	for _, t := range cred.Transport {
		if t != "" {
			transports = append(transports, string(t))
		}
	}
	name := passkeyName(req.Name)
	if name == "" {
		name = passkeyDefaultName(r.UserAgent())
	}
	row := store.Passkey{
		UserID:         u.ID,
		CredentialID:   base64.RawURLEncoding.EncodeToString(cred.ID),
		PublicKey:      base64.RawURLEncoding.EncodeToString(cred.PublicKey),
		SignCount:      cred.Authenticator.SignCount,
		AAGUID:         base64.RawURLEncoding.EncodeToString(cred.Authenticator.AAGUID),
		Transports:     strings.Join(transports, ","),
		BackupEligible: cred.Flags.BackupEligible,
		BackupState:    cred.Flags.BackupState,
		UserVerified:   cred.Flags.UserVerified,
		Name:           name,
	}
	if err := a.st.CreatePasskey(r.Context(), &row); err != nil {
		if store.IsUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "这枚通行密钥已经添加过了")
			return
		}
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	a.audit(r.Context(), u.ID, store.AuditPasskeyAdd, u.ID, 0, "添加通行密钥「"+name+"」")
	writeJSON(w, http.StatusOK, row)
}

func (a *API) renamePasskey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "通行密钥不存在")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name := passkeyName(req.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "名字不能为空")
		return
	}
	ok, err := a.st.RenamePasskey(r.Context(), userFrom(r).ID, id, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "通行密钥不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deletePasskey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "通行密钥不存在")
		return
	}
	ok, err := a.st.DeletePasskey(r.Context(), u.ID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "通行密钥不存在")
		return
	}
	a.audit(r.Context(), u.ID, store.AuditPasskeyRemove, u.ID, 0, "删除通行密钥 #"+strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}

// ---- 小工具 ----

// decodePasskeyFinish 读 finish 的请求体（带大小上限：凭证 JSON 是外来内容）。
func decodePasskeyFinish(w http.ResponseWriter, r *http.Request) (passkeyFinishReq, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, passkeyBodyLimit)
	var req passkeyFinishReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求内容过大")
			return req, false
		}
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return req, false
	}
	if req.CeremonyID == "" || len(req.Credential) == 0 {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return req, false
	}
	return req, true
}

// passkeyName 清洗用户填的名字：压掉空白、截到上限。
func passkeyName(s string) string {
	return truncateUTF8(strings.Join(strings.Fields(s), " "), passkeyNameMax)
}

// passkeyDefaultName 用户没填名字时按注册时的 UA 生成一个（如「Chrome · macOS」）。
// UA 是不可靠的自述字符串，只用来让人认出「哪一台」，认不出就叫「通行密钥」。
func passkeyDefaultName(ua string) string {
	browser := ""
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	os := ""
	switch {
	case strings.Contains(ua, "iPhone"):
		os = "iPhone"
	case strings.Contains(ua, "iPad"):
		os = "iPad"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	parts := []string{}
	for _, p := range []string{browser, os} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "通行密钥"
	}
	return strings.Join(parts, " · ")
}

// requirePasskeyAccount 账号侧四个接口的门槛：登录且非访客。
func (a *API) requirePasskeyAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !perm.SysAtLeast(userFrom(r), store.RoleUser) {
			writeErr(w, http.StatusForbidden, "访客不能使用通行密钥，注册一个账号即可")
			return
		}
		next.ServeHTTP(w, r)
	})
}
