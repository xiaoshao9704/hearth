package api

import (
	"context"
	"net/http"
	"time"
)

// healthz 纯探活：健康只表示进程活着。宣告探测的刷新由 RefreshAnnounce 的周期调用负责，
// 不挂在这个端点上——它匿名可达，触发副作用和回显内网拓扑都不合适。
func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// serverTime 给端到端延迟标尺做时钟对齐：无需登录、无副作用，只回服务器当前毫秒时间戳。
// 推流方与观众各自对齐到它，两端不必同机也能把采集时刻与渲染时刻相减出全程延迟。
func (a *API) serverTime(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"now_ms": time.Now().UnixMilli()})
}

// RefreshAnnounce 刷新进程内宣告探测（lkembed 的外部地址数据源，见 lkembed.go），
// 给进程内周期任务与端口映射变化回调用。
func (a *API) RefreshAnnounce(ctx context.Context) {
	a.announcer.Refresh(ctx)
}
