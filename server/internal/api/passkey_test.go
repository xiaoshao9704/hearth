package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hearth/server/internal/store"
)

// ---- 最小软件认证器 ----
//
// 真机的注册/登录响应必须带一枚真签名，所以测试里自带一个 ES256 认证器：
// 它按规范拼 authenticatorData / attestationObject（fmt=none）/ clientDataJSON 并签名，
// 让 origin 校验、挑战一次性、sign_count 单调这几条都能用真实载荷走通。
// 只手写用到的那几种 CBOR 项（无符号整数、负整数、字节串、文本串、定长 map），
// 不为此引入新依赖。

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major | byte(n)}
	case n < 1<<8:
		return []byte{major | 24, byte(n)}
	case n < 1<<16:
		return []byte{major | 25, byte(n >> 8), byte(n)}
	default:
		return []byte{major | 26, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
}

func cborUint(n uint64) []byte  { return cborHead(0x00, n) }
func cborNeg(n int64) []byte    { return cborHead(0x20, uint64(-n-1)) }
func cborBytes(b []byte) []byte { return append(cborHead(0x40, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(cborHead(0x60, uint64(len(s))), s...) }
func cborMap(n int) []byte      { return cborHead(0xa0, uint64(n)) }

// coseES256 P-256 公钥的 COSE_Key 编码（kty=EC2、alg=ES256、crv=P-256、x、y）。
func coseES256(pub *ecdsa.PublicKey) []byte {
	out := cborMap(5)
	out = append(out, cborUint(1)...)
	out = append(out, cborUint(2)...)
	out = append(out, cborUint(3)...)
	out = append(out, cborNeg(-7)...)
	out = append(out, cborNeg(-1)...)
	out = append(out, cborUint(1)...)
	out = append(out, cborNeg(-2)...)
	out = append(out, cborBytes(pub.X.FillBytes(make([]byte, 32)))...)
	out = append(out, cborNeg(-3)...)
	out = append(out, cborBytes(pub.Y.FillBytes(make([]byte, 32)))...)
	return out
}

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

type softAuthn struct {
	key    *ecdsa.PrivateKey
	credID []byte
	aaguid []byte
}

func newSoftAuthn(t *testing.T) *softAuthn {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatal(err)
	}
	return &softAuthn{key: key, credID: credID, aaguid: make([]byte, 16)}
}

func (s *softAuthn) authData(rpID string, flags byte, signCount uint32, attested bool) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], signCount)
	out = append(out, c[:]...)
	if attested {
		out = append(out, s.aaguid...)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(s.credID)))
		out = append(out, l[:]...)
		out = append(out, s.credID...)
		out = append(out, coseES256(&s.key.PublicKey)...)
	}
	return out
}

