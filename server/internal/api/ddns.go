// DDNS 的接入层接线：配置键快照 → ddns.Runner，状态进概览与自检回显。
// 触发节拍与 RefreshAnnounce 相同（端口映射变化回调 + 宣告探测周期任务 + 配置保存），
// 去重与退避在 ddns.Runner 内部。
package api

import (
	"context"
	"time"

	"hearth/server/internal/ddns"
)

// SetDDNS 注入 DDNS 运行器（main 在路由建好后设置；nil = 未启用，回显按 off 报）。
func (a *API) SetDDNS(r *ddns.Runner) { a.ddns = r }

// ddnsConfig 当前 DDNS 相关动态配置快照。
func (a *API) ddnsConfig(ctx context.Context) ddns.Config {
	return ddns.Config{
		Provider:     a.dynVal(ctx, "ddns_provider"),
		Host:         a.dynVal(ctx, "ddns_host"),
		DuckDNSToken: a.dynVal(ctx, "ddns_duckdns_token"),
		CFToken:      a.dynVal(ctx, "ddns_cf_token"),
		DNSPodID:     a.dynVal(ctx, "ddns_dnspod_id"),
		DNSPodToken:  a.dynVal(ctx, "ddns_dnspod_token"),
		AliyunID:     a.dynVal(ctx, "ddns_aliyun_id"),
		AliyunSecret: a.dynVal(ctx, "ddns_aliyun_secret"),
	}
}

// SyncDDNS 按当前配置与公网地址快照 reconcile DDNS（地址没变不会打提供方 API）。
// 调用点：宣告探测刷新后、配置保存后。
func (a *API) SyncDDNS() {
	if a.ddns == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ext, _ := a.AnnounceExternals()
	a.ddns.Sync(ctx, a.ddnsConfig(ctx), ext)
}

// ddnsStatus DDNS 状态回显；未接运行器时按 off 报。
func (a *API) ddnsStatus() ddns.Status {
	if a.ddns == nil {
		return ddns.Status{Provider: "off"}
	}
	return a.ddns.Status()
}
