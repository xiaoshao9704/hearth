package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hearth/server/internal/store"
)

// deviceFixture 造一个普通用户并登录，返回 API 与会话 token。
func deviceFixture(t *testing.T) (*API, *store.User, string) {
	t.Helper()
	a := testAPI(t)
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "alice", mustHash(t, "alice-password"))
	if err != nil {
		t.Fatal(err)
	}
	return a, u, loginAs(t, a, "alice", "alice-password", "browser/1.0")
}

func deviceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// deviceApproveCode 走一次批准，返回一次性码。
func deviceApproveCode(t *testing.T, a *API, token, verifier string) string {
	t.Helper()
	rec := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/approve", token,
		map[string]any{"challenge": deviceChallenge(verifier)})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve 状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Code == "" {
		t.Fatalf("approve 没返回码: %s", rec.Body.String())
	}
	return out.Code
}

func TestDeviceApproveRequiresLogin(t *testing.T) {
	a, _, _ := deviceFixture(t)
	rec := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/approve", "",
		map[string]any{"challenge": deviceChallenge("v")})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 approve 状态码=%d 期望 401: %s", rec.Code, rec.Body.String())
	}
}

func TestDeviceApproveRejectsBadChallenge(t *testing.T) {
	a, _, token := deviceFixture(t)
	for _, ch := range []string{"", "短", "not base64!!", base64.RawURLEncoding.EncodeToString([]byte("只有十六字节的摘要"))} {
		rec := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/approve", token,
			map[string]any{"challenge": ch})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("challenge=%q 状态码=%d 期望 400: %s", ch, rec.Code, rec.Body.String())
		}
	}
}

// 正常一轮：换到的会话与密码登录同形，且能直接调需鉴权接口；同一个码不能再用。
func TestDeviceExchangeRoundTripAndSingleUse(t *testing.T) {
	a, u, token := deviceFixture(t)
	const verifier = "verifier-0123456789abcdef0123456789abcdef"
	code := deviceApproveCode(t, a, token, verifier)

	rec := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/exchange", "",
		map[string]any{"code": code, "verifier": verifier})
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange 状态码=%d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token string      `json:"token"`
		User  *store.User `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.User == nil || resp.User.ID != u.ID {
		t.Fatalf("exchange 响应与登录不同形: %s", rec.Body.String())
	}
	if rec := doReq(t, a.Router(), http.MethodGet, "/api/me", resp.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("换来的会话不可用: %d %s", rec.Code, rec.Body.String())
	}

	again := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/exchange", "",
		map[string]any{"code": code, "verifier": verifier})
	if again.Code != http.StatusForbidden {
		t.Fatalf("重放同一个码状态码=%d 期望 403: %s", again.Code, again.Body.String())
	}
}

func TestDeviceExchangeRejectsWrongVerifier(t *testing.T) {
	a, _, token := deviceFixture(t)
	code := deviceApproveCode(t, a, token, "right-verifier")
	rec := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/exchange", "",
		map[string]any{"code": code, "verifier": "wrong-verifier"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("错 verifier 状态码=%d 期望 403: %s", rec.Code, rec.Body.String())
	}
	// 错一次也算用掉：码是一次性的，不给第二次试的机会
	if rec := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/exchange", "",
		map[string]any{"code": code, "verifier": "right-verifier"}); rec.Code != http.StatusForbidden {
		t.Fatalf("错过一次后仍能换: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeviceExchangeRejectsExpired(t *testing.T) {
	old := deviceCodeTTL
	deviceCodeTTL = 10 * time.Millisecond
	t.Cleanup(func() { deviceCodeTTL = old })

	a, _, token := deviceFixture(t)
	const verifier = "verifier-expiry"
	code := deviceApproveCode(t, a, token, verifier)
	time.Sleep(30 * time.Millisecond)
	rec := doReq(t, a.Router(), http.MethodPost, "/api/auth/device/exchange", "",
		map[string]any{"code": code, "verifier": verifier})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("过期码状态码=%d 期望 403: %s", rec.Code, rec.Body.String())
	}
}

// 桌面壳的三个本地 origin 内建放行，且 CORS_ORIGIN 收紧成具体域名时依然放行。
func TestCORSAllowsDesktopOrigins(t *testing.T) {
	t.Setenv("CORS_ORIGIN", "https://hearth.example.com")
	a := testAPI(t)
	for _, origin := range desktopOrigins {
		req := httptest.NewRequest(http.MethodOptions, "/api/auth/device/exchange", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		a.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("origin=%s 预检状态码=%d 期望 204", origin, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Fatalf("origin=%s 回的 Allow-Origin=%q", origin, got)
		}
	}
	// 其它来源仍按配置回，桌面放行不等于对谁都放行
	req := httptest.NewRequest(http.MethodOptions, "/api/auth/device/exchange", nil)
	req.Header.Set("Origin", "https://evil.example.net")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://hearth.example.com" {
		t.Fatalf("陌生来源回的 Allow-Origin=%q 期望按配置", got)
	}
}
