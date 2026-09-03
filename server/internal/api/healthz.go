package api

import (
	"context"
	"net/http"
	"time"
)

// refreshableAnnouncer 进程内 ICE-Lite 内核（ember / 进程内 bellows）的宣告探测出口。
type refreshableAnnouncer interface {
	RefreshAnnounce(ctx context.Context) (changed bool, externals []string, probedAt time.Time)
	AnnounceSnapshot() (externals []string, probedAt time.Time)
}

// healthz 纯探活：健康只表示进程活着。宣告探测的刷新由 RefreshAnnounce 的周期调用负责，
// 不挂在这个端点上——它匿名可达，触发副作用和回显内网拓扑都不合适。
func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RefreshAnnounce 刷新全部进程内内核的宣告探测，给进程内周期任务与端口映射变化回调用。
func (a *API) RefreshAnnounce(ctx context.Context) {
	if a.ember != nil {
		a.ember.RefreshAnnounce(ctx)
	}
	a.providersMu.RLock()
	var ras []refreshableAnnouncer
	for _, inst := range a.providers {
		if ra, ok := inst.Ingest.(refreshableAnnouncer); ok {
			ras = append(ras, ra)
		}
	}
	a.providersMu.RUnlock()
	for _, ra := range ras {
		ra.RefreshAnnounce(ctx)
	}
}
