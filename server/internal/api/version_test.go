package api

import (
	"net/http/httptest"
	"testing"

	"hearth/server/internal/config"
	"hearth/server/internal/store"
)

// X-Hearth-Version 让前端探测服务端升级并提示刷新；值即 New 收到的构建版本号（main.version 注入）。
func TestVersionHeaderOnAPIRoutes(t *testing.T) {
	s, err := store.Open("sqlite://" + t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const want = "v1.2.3-test"
	r := New(s, config.Load(), nil, want).Router()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/api/site", nil))
	if got := rec.Header().Get("X-Hearth-Version"); got != want {
		t.Fatalf("/api/site 的 X-Hearth-Version 应为 %q，实际 %q", want, got)
	}

	// 只作用于 /api/*，healthz 等探活路由不回填，语义不外溢到静态资源。
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest("GET", "/healthz", nil))
	if got := rec2.Header().Get("X-Hearth-Version"); got != "" {
		t.Fatalf("/healthz 不应带 X-Hearth-Version，实际 %q", got)
	}
}
