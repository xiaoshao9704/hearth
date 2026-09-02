package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	a := testAPI(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("healthz 应返回 200，实际 %d", rec.Code)
	}
	var body struct {
		OK       bool                       `json:"ok"`
		Announce map[string]json.RawMessage `json:"announce"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	if !body.OK {
		t.Fatal("ok 应为 true")
	}
	if _, ok := body.Announce["ember"]; !ok {
		t.Fatalf("announce 应含内建 ember: %s", rec.Body.String())
	}
}

// 外部来源（非回环）带 refresh 参数也只回显不探测——门控逻辑本身在
// lite.LoopbackRemote 的单测里覆盖，这里只确认端点行为不炸且不回 5xx。
func TestHealthzRefreshFromExternal(t *testing.T) {
	a := testAPI(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz?refresh=1", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("外部 refresh 请求也应 200（只回显），实际 %d", rec.Code)
	}
}
