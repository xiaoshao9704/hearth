package api

import (
	"context"
	"encoding/json"
	"testing"
)

// TestAdminTLSShape /api/admin/tls 的响应形状是前后端契约（见 docs/plan-tls.md），
// 键少一个前端就少一块。来源 off：不生成任何证书文件，cert/ca 为 null、/ca.crt 404。
func TestAdminTLSShape(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("TLS_CERT_SOURCE", "")
	a := testAPI(t)
	ctx := context.Background()
	if err := a.st.SetSetting(ctx, "cfg_tls_cert_source", "off"); err != nil {
		t.Fatalf("落库证书来源失败: %v", err)
	}
	a.CheckTLS(ctx)

	r := a.Router()
	token := adminToken(t, a)
	rec := doReq(t, r, "GET", "/api/admin/tls", token, nil)
	if rec.Code != 200 {
		t.Fatalf("状态接口应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	for _, k := range []string{"source", "mode", "http_addr", "https_addr", "cert", "ca",
		"cert_file", "key_file", "external", "portmap"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("响应缺少字段 %s: %s", k, rec.Body.String())
		}
	}
	if got["source"] != "off" || got["mode"] != "merged" {
		t.Fatalf("来源/模式不对: %v %v", got["source"], got["mode"])
	}
	if got["cert"] != nil || got["ca"] != nil {
		t.Fatalf("off 时不该有证书与根证书信息: %v %v", got["cert"], got["ca"])
	}
	ext, _ := got["external"].(map[string]any)
	if _, ok := ext["addresses"]; !ok {
		t.Fatalf("external 应含 addresses: %v", got["external"])
	}
	pm, _ := got["portmap"].(map[string]any)
	for _, k := range []string{"mode", "diagnosis", "detail", "v6_detail", "pinholes"} {
		if _, ok := pm[k]; !ok {
			t.Fatalf("portmap 缺少字段 %s: %v", k, got["portmap"])
		}
	}

	// 非管理员拿不到状态，公开入口在非自签来源下 404。
	if rec := doReq(t, r, "GET", "/api/admin/tls", "", nil); rec.Code != 401 {
		t.Fatalf("未登录应 401，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, "GET", "/ca.crt", "", nil); rec.Code != 404 {
		t.Fatalf("非自签来源 /ca.crt 应 404，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, "GET", "/ca", "", nil); rec.Code != 200 {
		t.Fatalf("安装说明页应可匿名访问，实际 %d", rec.Code)
	}
}

// TestPortWantsHTTPS 分开模式下 https 端口要一并申请映射（合并模式下它就是 http 那条）。
func TestPortWantsHTTPS(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("PORTMAP_MODE", "")
	a := testAPI(t)
	ctx := context.Background()
	has := func() bool {
		for _, w := range a.PortWants(ctx) {
			if w.Desc == "hearth https" {
				return w.Proto == "tcp" && !w.StrictPort
			}
		}
		return false
	}
	if has() {
		t.Fatal("合并模式不该有独立的 https 映射")
	}
	a.cfg.HTTPSAddr = ":8443"
	if !has() {
		t.Fatalf("分开模式应有一条非 Strict 的 https tcp want: %+v", a.PortWants(ctx))
	}
}

// TestSiteTLSFields /api/site 带上前端拼推流地址要用的两项。
func TestSiteTLSFields(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("TLS_CERT_SOURCE", "")
	a := testAPI(t)
	rec := doReq(t, a.Router(), "GET", "/api/site", "", nil)
	if rec.Code != 200 {
		t.Fatalf("/api/site 应 200，实际 %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got["tls_source"] != "self" {
		t.Fatalf("默认证书来源应为 self: %v", got["tls_source"])
	}
	if _, ok := got["http_port"].(float64); !ok {
		t.Fatalf("http_port 应为数字: %v", got["http_port"])
	}
}
