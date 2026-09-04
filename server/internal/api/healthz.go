package api

import (
	"context"
	"net/http"
)

// healthz 纯探活：健康只表示进程活着。宣告探测的刷新由 RefreshAnnounce 的周期调用负责，
// 不挂在这个端点上——它匿名可达，触发副作用和回显内网拓扑都不合适。
func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RefreshAnnounce 刷新进程内宣告探测（lkembed 的外部地址数据源，见 lkembed.go），
// 给进程内周期任务与端口映射变化回调用。
func (a *API) RefreshAnnounce(ctx context.Context) {
	a.announcer.Refresh(ctx)
}
