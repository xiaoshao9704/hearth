package webui

import (
	"mime"
	"net/http/httptest"
	"strings"
	"testing"
)

// dist/ 里没有嵌入产物（本地未跑过前端构建）时，Handler 返回 nil，这里没有服务器可测；
// 直接断言 mime 表本身已经注册了 .webmanifest，这是本包 init() 的职责，与是否有嵌入产物无关。
func TestWebmanifestMimeTypeRegistered(t *testing.T) {
	got := mime.TypeByExtension(".webmanifest")
	if !strings.HasPrefix(got, "application/manifest+json") {
		t.Fatalf("mime.TypeByExtension(\".webmanifest\") = %q, want application/manifest+json", got)
	}
}

// 若本地存在嵌入产物（跑过 web 构建后 dist/ 非空），进一步端到端断言 Handler 实际返回的 Content-Type。
func TestHandlerServesWebmanifestWithCorrectContentType(t *testing.T) {
	h := Handler()
	if h == nil {
		t.Skip("dist/ 未内嵌前端产物，跳过端到端断言")
	}

	req := httptest.NewRequest("GET", "/manifest.webmanifest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GET /manifest.webmanifest = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/manifest+json") {
		t.Fatalf("Content-Type = %q, want prefix application/manifest+json", ct)
	}
}

// 顺带确认修复没有影响其他扩展名的既有行为。
func TestHandlerServesJSWithCorrectContentType(t *testing.T) {
	h := Handler()
	if h == nil {
		t.Skip("dist/ 未内嵌前端产物，跳过端到端断言")
	}

	req := httptest.NewRequest("GET", "/service-worker.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GET /service-worker.js = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("Content-Type = %q, want prefix text/javascript", ct)
	}
}
