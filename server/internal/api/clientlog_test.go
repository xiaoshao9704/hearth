package api

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClientLogRequiresAuthAndRedactsSecrets(t *testing.T) {
	a := testAPI(t)
	r := a.Router()
	if rec := doReq(t, r, http.MethodPost, "/api/client-log", "", map[string]any{
		"level": "error", "event": "connect_failed",
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码=%d, want %d", rec.Code, http.StatusUnauthorized)
	}

	token := adminToken(t, a)
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(old) })
	rec := doReq(t, r, http.MethodPost, "/api/client-log", token, map[string]any{
		"level":         "error",
		"event":         "connect_failed",
		"session":       "a1b2c3",
		"channel":       "general",
		"role":          "voice",
		"engine":        "livekit",
		"endpoint":      "example.com:443",
		"attempt":       2,
		"elapsed_ms":    15001,
		"online":        true,
		"visibility":    "visible",
		"error_name":    "Error",
		"error_message": "failed wss://example.com/rtc?access_token=secret eyJaaaaaaaaaaa.bbbbbbbbbbb.ccccccccccc",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("状态码=%d, want %d: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	got := out.String()
	for _, secret := range []string{"access_token=secret", "eyJaaaaaaaaaaa.bbbbbbbbbbb.ccccccccccc", token} {
		if strings.Contains(got, secret) {
			t.Fatalf("日志泄漏敏感值 %q: %s", secret, got)
		}
	}
	for _, want := range []string{`event="connect_failed"`, `role="voice"`, `error="failed wss://example.com/rtc?&lt;redacted&gt; &lt;token&gt;"`} {
		if !strings.Contains(got, strings.ReplaceAll(strings.ReplaceAll(want, "&lt;", "<"), "&gt;", ">")) {
			t.Fatalf("日志缺少 %q: %s", want, got)
		}
	}
}

func TestClientLogValidatesSizeAndRate(t *testing.T) {
	a := testAPI(t)
	r := a.Router()
	token := adminToken(t, a)

	if rec := doReq(t, r, http.MethodPost, "/api/client-log", token, map[string]any{
		"level": "info", "event": "Bad Event",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("无效事件状态码=%d, want %d", rec.Code, http.StatusBadRequest)
	}
	if rec := doReq(t, r, http.MethodPost, "/api/client-log", token, map[string]any{
		"level": "error", "event": "window_error", "error_message": strings.Repeat("x", clientLogBodyLimit),
	}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大正文状态码=%d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}

	u, err := a.st.UserByToken(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	a.clientLogRates = map[int64]clientLogRate{u.ID: {started: time.Now(), count: clientLogPerMinute - 1}}
	valid := map[string]any{"level": "info", "event": "connection_state", "online": true}
	if rec := doReq(t, r, http.MethodPost, "/api/client-log", token, valid); rec.Code != http.StatusNoContent {
		t.Fatalf("限额内状态码=%d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec := doReq(t, r, http.MethodPost, "/api/client-log", token, valid); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限状态码=%d, want %d", rec.Code, http.StatusTooManyRequests)
	}
}

// detail 承载失败时的候选/候选对快照：与其它字段同样脱敏，并截到 2000 字符（body 8 KB 上限之内）。
func TestClientLogRedactsAndTruncatesDetail(t *testing.T) {
	a := testAPI(t)
	r := a.Router()
	token := adminToken(t, a)

	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(old) })

	detail := "succeeded udp 203.0.113.7:47720 wss://example.com/rtc?access_token=secret " +
		"eyJaaaaaaaaaaa.bbbbbbbbbbb.ccccccccccc " + strings.Repeat("p", 3000)
	if rec := doReq(t, r, http.MethodPost, "/api/client-log", token, map[string]any{
		"level": "error", "event": "sdk_ice_failed", "detail": detail,
	}); rec.Code != http.StatusNoContent {
		t.Fatalf("状态码=%d, want %d: %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	got := out.String()
	for _, secret := range []string{"access_token=secret", "eyJaaaaaaaaaaa.bbbbbbbbbbb.ccccccccccc"} {
		if strings.Contains(got, secret) {
			t.Fatalf("detail 泄漏敏感值 %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, `detail="succeeded udp 203.0.113.7:47720 wss://example.com/rtc?<redacted> <token> `) {
		t.Fatalf("detail 未按预期脱敏: %s", got)
	}
	if strings.Contains(got, strings.Repeat("p", 2001)) {
		t.Fatalf("detail 未截断到 2000 字符: %s", got)
	}
}
