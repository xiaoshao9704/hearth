package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

// maskProviderEnv 屏蔽部署环境里的内核 env，保证测试从「全新部署」前提出发。
func maskProviderEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{"LIVEKIT_API_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET", "LIVEKIT_URL",
		"INGRESS_UPSTREAM_URL", "BELLOWS_REMOTE_URL", "BELLOWS_SHARED_SECRET",
		"VOICE_PROVIDER", "STAGE_PROVIDER", "INGEST_PROVIDER"} {
		t.Setenv(e, "")
	}
}

// adminToken 造管理员（首个用户自动 is_admin）并签发会话 token。
func adminToken(t *testing.T, a *API) string {
	t.Helper()
	ctx := context.Background()
	u, err := a.st.CreateUser(ctx, "admin", "x")
	if err != nil {
		t.Fatalf("造管理员失败: %v", err)
	}
	token, err := a.st.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("签发会话失败: %v", err)
	}
	return token
}

// doReq 走完整 Router 发请求（含鉴权中间件）。
func doReq(t *testing.T, r *chi.Mux, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// providerViewT 管理后台实例视图的响应形状（web 端依赖此结构）。
type providerViewT struct {
	Alias     string            `json:"alias"`
	Type      string            `json:"type"`
	Caps      []string          `json:"caps"`
	Locked    bool              `json:"locked"`
	Builtin   bool              `json:"builtin"`
	Params    map[string]string `json:"params"`
	ParamsSet map[string]bool   `json:"params_set"`
}

type providerListResp struct {
	Instances []providerViewT `json:"instances"`
	Types     []struct {
		Type   string `json:"type"`
		Label  string `json:"label"`
		Fields []struct {
			Name   string `json:"name"`
			Secret bool   `json:"secret"`
		} `json:"fields"`
	} `json:"types"`
}

func getProviders(t *testing.T, r *chi.Mux, token string) providerListResp {
	t.Helper()
	rec := doReq(t, r, "GET", "/api/admin/providers", token, nil)
	if rec.Code != 200 {
		t.Fatalf("列表应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var resp providerListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析列表响应失败: %v", err)
	}
	return resp
}

func findView(views []providerViewT, alias string) *providerViewT {
	for i := range views {
		if views[i].Alias == alias {
			return &views[i]
		}
	}
	return nil
}

var lkParams = map[string]string{
	"livekit_api_url": "http://10.0.0.2:7880", "livekit_api_key": "k", "livekit_api_secret": "s3",
}

// 列表与类型模式：内建实例可见、可注册类型只报 3 个、Secret 字段掩码；
// 非管理员 403、未登录 401。
func TestAdminProvidersList(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	token := adminToken(t, a)
	r := a.Router()

	resp := getProviders(t, r, token)
	if len(resp.Instances) != 3 || resp.Instances[0].Alias != "ember" ||
		resp.Instances[1].Alias != "bellows" || resp.Instances[2].Alias != AliasLkembed {
		t.Fatalf("全新部署应只有内建实例: %+v", resp.Instances)
	}
	if !resp.Instances[0].Builtin || resp.Instances[0].Caps[0] != "voice" {
		t.Fatalf("ember 应为内建语音实例: %+v", resp.Instances[0])
	}
	if len(resp.Types) != 3 {
		t.Fatalf("可注册类型应为 3 个: %+v", resp.Types)
	}
	for _, typ := range resp.Types {
		if typ.Label == "" || len(typ.Fields) == 0 {
			t.Fatalf("类型模式应带 label 与字段: %+v", typ)
		}
	}

	// 非管理员 → 403；未登录 → 401
	ctx := context.Background()
	u2, err := a.st.CreateUser(ctx, "bob", "x")
	if err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	tok2, _ := a.st.CreateSession(ctx, u2.ID)
	if rec := doReq(t, r, "GET", "/api/admin/providers", tok2, nil); rec.Code != 403 {
		t.Fatalf("非管理员应 403，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, "GET", "/api/admin/providers", "", nil); rec.Code != 401 {
		t.Fatalf("未登录应 401，实际 %d", rec.Code)
	}
}

// 注册 → 列表可见（Secret 掩码 + params_set）→ PUT 更新（空 secret 保留旧值、
// livekit_url 空串清除）→ DELETE 后从注册表消失。
func TestAdminProviderCRUD(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	token := adminToken(t, a)
	r := a.Router()
	ctx := context.Background()

	rec := doReq(t, r, "POST", "/api/admin/providers", token,
		map[string]any{"type": "livekit", "alias": "lk1", "params": lkParams})
	if rec.Code != 201 {
		t.Fatalf("注册应 201，实际 %d: %s", rec.Code, rec.Body.String())
	}
	inst := a.instance("lk1")
	if inst == nil || inst.Voice == nil || inst.Stage == nil {
		t.Fatalf("注册后应立即在注册表可见且具备 voice/stage 能力: %+v", inst)
	}

	// 列表里 secret 掩码为空串、params_set 报告已设置、非 secret 原样可见
	v := findView(getProviders(t, r, token).Instances, "lk1")
	if v == nil {
		t.Fatal("列表应包含 lk1")
	}
	if v.Params["livekit_api_secret"] != "" || !v.ParamsSet["livekit_api_secret"] {
		t.Fatalf("secret 应掩码且 params_set 为 true: %+v", v)
	}
	if v.Params["livekit_api_url"] != lkParams["livekit_api_url"] {
		t.Fatalf("非 secret 字段应原样可见: %+v", v.Params)
	}

	// PUT：url/key 更新，secret 空串 = 保留旧值，livekit_url 空串 = 清除
	rec = doReq(t, r, "PUT", "/api/admin/providers/lk1", token, map[string]any{"params": map[string]string{
		"livekit_api_url": "http://10.0.0.3:7880", "livekit_api_key": "k2",
		"livekit_api_secret": "", "livekit_url": ""}})
	if rec.Code != 204 {
		t.Fatalf("更新应 204，实际 %d: %s", rec.Code, rec.Body.String())
	}
	dbRec, err := a.st.ProviderByAlias(ctx, "lk1")
	if err != nil {
		t.Fatalf("读回实例失败: %v", err)
	}
	if dbRec.Params["livekit_api_secret"] != "s3" {
		t.Fatalf("空 secret 应保留旧值: %+v", dbRec.Params)
	}
	if dbRec.Params["livekit_api_url"] != "http://10.0.0.3:7880" || dbRec.Params["livekit_api_key"] != "k2" {
		t.Fatalf("非 secret 字段应更新: %+v", dbRec.Params)
	}
	if _, ok := dbRec.Params["livekit_url"]; ok {
		t.Fatalf("livekit_url 空串应清除: %+v", dbRec.Params)
	}
	if a.instance("lk1").Params["livekit_api_url"] != "http://10.0.0.3:7880" {
		t.Fatal("更新后注册表应已重建")
	}

	// DELETE → 204，注册表消失
	rec = doReq(t, r, "DELETE", "/api/admin/providers/lk1", token, nil)
	if rec.Code != 204 {
		t.Fatalf("删除应 204，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if a.instance("lk1") != nil {
		t.Fatal("删除后实例应从注册表消失")
	}
}

// 校验：alias 非法 400；保留名/冲突 409；未知类型 400；必填字段缺失 400；
// livekit 的 livekit_url 可省略。
func TestAdminProviderValidation(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	token := adminToken(t, a)
	r := a.Router()

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"大写 alias", map[string]any{"type": "livekit", "alias": "LK1", "params": lkParams}, 400},
		{"保留名 ember", map[string]any{"type": "livekit", "alias": "ember", "params": lkParams}, 409},
		{"保留名 livekit", map[string]any{"type": "livekit", "alias": "livekit", "params": lkParams}, 409},
		{"未知类型", map[string]any{"type": "nope", "alias": "x1", "params": lkParams}, 400},
		{"缺 secret", map[string]any{"type": "livekit", "alias": "x1", "params": map[string]string{
			"livekit_api_url": "http://x", "livekit_api_key": "k"}}, 400},
		{"缺非 secret 字段", map[string]any{"type": "bellows-remote", "alias": "x1", "params": map[string]string{
			"bellows_shared_secret": "s"}}, 400},
	}
	for _, tc := range cases {
		if rec := doReq(t, r, "POST", "/api/admin/providers", token, tc.body); rec.Code != tc.want {
			t.Fatalf("%s: 应 %d，实际 %d: %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}

	// livekit_url 可选：不带也能注册
	rec := doReq(t, r, "POST", "/api/admin/providers", token,
		map[string]any{"type": "livekit", "alias": "lk1", "params": lkParams})
	if rec.Code != 201 {
		t.Fatalf("不带 livekit_url 应能注册，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// alias 冲突 → 409
	rec = doReq(t, r, "POST", "/api/admin/providers", token,
		map[string]any{"type": "livekit", "alias": "lk1", "params": lkParams})
	if rec.Code != 409 {
		t.Fatalf("重复 alias 应 409，实际 %d", rec.Code)
	}
	// PUT 非 secret 必填字段置空 → 400
	rec = doReq(t, r, "PUT", "/api/admin/providers/lk1", token, map[string]any{"params": map[string]string{
		"livekit_api_url": "", "livekit_api_key": "k", "livekit_api_secret": "s"}})
	if rec.Code != 400 {
		t.Fatalf("必填字段置空应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 内建与 env 锁定实例只读：PUT/DELETE 409；不存在的实例 PUT/DELETE 404；
// env 锁定实例的 alias 不可被 POST 占用。
func TestAdminProviderReadOnly(t *testing.T) {
	maskProviderEnv(t)
	t.Setenv("LIVEKIT_API_URL", "http://10.0.0.2:7880")
	t.Setenv("LIVEKIT_API_KEY", "k")
	t.Setenv("LIVEKIT_API_SECRET", "s")
	a := testAPI(t)
	token := adminToken(t, a)
	r := a.Router()

	if inst := a.instance("livekit"); inst == nil || !inst.Locked {
		t.Fatalf("env 应合成锁定的 livekit 实例: %+v", inst)
	}
	putBody := map[string]any{"params": lkParams}
	for _, alias := range []string{"ember", "bellows", "livekit"} {
		if rec := doReq(t, r, "PUT", "/api/admin/providers/"+alias, token, putBody); rec.Code != 409 {
			t.Fatalf("PUT %s 只读实例应 409，实际 %d: %s", alias, rec.Code, rec.Body.String())
		}
		if rec := doReq(t, r, "DELETE", "/api/admin/providers/"+alias, token, nil); rec.Code != 409 {
			t.Fatalf("DELETE %s 只读实例应 409，实际 %d", alias, rec.Code)
		}
	}
	if rec := doReq(t, r, "PUT", "/api/admin/providers/nope", token, putBody); rec.Code != 404 {
		t.Fatalf("PUT 不存在实例应 404，实际 %d", rec.Code)
	}
	if rec := doReq(t, r, "DELETE", "/api/admin/providers/nope", token, nil); rec.Code != 404 {
		t.Fatalf("DELETE 不存在实例应 404，实际 %d", rec.Code)
	}
	// env 锁定实例占用 alias → POST 409
	if rec := doReq(t, r, "POST", "/api/admin/providers", token,
		map[string]any{"type": "livekit", "alias": "livekit", "params": lkParams}); rec.Code != 409 {
		t.Fatalf("与 env 锁定实例冲突应 409，实际 %d", rec.Code)
	}
}

// 删除被选择器引用的实例 → 409「先切换选择器」；两个选择器都覆盖。
func TestAdminProviderDeleteReferenced(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	token := adminToken(t, a)
	r := a.Router()
	ctx := context.Background()

	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk1", Type: TypeLivekit, Params: lkParams}); err != nil {
		t.Fatalf("造实例失败: %v", err)
	}
	a.reloadProviders(ctx)

	for _, sel := range []string{"cfg_voice_provider", "cfg_stage_provider"} {
		if err := a.st.SetSetting(ctx, sel, "lk1"); err != nil {
			t.Fatalf("设选择器失败: %v", err)
		}
		if rec := doReq(t, r, "DELETE", "/api/admin/providers/lk1", token, nil); rec.Code != 409 {
			t.Fatalf("%s 引用时应 409，实际 %d", sel, rec.Code)
		}
		if err := a.st.SetSetting(ctx, sel, ""); err != nil {
			t.Fatalf("清选择器失败: %v", err)
		}
	}
	if rec := doReq(t, r, "DELETE", "/api/admin/providers/lk1", token, nil); rec.Code != 204 {
		t.Fatalf("无引用后删除应 204，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 并发创建+删除后注册表与 DB 一致（reloadMu 串行化「写 DB → 重建」，
// 后写者不会把过期快照换上去让已删除实例复活）。
func TestAdminProviderConcurrentCRUD(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	token := adminToken(t, a)
	r := a.Router()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			alias := fmt.Sprintf("lk%d", i)
			if rec := doReq(t, r, "POST", "/api/admin/providers", token,
				map[string]any{"type": "livekit", "alias": alias, "params": lkParams}); rec.Code != 201 {
				t.Errorf("创建 %s 应 201，实际 %d: %s", alias, rec.Code, rec.Body.String())
				return
			}
			if rec := doReq(t, r, "DELETE", "/api/admin/providers/"+alias, token, nil); rec.Code != 204 {
				t.Errorf("删除 %s 应 204，实际 %d", alias, rec.Code)
			}
		}(i)
	}
	wg.Wait()

	recs, err := a.st.ListProviders(ctx)
	if err != nil {
		t.Fatalf("读 DB 失败: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("DB 里实例应全部删完: %+v", recs)
	}
	for _, inst := range a.listInstances(ctx) {
		if !inst.Builtin {
			t.Fatalf("注册表应只剩内建实例，发现 %+v", inst)
		}
	}
}
