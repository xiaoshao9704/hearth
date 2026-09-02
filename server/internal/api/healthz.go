package api

import (
	"context"
	"net/http"
	"time"

	"hearth/server/internal/rtc/lite"
)

// refreshableAnnouncer 进程内 ICE-Lite 内核（ember / 进程内 bellows）的宣告探测出口。
type refreshableAnnouncer interface {
	RefreshAnnounce(ctx context.Context) (changed bool, externals []string, probedAt time.Time)
	AnnounceSnapshot() (externals []string, probedAt time.Time)
}

// healthz 探活 + 宣告探测刷新触发。refresh=1 只接受回环来源（容器内健康检查天然回环），
// 经反代进来的外部请求即使带参数也只回显不探测；叠加 Announcer 的最小间隔，端点无需鉴权。
func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1" && lite.LoopbackRemote(r.RemoteAddr)
	announce := map[string]any{}
	if a.ember != nil {
		announce["ember"] = a.kernelAnnounce(r.Context(), a.ember, refresh)
	}
	a.providersMu.RLock()
	for alias, inst := range a.providers {
		if ra, ok := inst.Ingest.(refreshableAnnouncer); ok {
			announce[alias] = a.kernelAnnounce(r.Context(), ra, refresh)
		}
	}
	a.providersMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "announce": announce})
}

func (a *API) kernelAnnounce(ctx context.Context, ra refreshableAnnouncer, refresh bool) map[string]any {
	var externals []string
	var probedAt time.Time
	if refresh {
		_, externals, probedAt = ra.RefreshAnnounce(ctx)
	} else {
		externals, probedAt = ra.AnnounceSnapshot()
	}
	return map[string]any{"externals": externals, "probed_at": probedAt}
}
