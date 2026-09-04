package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"hearth/server/internal/tlsx"
)

func TestProbeExternalOffUsesPublicBasePort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got := (&API{}).probeExternal(context.Background(), tlsx.Config{Mode: "off"}, netcheckResult{}, srv.URL)
	if got.Verdict != "reachable" || got.URL != srv.URL+"/healthz" {
		t.Fatalf("off 模式应按公开地址的实际端口回探: %+v", got)
	}
}
