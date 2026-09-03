package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	a := testAPI(t)
	for _, target := range []string{"/healthz", "/healthz?refresh=1"} {
		rec := httptest.NewRecorder()
		a.Router().ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != 200 {
			t.Fatalf("%s 应返回 200，实际 %d", target, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应应为 JSON: %v", err)
		}
		// 纯探活：不回显宣告地址（内网拓扑），refresh 参数也已不再受理
		if body["ok"] != true || len(body) != 1 {
			t.Fatalf("%s 应只回 {\"ok\":true}，实际 %s", target, rec.Body.String())
		}
	}
}

// RefreshAnnounce 是进程内周期刷新的入口：遍历内建 ember 与实现了接口的 ingest 实例，不炸即可。
func TestRefreshAnnounce(t *testing.T) {
	testAPI(t).RefreshAnnounce(context.Background())
}
