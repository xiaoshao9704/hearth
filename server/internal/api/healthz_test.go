package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"
	"time"
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

// /api/time 是延迟标尺的时钟源：无需登录、只回一个毫秒时间戳
func TestServerTime(t *testing.T) {
	a := testAPI(t)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, httptest.NewRequest("GET", "/api/time", nil))
	if rec.Code != 200 {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	now, ok := body["now_ms"].(float64)
	if !ok || len(body) != 1 {
		t.Fatalf("应只回 now_ms，实际 %s", rec.Body.String())
	}
	if delta := math.Abs(now - float64(time.Now().UnixMilli())); delta > 5000 {
		t.Fatalf("now_ms 应接近当前时间，差 %.0f ms", delta)
	}
}

// RefreshAnnounce 是进程内周期刷新的入口：刷新 API 持有的 Announcer 与实现了接口的 ingest 实例，不炸即可。
func TestRefreshAnnounce(t *testing.T) {
	testAPI(t).RefreshAnnounce(context.Background())
}