func clientDataJSON(t *testing.T, typ, challenge, origin string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// register 造一份注册响应（前端 credential.toJSON() 的形状）。
func (s *softAuthn) register(t *testing.T, rpID, origin, challenge string) json.RawMessage {
	t.Helper()
	ad := s.authData(rpID, flagUP|flagUV|flagBE|flagBS|flagAT, 0, true)
	obj := cborMap(3)
	obj = append(obj, cborText("fmt")...)
	obj = append(obj, cborText("none")...)
	obj = append(obj, cborText("attStmt")...)
	obj = append(obj, cborMap(0)...)
	obj = append(obj, cborText("authData")...)
	obj = append(obj, cborBytes(ad)...)
	b, err := json.Marshal(map[string]any{
		"id": b64(s.credID), "rawId": b64(s.credID), "type": "public-key",
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64(clientDataJSON(t, "webauthn.create", challenge, origin)),
			"attestationObject": b64(obj),
			"transports":        []string{"internal", "hybrid"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// assert 造一份登录响应；signCount 由调用方给（回退用例要能给旧值）。
func (s *softAuthn) assert(t *testing.T, rpID, origin, challenge string, userHandle []byte, signCount uint32) json.RawMessage {
	t.Helper()
	ad := s.authData(rpID, flagUP|flagUV|flagBE|flagBS, signCount, false)
	cd := clientDataJSON(t, "webauthn.get", challenge, origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{
		"id": b64(s.credID), "rawId": b64(s.credID), "type": "public-key",
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
		"response": map[string]any{
			"clientDataJSON":    b64(cd),
			"authenticatorData": b64(ad),
			"signature":         b64(sig),
			"userHandle":        b64(userHandle),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- 请求辅助 ----

// beginOpts 一次 begin 的返回：ceremony_id + 原始 options（挑战/rp.id 从这里取）。
type beginOpts struct {
	CeremonyID string `json:"ceremony_id"`
	Options    struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		AuthenticatorSelection struct {
			ResidentKey      string `json:"residentKey"`
			UserVerification string `json:"userVerification"`
		} `json:"authenticatorSelection"`
		Attestation        string           `json:"attestation"`
		AllowCredentials   []map[string]any `json:"allowCredentials"`
		ExcludeCredentials []map[string]any `json:"excludeCredentials"`
	} `json:"options"`
}

// passkeyReq 发一个请求，可覆盖 Host 与 X-Forwarded-Proto（RP 推导用例要用）。
func passkeyReq(t *testing.T, a *API, method, path, token string, body any, host, xfp string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if host != "" {
		req.Host = host
	}
	if xfp != "" {
		req.Header.Set("X-Forwarded-Proto", xfp)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/131.0 Safari/537.36")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	return rec
}

func decodeBegin(t *testing.T, rec *httptest.ResponseRecorder) beginOpts {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("begin 状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var out beginOpts
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.CeremonyID == "" || out.Options.Challenge == "" {
		t.Fatalf("begin 响应缺 ceremony_id/challenge: %s", rec.Body.String())
	}
	return out
}

// passkeyFixture 造一个普通用户并登录，返回 API 与会话 token。
func passkeyFixture(t *testing.T) (*API, *store.User, string) {
	t.Helper()
	a := testAPI(t)
	ctx := context.Background()
	// 首个账号无条件是 super，先垫一个再造被测用户，避免测的是 super
	if _, err := a.st.CreateUser(ctx, "root", mustHash(t, "root-password")); err != nil {
		t.Fatal(err)
	}
	u, err := a.st.CreateUser(ctx, "alice", mustHash(t, "alice-password"))
	if err != nil {
		t.Fatal(err)
	}
	return a, u, loginAs(t, a, "alice", "alice-password", "test-agent/1.0")
}

// addPasskey 走完整的注册两步，返回认证器（后续登录用同一枚）。
func addPasskey(t *testing.T, a *API, token, host, xfp, rpID, origin string) *softAuthn {
	t.Helper()
	begin := decodeBegin(t, passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/begin", token, nil, host, xfp))
	if begin.Options.RP.ID != rpID {
		t.Fatalf("rp.id=%q 期望 %q", begin.Options.RP.ID, rpID)
	}
	if begin.Options.AuthenticatorSelection.ResidentKey != "required" {
		t.Fatalf("residentKey=%q 期望 required", begin.Options.AuthenticatorSelection.ResidentKey)
	}
	if begin.Options.AuthenticatorSelection.UserVerification != "preferred" {
		t.Fatalf("userVerification=%q 期望 preferred", begin.Options.AuthenticatorSelection.UserVerification)
	}
	if begin.Options.Attestation != "none" {
		t.Fatalf("attestation=%q 期望 none", begin.Options.Attestation)
	}
	auth := newSoftAuthn(t)
	rec := passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/finish", token,
		map[string]any{"ceremony_id": begin.CeremonyID, "credential": auth.register(t, rpID, origin, begin.Options.Challenge)},
		host, xfp)
	if rec.Code != http.StatusOK {
		t.Fatalf("注册 finish 状态码=%d: %s", rec.Code, rec.Body.String())
	}
	return auth
}

// passkeyLogin 走完整的登录两步，返回响应记录（状态码由调用方断言）。
func passkeyLogin(t *testing.T, a *API, auth *softAuthn, userID int64, host, xfp, rpID, origin string, signCount uint32) *httptest.ResponseRecorder {
	t.Helper()
	begin := decodeBegin(t, passkeyReq(t, a, http.MethodPost, "/api/auth/passkey/login/begin", "", nil, host, xfp))
	if len(begin.Options.AllowCredentials) != 0 {
		t.Fatalf("登录 begin 的 allowCredentials 应为空（可发现凭证）: %+v", begin.Options.AllowCredentials)
	}
	cred := auth.assert(t, rpID, origin, begin.Options.Challenge, passkeyUserHandle(userID), signCount)
	return passkeyReq(t, a, http.MethodPost, "/api/auth/passkey/login/finish", "",
		map[string]any{"ceremony_id": begin.CeremonyID, "credential": cred}, host, xfp)
}

// ---- 用例 ----

// 注册 → /api/me 计数 → 一键登录 → 新会话可用，整条链路走真签名。
func TestPasskeyRegisterAndLoginRoundTrip(t *testing.T) {
	a, u, token := passkeyFixture(t)
	auth := addPasskey(t, a, token, "example.com", "", "example.com", "http://example.com")

	rec := passkeyReq(t, a, http.MethodGet, "/api/me", token, nil, "example.com", "")
	var me struct {
		ID           int64 `json:"id"`
		PasskeyCount int   `json:"passkey_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.PasskeyCount != 1 {
		t.Fatalf("/api/me 的 passkey_count=%d 期望 1", me.PasskeyCount)
	}

	rec = passkeyLogin(t, a, auth, u.ID, "example.com", "", "example.com", "http://example.com", 7)
	if rec.Code != http.StatusOK {
		t.Fatalf("通行密钥登录状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
		User  struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.User.ID != u.ID {
		t.Fatalf("登录响应形状不对: %s", rec.Body.String())
	}
	// 新会话真的能用（与密码登录同一条签发路径）
	if rec := passkeyReq(t, a, http.MethodGet, "/api/me", resp.Token, nil, "example.com", ""); rec.Code != http.StatusOK {
		t.Fatalf("用通行密钥签出的会话调 /api/me 状态码=%d", rec.Code)
	}
	rows, err := a.st.ListPasskeys(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SignCount != 7 || rows[0].LastUsedAt == nil {
		t.Fatalf("登录后应更新 sign_count/last_used_at: %+v", rows)
	}
	if !rows[0].BackupEligible || !rows[0].BackupState {
		t.Fatalf("备份标记应落库: %+v", rows[0])
	}
	if rows[0].Name != "Chrome · macOS" {
		t.Fatalf("默认名字应按 UA 生成，实际 %q", rows[0].Name)
	}
}

// origin 不在允许列表（凭证是在别的站点采集的）→ 拒。
func TestPasskeyLoginRejectsForeignOrigin(t *testing.T) {
	a, u, token := passkeyFixture(t)
	auth := addPasskey(t, a, token, "example.com", "", "example.com", "http://example.com")
	rec := passkeyLogin(t, a, auth, u.ID, "example.com", "", "example.com", "https://evil.example.com", 3)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("异站 origin 应 401，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 注册也要挡住异站 origin。
func TestPasskeyRegisterRejectsForeignOrigin(t *testing.T) {
	a, _, token := passkeyFixture(t)
	begin := decodeBegin(t, passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/begin", token, nil, "example.com", ""))
	auth := newSoftAuthn(t)
	rec := passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/finish", token,
		map[string]any{"ceremony_id": begin.CeremonyID,
			"credential": auth.register(t, "example.com", "https://evil.example.com", begin.Options.Challenge)},
		"example.com", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("异站 origin 注册应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// ceremony 一次性：同一个 ceremony_id 再用一次必然落空（挑战重放）。
func TestPasskeyCeremonyIsSingleUse(t *testing.T) {
	a, u, token := passkeyFixture(t)
	auth := addPasskey(t, a, token, "example.com", "", "example.com", "http://example.com")

	begin := decodeBegin(t, passkeyReq(t, a, http.MethodPost, "/api/auth/passkey/login/begin", "", nil, "example.com", ""))
	body := map[string]any{
		"ceremony_id": begin.CeremonyID,
		"credential":  auth.assert(t, "example.com", "http://example.com", begin.Options.Challenge, passkeyUserHandle(u.ID), 5),
	}
	if rec := passkeyReq(t, a, http.MethodPost, "/api/auth/passkey/login/finish", "", body, "example.com", ""); rec.Code != http.StatusOK {
		t.Fatalf("首次 finish 应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec := passkeyReq(t, a, http.MethodPost, "/api/auth/passkey/login/finish", "", body, "example.com", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("重放同一个 ceremony_id 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 注册侧同理
	rb := decodeBegin(t, passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/begin", token, nil, "example.com", ""))
	auth2 := newSoftAuthn(t)
	regBody := map[string]any{"ceremony_id": rb.CeremonyID,
		"credential": auth2.register(t, "example.com", "http://example.com", rb.Options.Challenge)}
	if rec := passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/finish", token, regBody, "example.com", ""); rec.Code != http.StatusOK {
		t.Fatalf("首次注册 finish 应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/finish", token, regBody, "example.com", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("重放注册 ceremony 应 400，实际 %d", rec.Code)
	}
}

// sign_count 回退（新值 ≤ 旧值且不都为 0）→ 拒登并留审计。
func TestPasskeySignCountRegressionRejected(t *testing.T) {
	a, u, token := passkeyFixture(t)
	auth := addPasskey(t, a, token, "example.com", "", "example.com", "http://example.com")
	if rec := passkeyLogin(t, a, auth, u.ID, "example.com", "", "example.com", "http://example.com", 9); rec.Code != http.StatusOK {
		t.Fatalf("首次登录应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec := passkeyLogin(t, a, auth, u.ID, "example.com", "", "example.com", "http://example.com", 9)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("计数回退应 401，实际 %d: %s", rec.Code, rec.Body.String())
	}
	entries, err := a.st.ListAudit(context.Background(), store.AuditFilter{Action: store.AuditPasskeyReplay})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ActorUID != u.ID {
		t.Fatalf("应留一条 passkey_replay 审计: %+v", entries)
	}
}

// 停用账号的凭证不能登录。
func TestPasskeyLoginRejectsDisabledUser(t *testing.T) {
	a, u, token := passkeyFixture(t)
	auth := addPasskey(t, a, token, "example.com", "", "example.com", "http://example.com")
	if err := a.st.SetUserDisabled(context.Background(), u.ID, true); err != nil {
		t.Fatal(err)
	}
	rec := passkeyLogin(t, a, auth, u.ID, "example.com", "", "example.com", "http://example.com", 4)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("停用账号应 403，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 访客调不动账号侧的通行密钥接口。
func TestPasskeyGuestForbidden(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	if _, err := a.st.CreateUser(ctx, "root", mustHash(t, "root-password")); err != nil {
		t.Fatal(err)
	}
	g, err := a.st.CreateGuest(ctx, "guest-1", time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.st.CreateSessionWithUA(ctx, g.ID, "test-agent/1.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/account/passkeys"},
		{http.MethodPost, "/api/account/passkeys/begin"},
		{http.MethodPost, "/api/account/passkeys/finish"},
		{http.MethodPatch, "/api/account/passkeys/1"},
		{http.MethodDelete, "/api/account/passkeys/1"},
	} {
		rec := passkeyReq(t, a, tc.method, tc.path, token, nil, "example.com", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("访客 %s %s 应 403，实际 %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// 登录 begin 按来源 IP 限频（同一个 IP 每分钟 20 次）。
func TestPasskeyLoginBeginRateLimited(t *testing.T) {
	a := testAPI(t)
	for i := 0; i < passkeyLoginPerMin; i++ {
		rec := passkeyReq(t, a, http.MethodPost, "/api/auth/passkey/login/begin", "", nil, "example.com", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 begin 应成功，实际 %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := passkeyReq(t, a, http.MethodPost, "/api/auth/passkey/login/begin", "", nil, "example.com", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应 429，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// RP ID / origin 的默认推导：反代（X-Forwarded-Proto: https）与本地 vite dev（localhost:5173）
// 各走一遍完整注册+登录——注册与登录都过说明推导出的 origin 正是浏览器上报的那个。
func TestPasskeyRPDerivation(t *testing.T) {
	for _, tc := range []struct{ name, host, xfp, rpID, origin string }{
		{"反代终止 TLS", "hearth.example.com", "https", "hearth.example.com", "https://hearth.example.com"},
		{"本地开发", "localhost:5173", "", "localhost", "http://localhost:5173"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, u, token := passkeyFixture(t)
			auth := addPasskey(t, a, token, tc.host, tc.xfp, tc.rpID, tc.origin)
			rec := passkeyLogin(t, a, auth, u.ID, tc.host, tc.xfp, tc.rpID, tc.origin, 2)
			if rec.Code != http.StatusOK {
				t.Fatalf("登录应成功，实际 %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// 显式配置 RP ID / origin 时以配置为准（换域名的部署要能对齐）。
func TestPasskeyRPFromConfig(t *testing.T) {
	a, u, token := passkeyFixture(t)
	ctx := context.Background()
	if err := a.st.SetSetting(ctx, "cfg_passkey_rp_id", "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := a.st.SetSetting(ctx, "cfg_passkey_origins", "https://example.com, https://alt.example.com"); err != nil {
		t.Fatal(err)
	}
	// 请求 Host 与配置不同：RP ID 仍取配置值，origin 取列表里的第二个
	auth := addPasskey(t, a, token, "internal.invalid:8080", "", "example.com", "https://alt.example.com")
	if rec := passkeyLogin(t, a, auth, u.ID, "internal.invalid:8080", "", "example.com", "https://example.com", 2); rec.Code != http.StatusOK {
		t.Fatalf("列表内的 origin 应通过，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := passkeyLogin(t, a, auth, u.ID, "internal.invalid:8080", "", "example.com", "https://other.example.com", 3); rec.Code != http.StatusUnauthorized {
		t.Fatalf("列表外的 origin 应 401，实际 %d", rec.Code)
	}
}

// 改名与删除：删掉之后这枚凭证再也登不进来。
func TestPasskeyRenameAndDelete(t *testing.T) {
	a, u, token := passkeyFixture(t)
	auth := addPasskey(t, a, token, "example.com", "", "example.com", "http://example.com")

	rec := passkeyReq(t, a, http.MethodGet, "/api/account/passkeys", token, nil, "example.com", "")
	var list struct {
		Passkeys []store.Passkey `json:"passkeys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Passkeys) != 1 {
		t.Fatalf("列表应有一枚: %s", rec.Body.String())
	}
	// 凭证本体不得出现在响应里
	if strings.Contains(rec.Body.String(), b64(auth.credID)) {
		t.Fatal("列表响应泄漏了 credential_id")
	}
	id := list.Passkeys[0].ID

	if rec := passkeyReq(t, a, http.MethodPatch, "/api/account/passkeys/"+itoa(id), token,
		map[string]any{"name": "  书房那台  "}, "example.com", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("改名应 204，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rows, err := a.st.ListPasskeys(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Name != "书房那台" {
		t.Fatalf("改名后 name=%q", rows[0].Name)
	}

	if rec := passkeyReq(t, a, http.MethodDelete, "/api/account/passkeys/"+itoa(id), token, nil, "example.com", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，实际 %d", rec.Code)
	}
	if rec := passkeyLogin(t, a, auth, u.ID, "example.com", "", "example.com", "http://example.com", 6); rec.Code != http.StatusUnauthorized {
		t.Fatalf("删除后应登不进来，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 同一枚凭证不能重复添加（excludeCredentials 之外的服务端兜底）。
func TestPasskeyDuplicateRejected(t *testing.T) {
	a, _, token := passkeyFixture(t)
	auth := addPasskey(t, a, token, "example.com", "", "example.com", "http://example.com")
	begin := decodeBegin(t, passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/begin", token, nil, "example.com", ""))
	if len(begin.Options.ExcludeCredentials) != 1 {
		t.Fatalf("excludeCredentials 应列出已有的一枚: %+v", begin.Options.ExcludeCredentials)
	}
	rec := passkeyReq(t, a, http.MethodPost, "/api/account/passkeys/finish", token,
		map[string]any{"ceremony_id": begin.CeremonyID,
			"credential": auth.register(t, "example.com", "http://example.com", begin.Options.Challenge)},
		"example.com", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复添加应 409，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 未注册的凭证登录 → 401（反查落空，不泄漏"这个账号存不存在"）。
func TestPasskeyUnknownCredential(t *testing.T) {
	a, u, _ := passkeyFixture(t)
	auth := newSoftAuthn(t)
	rec := passkeyLogin(t, a, auth, u.ID, "example.com", "", "example.com", "http://example.com", 1)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未知凭证应 401，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPasskeyDefaultName(t *testing.T) {
	for _, tc := range []struct{ ua, want string }{
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1", "Safari · iPhone"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/131.0 Safari/537.36 Edg/131.0", "Edge · Windows"},
		{"", "通行密钥"},
	} {
		if got := passkeyDefaultName(tc.ua); got != tc.want {
			t.Fatalf("passkeyDefaultName(%q)=%q 期望 %q", tc.ua, got, tc.want)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
