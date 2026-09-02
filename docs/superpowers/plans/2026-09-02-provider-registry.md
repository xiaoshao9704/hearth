# Provider 注册制 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 把 livekit / livekit-ingress / 远程 bellows 三种外部服务改为注册制（DB 无限实例 + alias 命名 + env 锁定实例 + 内建默认），接入路径统一为 `/providers/{alias}/...`。

**Architecture:** 实例即对象——每条注册构造独立的 rtc.Provider/IngestProvider 对象，api 持 `map[alias]实例` 注册表；选择器值从实现名改为实例 alias；反代与 WS/推流入口按路径 alias 分发。spec 见下。

**Tech Stack:** Go 1.27、chi v5、sqlite/mysql/pg 三方言 store、Solid+vanilla TS 前端。

**Spec:** `docs/superpowers/specs/2026-09-02-provider-registry-design.md`（先读它再动手）

## Global Constraints

- 构建验证：`cd server && go build ./... && go vet ./... && go test -race ./...`；前端 `cd web && npx tsc --noEmit && npm run build`。
- **URL 处理默认用 `net/url.URL`**：解析与拼接都走结构体（`url.Parse`、改 Scheme/Host/Path），禁止手写字符串拼 URL；仅反代剥前缀这类纯 path 操作可用字符串。
- 代码注释、面向用户错误文案用中文；标识符、日志键、技术术语保留英文；注释只写代码说不清的约束。
- 最小改动：不顺手重构；rtc 中性接口（rtc.go）不改签名。
- **不执行任何 git 写操作**（commit/tag/push），除非用户明确要求——本计划的验证步骤只到 build/vet/test。
- 旧配置键名在实例 params 里原样复用（`livekit_api_url` 等），rtc 实现代码零改动即可承接。

---

### Task 1: store — providers 表 + CRUD + ingress 记录迁移

**Files:**
- Create: `server/internal/store/providers.go`
- Modify: `server/internal/store/store.go`（三个方言 DDL 块 + migrate compat）
- Test: `server/internal/store/providers_test.go`

**Interfaces:**
- Produces（后续任务依赖的确切签名）:
```go
type ProviderRecord struct {
	Alias     string            `json:"alias"`
	Type      string            `json:"type"`
	Params    map[string]string `json:"params"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}
func (s *Store) CreateProvider(ctx context.Context, rec *ProviderRecord) error          // alias 冲突 → IsUniqueViolation
func (s *Store) ProviderByAlias(ctx context.Context, alias string) (*ProviderRecord, error) // 不存在 → ErrNotFound
func (s *Store) ListProviders(ctx context.Context) ([]*ProviderRecord, error)           // 按 created_at 升序
func (s *Store) UpdateProviderParams(ctx context.Context, alias string, params map[string]string) error
func (s *Store) DeleteProvider(ctx context.Context, alias string) error
```

- [x] **Step 1: 写失败测试** `providers_test.go`

```go
package store

import (
	"context"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	f := t.TempDir() + "/test.db"
	s, err := Open("sqlite://" + f) // 按 store.Open 现有 DSN 形式调整
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProviderCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := &ProviderRecord{Alias: "lk-main", Type: "livekit",
		Params: map[string]string{"livekit_api_url": "http://127.0.0.1:7880", "livekit_api_secret": "s3cret"}}
	if err := s.CreateProvider(ctx, rec); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	got, err := s.ProviderByAlias(ctx, "lk-main")
	if err != nil || got.Type != "livekit" || got.Params["livekit_api_secret"] != "s3cret" {
		t.Fatalf("读回不一致: %+v err=%v", got, err)
	}
	if err := s.CreateProvider(ctx, rec); !IsUniqueViolation(err) {
		t.Fatalf("重复 alias 应唯一冲突，实际 %v", err)
	}
	if err := s.UpdateProviderParams(ctx, "lk-main", map[string]string{"livekit_api_url": "http://x:7880"}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	got, _ = s.ProviderByAlias(ctx, "lk-main")
	if len(got.Params) != 1 || got.Params["livekit_api_url"] != "http://x:7880" {
		t.Fatalf("更新后 params 应整体替换: %+v", got.Params)
	}
	if _, err := s.ProviderByAlias(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("不存在应 ErrNotFound，实际 %v", err)
	}
	if err := s.DeleteProvider(ctx, "lk-main"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := s.ProviderByAlias(ctx, "lk-main"); err != ErrNotFound {
		t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `cd server && go test ./internal/store/ -run TestProviderCRUD -v`
Expected: 编译失败（ProviderRecord 未定义）

- [x] **Step 3: 实现**

`store.go` 三个方言 DDL 块各加（风格对齐现有表：sqlite/mysql 用现有片段的列类型，pg 块用 `TIMESTAMP`）：

```sql
CREATE TABLE IF NOT EXISTS providers (
  alias VARCHAR(64) PRIMARY KEY,
  type VARCHAR(32) NOT NULL,
  params TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
)
```

`migrate()` 的 compat 列表后追加一次性数据迁移（幂等，新代码不会再写旧值）：

```go
// 注册制迁移：ingresses.provider 从实现名改为实例 alias，旧值 livekit 指 livekit-ingress 实例
if _, err := s.db.Exec(`UPDATE ingresses SET provider='livekit-ingress' WHERE provider='livekit'`); err != nil {
	return err
}
```

`providers.go` 实现 CRUD：params 字段 JSON 编解码（`json.Marshal`/`Unmarshal` 到 TEXT 列），查询用 `s.q()` rebind，`CreateProvider` 用 `IsUniqueViolation` 可判的唯一冲突；`UpdateProviderParams` 同时刷新 `updated_at`（`UPDATE providers SET params=?, updated_at=CURRENT_TIMESTAMP WHERE alias=?`，影响 0 行 → ErrNotFound）。

- [x] **Step 4: 跑测试确认通过 + 全量构建**

Run: `cd server && go test -race ./internal/store/ && go build ./... && go vet ./...`
Expected: PASS

---

### Task 2: livekitrtc 拆分 — Provider 只管语音/舞台，新增 Ingress 类型

**Files:**
- Modify: `server/internal/rtc/livekitrtc/livekitrtc.go`
- Create: `server/internal/rtc/livekitrtc/ingress.go`
- Modify: `server/internal/api/api.go`（New 里内核注册表的最小接线，保持编译绿）
- Modify: `server/internal/api/dyncfg.go`（ingest 选择器选项 `livekit`→`livekit-ingress`）

**Interfaces:**
- Produces:
```go
// livekitrtc.go：Provider 只保留 rtc.Provider + rtc.StageProvider 能力
func ConfigKeys() []rtc.ConfigKey // 只剩 livekit_api_url/livekit_api_key/livekit_api_secret/livekit_url

// ingress.go：
func IngressKeys() []rtc.ConfigKey // livekit_api_url/key/secret + ingress_upstream_url
type Ingress struct{ /* cfg rtc.ConfigFunc + 缓存的 lkingress.Client */ }
func NewIngress(cfg rtc.ConfigFunc) *Ingress
// 实现 rtc.IngestProvider：Name()="livekit-ingress"；Enabled=api_url/key/secret/upstream 四者非空；
// PublicBase 恒 ""；ProxyUpstream=ingress_upstream_url
```

- [x] **Step 1: 拆分实现**

`livekitrtc.go`：删 `Enabled/CreateEndpoint/DeleteEndpoint/PublicBase/ProxyUpstream` 五个 IngestProvider 方法与 `ing` 客户端字段；`clients()` 只留 `lkroom.Client`；`ConfigKeys()` 删 ingress_* 两项。`JoinCredentials` 里 `livekit_url` 保留（实例可选覆盖浏览器地址）。

`ingress.go` 全文：

```go
// LiveKit Ingress 推流入口：端点管理走 LiveKit Twirp API（lkingress），
// WHIP 信令反代到 ingress_upstream_url。与 livekitrtc.Provider 分实例后，
// 凭证字段在实例 params 里重复声明（实例即对象，互不引用）。
package livekitrtc

import (
	"context"
	"sync"

	"hearth/server/internal/lkingress"
	"hearth/server/internal/rtc"
)

// IngressKeys 注册表单的参数字段（兼作 env 锁定实例的探测键）。
func IngressKeys() []rtc.ConfigKey {
	return []rtc.ConfigKey{
		{Name: "livekit_api_url", Env: "LIVEKIT_API_URL", Label: "Twirp API 地址", Hint: "端点管理用的 LiveKit API 地址"},
		{Name: "livekit_api_key", Env: "LIVEKIT_API_KEY", Label: "API Key"},
		{Name: "livekit_api_secret", Env: "LIVEKIT_API_SECRET", Secret: true, Label: "API Secret"},
		{Name: "ingress_upstream_url", Env: "INGRESS_UPSTREAM_URL", Label: "WHIP 上游地址", Hint: "ingress 进程的 WHIP 监听地址"},
	}
}

// Ingress 实现 rtc.IngestProvider（LiveKit Ingress）。
type Ingress struct {
	cfg rtc.ConfigFunc

	mu               sync.Mutex
	url, key, secret string
	ing              *lkingress.Client
}

func NewIngress(cfg rtc.ConfigFunc) *Ingress { return &Ingress{cfg: cfg} }

func (i *Ingress) Name() string { return "livekit-ingress" }

func (i *Ingress) client(ctx context.Context) *lkingress.Client {
	url, key, secret := i.cfg(ctx, "livekit_api_url"), i.cfg(ctx, "livekit_api_key"), i.cfg(ctx, "livekit_api_secret")
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ing == nil || url != i.url || key != i.key || secret != i.secret {
		i.url, i.key, i.secret = url, key, secret
		i.ing = lkingress.NewClient(url, key, secret)
	}
	return i.ing
}

func (i *Ingress) Enabled(ctx context.Context) bool {
	return i.cfg(ctx, "livekit_api_key") != "" && i.cfg(ctx, "livekit_api_secret") != "" &&
		i.cfg(ctx, "ingress_upstream_url") != ""
}

func (i *Ingress) CreateEndpoint(ctx context.Context, room, username string) (string, string, error) {
	return i.client(ctx).Create(ctx, room, username)
}

func (i *Ingress) DeleteEndpoint(ctx context.Context, id string) error {
	return i.client(ctx).Delete(ctx, id)
}

// PublicBase 恒空：推流地址由接入层按 /providers/{alias}/w/ 推导。
func (i *Ingress) PublicBase(context.Context) string { return "" }

func (i *Ingress) ProxyUpstream(ctx context.Context) string {
	return i.cfg(ctx, "ingress_upstream_url")
}
```

`api.go` 最小接线（Task 3 会整体换成注册表）：`a.ingestKernels = map[string]rtc.IngestProvider{"livekit-ingress": livekitrtc.NewIngress(a.dynVal), "bellows": pw}`；`dyncfg.go` 选择器 `ingest_provider` Options 改 `[]string{"livekit-ingress", "bellows"}`，`dynVal` 里 ingest 默认回落仍 `livekit` 暂留（Task 3 改）。

- [x] **Step 2: 构建验证**

Run: `cd server && go build ./... && go vet ./... && go test -race ./...`
Expected: 全绿（bellows 既有测试不受影响）

---

### Task 3: api 实例注册表 + 配置迁移

**Files:**
- Create: `server/internal/api/providers.go`
- Modify: `server/internal/api/api.go`（API 结构体换注册表字段、New 重接线、deleteOldEndpoint/createIngress 改按 alias）
- Modify: `server/internal/api/dyncfg.go`（选择器默认值与回落、校验钩子）
- Test: `server/internal/api/providers_test.go`

**Interfaces:**
- Consumes: store 的 `ProviderRecord` CRUD（Task 1）；`livekitrtc.New/NewIngress`（Task 2）
- Produces:
```go
// 实例类型（内建两类 + 可注册三类）
const (
	TypeLivekit        = "livekit"
	TypeLivekitIngress = "livekit-ingress"
	TypeBellowsRemote  = "bellows-remote"
	TypeEmber          = "ember"   // 内建
	TypeBellows        = "bellows" // 内建（进程内 WHIP 直通）
)

type ProviderInstance struct {
	Alias   string
	Type    string
	Params  map[string]string
	Locked  bool // env 锁定，只读
	Builtin bool
	Voice   rtc.Provider       // livekit/ember 非空
	Stage   rtc.StageProvider  // livekit 非空
	Ingest  rtc.IngestProvider // livekit-ingress/bellows/bellows-remote 非空
}
func (i *ProviderInstance) Caps() []string // 如 ["voice","stage"] / ["ingest"]

func (a *API) reloadProviders(ctx context.Context)
func (a *API) instance(alias string) *ProviderInstance
func (a *API) listInstances(ctx context.Context) []*ProviderInstance // 内建 → env 锁定 → DB（按创建序）
func (a *API) voiceInstance(ctx context.Context) (string, rtc.Provider)        // 回落 ember
func (a *API) stageInstance(ctx context.Context) (string, rtc.StageProvider)   // none/无能力 → ("", nil)
func (a *API) ingestInstance(ctx context.Context) (string, rtc.IngestProvider) // 回落内建 bellows
func (a *API) providerTypeFields(typ string) []rtc.ConfigKey                   // 注册表单字段模式
func (a *API) migrateProviders(ctx context.Context)                            // 启动一次性迁移
```

- [x] **Step 1: 写失败测试** `providers_test.go`（API 结构体需要 store，用 sqlite 临时库；env 用 `t.Setenv`）

```go
package api

import (
	"context"
	"testing"

	"hearth/server/internal/store"
)

func testAPI(t *testing.T) *API {
	t.Helper()
	s, err := store.Open("sqlite://" + t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, config.Load(), chat.NewHub(s, "")) // 签名按现状对齐
}

func TestBuiltinInstancesFirst(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	a.reloadProviders(ctx)
	list := a.listInstances(ctx)
	if len(list) < 2 || list[0].Alias != "ember" || list[1].Alias != "bellows" {
		t.Fatalf("内建实例应排最前: %+v", list)
	}
	if !list[0].Builtin || list[0].Voice == nil || list[1].Ingest == nil {
		t.Fatal("ember 应有语音能力，bellows 应有推流能力")
	}
}

func TestEnvLockedInstance(t *testing.T) {
	t.Setenv("LIVEKIT_API_KEY", "k")
	t.Setenv("LIVEKIT_API_SECRET", "s")
	t.Setenv("LIVEKIT_API_URL", "http://10.0.0.2:7880")
	a := testAPI(t)
	a.reloadProviders(context.Background())
	inst := a.instance("livekit")
	if inst == nil || !inst.Locked || inst.Type != TypeLivekit {
		t.Fatalf("env 应合成锁定的 livekit 实例: %+v", inst)
	}
	if inst.Params["livekit_api_url"] != "http://10.0.0.2:7880" {
		t.Fatalf("params 应取自 env: %+v", inst.Params)
	}
}

func TestSelectorResolutionAndFallback(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	// 未设置选择器：默认 ember / none / bellows（内建优先）
	if alias, _ := a.voiceInstance(ctx); alias != "ember" {
		t.Fatalf("voice 默认应为 ember，实际 %q", alias)
	}
	if _, sp := a.stageInstance(ctx); sp != nil {
		t.Fatal("stage 默认应为 none")
	}
	if alias, _ := a.ingestInstance(ctx); alias != "bellows" {
		t.Fatalf("ingest 默认应为 bellows，实际 %q", alias)
	}
	// 注册一套 livekit 并选中
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk2", Type: TypeLivekit,
		Params: map[string]string{"livekit_api_url": "http://x", "livekit_api_key": "k", "livekit_api_secret": "s"}})
	a.reloadProviders(ctx)
	a.st.SetSetting(ctx, "cfg_voice_provider", "lk2")
	if alias, _ := a.voiceInstance(ctx); alias != "lk2" {
		t.Fatalf("voice 应解析到 lk2，实际 %q", alias)
	}
	// 选了无对应槽位能力的实例 → 回落
	a.st.SetSetting(ctx, "cfg_voice_provider", "lk2")
	a.st.SetSetting(ctx, "cfg_ingest_provider", "lk2") // livekit 无推流能力
	if alias, _ := a.ingestInstance(ctx); alias != "bellows" {
		t.Fatalf("无推流能力的实例应回落 bellows，实际 %q", alias)
	}
}

func TestMigrateImportsLegacyCfg(t *testing.T) {
	a := testAPI(t)
	ctx := context.Background()
	a.st.SetSetting(ctx, "cfg_livekit_api_url", "http://old:7880")
	a.st.SetSetting(ctx, "cfg_livekit_api_key", "k")
	a.st.SetSetting(ctx, "cfg_livekit_api_secret", "s")
	a.st.SetSetting(ctx, "cfg_ingest_provider", "livekit") // 旧值：livekit 的 ingress 面
	a.st.SetSetting(ctx, "cfg_ingress_upstream_url", "http://old:58080")
	a.migrateProviders(ctx)
	if a.instance("livekit") == nil || a.instance("livekit").Locked {
		t.Fatal("旧 cfg_livekit_* 应导入为 DB 实例 livekit")
	}
	ing := a.instance("livekit-ingress")
	if ing == nil || ing.Params["ingress_upstream_url"] != "http://old:58080" {
		t.Fatalf("ingress 旧键应导入 livekit-ingress 实例: %+v", ing)
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_livekit_api_url"); v != "" {
		t.Fatal("导入后旧 cfg_ 键应删除")
	}
	if v, _ := a.st.GetSetting(ctx, "cfg_ingest_provider"); v != "livekit-ingress" {
		t.Fatalf("旧选择器值应改写为 livekit-ingress，实际 %q", v)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `cd server && go test ./internal/api/ -run 'TestBuiltin|TestEnv|TestSelector|TestMigrate' -v`
Expected: 编译失败（符号未定义）

- [x] **Step 3: 实现 `providers.go`**

要点（完整逻辑，非伪码）：

```go
// alias 规则：单段小写，出现在 URL 路径里；类型同名的 alias 保留给 env 锁定实例
var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var reservedAliases = map[string]bool{
	TypeEmber: true, TypeBellows: true, TypeLivekit: true,
	TypeLivekitIngress: true, TypeBellowsRemote: true,
}

// 注册表单字段模式（值在实例 params 里复用旧键名，rtc 实现零改动）
func (a *API) providerTypeFields(typ string) []rtc.ConfigKey {
	switch typ {
	case TypeLivekit:
		return livekitrtc.ConfigKeys()
	case TypeLivekitIngress:
		return livekitrtc.IngressKeys()
	case TypeBellowsRemote:
		return bellows.RemoteKeys() // bellows_remote_url + bellows_shared_secret，见 Step 4
	}
	return nil
}

// paramsCfg 把实例 params 包成 rtc.ConfigFunc（实例即对象的关键）
func paramsCfg(params map[string]string) rtc.ConfigFunc {
	return func(_ context.Context, name string) string { return params[name] }
}
```

`reloadProviders`：重建 `a.providers`（`map[string]*ProviderInstance`，RWMutex 保护）：
1. 内建：`ember`（Voice=a.ember）；`bellows`（Ingest=`bellows.New(builtinBellowsCfg, a.ingressResolver)`，见下）。
2. env 锁定：按 `providerTypeFields` 里各字段的 Env 探测——livekit 三件套任一存在 → 合成 alias=livekit 实例，params 从 env 读全字段；`INGRESS_UPSTREAM_URL` → livekit-ingress；`BELLOWS_REMOTE_URL` → bellows-remote（`bellows.New(paramsCfg, nil)`）。
3. DB：`ListProviders` 逐条构造（livekit → `livekitrtc.New(paramsCfg)`；livekit-ingress → `livekitrtc.NewIngress(paramsCfg)`；bellows-remote → `bellows.New(paramsCfg, nil)`）。

内建 bellows 的 ConfigFunc（livekit_* 动态路由到舞台线生效的 livekit 实例；端口/IP 走全局 dynVal）：

```go
func (a *API) builtinBellowsCfg(ctx context.Context, name string) string {
	if strings.HasPrefix(name, "livekit_") {
		if _, sp := a.stageInstance(ctx); sp != nil {
			if inst := a.instance(a.dynVal(ctx, "stage_provider")); inst != nil && inst.Type == TypeLivekit {
				return inst.Params[name]
			}
		}
		return ""
	}
	return a.dynVal(ctx, name)
}
```

注意 `a.ingressResolver` 即 api.New 里现有的 resolve 闭包（ingressOwner → ErrUnknownKey 映射），原样保留给内建实例。

选择器解析（`dynVal` 回落默认值改为：`voice_provider`→`ember`、`stage_provider`→`none`、`ingest_provider`→`bellows`）：

```go
func (a *API) voiceInstance(ctx context.Context) (string, rtc.Provider) {
	if inst := a.instance(a.dynVal(ctx, "voice_provider")); inst != nil && inst.Voice != nil {
		return inst.Alias, inst.Voice
	}
	return "ember", a.instance("ember").Voice
}
// stageInstance："none" 或无 Stage 能力 → ("", nil)；ingestInstance 同理回落 "bellows"
```

`migrateProviders`（api.New 里在首次 reloadProviders 之前调）：
1. 旧 cfg_ 键导入：`cfg_livekit_api_url/key/secret/livekit_url` 任一非空且 DB 无 alias=livekit → 建 DB 实例（type=livekit，params 用旧键名），导入后 `SetSetting(key, "")` 清掉旧键；`cfg_ingress_upstream_url` 非空 → livekit-ingress 实例（params 含 livekit 三件套 + upstream）；`cfg_bellows_remote_url` 非空 → bellows-remote 实例。
2. 选择器改写：`cfg_ingest_provider == "livekit"` → 写 `livekit-ingress`。
3. 选择器默认落库（升级后行为不变的兜底）：`VOICE_PROVIDER` env 与 `cfg_voice_provider` 都为空时，若 livekit 实例存在（env 锁定或刚导入）→ 写 `cfg_voice_provider=livekit`；stage 同理；ingest 在 livekit-ingress 实例存在时写 `livekit-ingress`。
4. 末尾 `a.reloadProviders(ctx)`。

`api.go` 结构体：`voiceKernels/stageKernels/ingestKernels` 三个 map 删除，换成 `providersMu sync.RWMutex` + `providers map[string]*ProviderInstance`；`voiceProvider/stageProvider/ingestProvider` 三个取值函数删除，调用点全改走 `voiceInstance/stageInstance/ingestInstance`（返回 (alias, 对象)，调用点需要对象的取第二位）。`deleteOldEndpoint`：`ik := a.instance(rec.Provider)`，为 nil 或其 Ingest 为 nil → 打日志跳过内核侧删除；WHIPGrantIssuer 撤销逻辑不变（对 inst.Ingest 断言）。`createIngress`：`ip.Name()` 改为存当前 ingest 实例的 alias。

- [x] **Step 4: bellows.RemoteKeys + ConfigKeys 收敛**

`bellows.go`：`ConfigKeys()` 删 `bellows_remote_url`、`bellows_shared_secret` 两项（移入新增 `RemoteKeys()`，作注册表单字段模式），保留 `bellows_udp_port`/`bellows_public_ip`（内建进程内形态的全局键）。Enabled 等实现不变（参数来源换成实例 params 是注册表的事）。

- [x] **Step 5: 跑测试 + 全量验证**

Run: `cd server && go test -race ./internal/api/ && go build ./... && go vet ./... && go test -race ./...`
Expected: 全绿

---

### Task 4: 反代重写 — /providers/{alias} 分发

**Files:**
- Modify: `server/internal/api/proxy.go`（整文件重写分发逻辑）
- Modify: `server/internal/api/api.go`（Router 删 `/api/voice`；fillCred/signalURL/ingressURL 改新路径）
- Modify: `server/internal/api/voice.go`（如有路径假设，对齐）
- Test: `server/internal/api/proxy_test.go`

**Interfaces:**
- Consumes: Task 3 的 `instance()` / `ingestInstance()` / `voiceInstance()` / `stageInstance()`
- Produces:
```go
func (a *API) RegisterProxies(r chi.Router)          // 只挂 /providers/*
func (a *API) serveProvider(w http.ResponseWriter, r *http.Request) // 按 alias + 子路径分发
// 接入路径形状：
//   /providers/{alias}/rtc/*   livekit 信令反代（剥 /providers/{alias}）
//   /providers/ember/voice     ember WS 信令（原 /api/voice 处理函数）
//   /providers/{alias}/w[/*]   WHIP（三类推流实例；r.URL.Path 改写为 /w 段后复用现有逻辑）
```

- [x] **Step 1: 写失败测试** `proxy_test.go`

```go
// 分发：未知 alias 404；livekit /rtc 反代到实例 api_url；ember /voice 到达 WS 处理器（无升级头 → 非 404）；
// WHIP POST 按路径 alias 裁决（bellows-remote 实例：假 key 404、真 key 带 grant 头反代）
func TestProviderDispatch(t *testing.T) {
	a := testAPI(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Path", r.URL.Path)
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	ctx := context.Background()
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "lk1", Type: TypeLivekit, Params: map[string]string{
		"livekit_api_url": upstream.URL, "livekit_api_key": "k", "livekit_api_secret": "s"}})
	a.reloadProviders(ctx)

	r := a.Router()
	a.RegisterProxies(r)

	// 未知 alias
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/nope/rtc/validate", nil))
	if rec.Code != 404 {
		t.Fatalf("未知 alias 应 404，实际 %d", rec.Code)
	}
	// livekit 信令反代：/providers/lk1/rtc/validate → 上游 /rtc/validate
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/providers/lk1/rtc/validate", nil))
	if rec.Code != 200 || rec.Header().Get("X-Upstream-Path") != "/rtc/validate" {
		t.Fatalf("livekit 反代路径错误: %d %q", rec.Code, rec.Header().Get("X-Upstream-Path"))
	}
}

// WHIP 按 alias：bellows-remote 实例的 POST 走 definitive 判定 + 签 grant 头
func TestWhipPerAlias(t *testing.T) {
	a := testAPI(t)
	var gotGrant string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGrant = r.Header.Get("X-Bellows-Grant")
		w.WriteHeader(201)
	}))
	defer upstream.Close()
	ctx := context.Background()
	a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "r1", Type: TypeBellowsRemote, Params: map[string]string{
		"bellows_remote_url": upstream.URL, "bellows_shared_secret": "sec"}})
	// 造一个用户+频道+ingress 记录（provider 存 alias "r1"）
	// …用 store 直接 CreateUser/CreateChannel/CreateIngress…
	a.reloadProviders(ctx)
	r := a.Router()
	a.RegisterProxies(r)

	// 假 key → 404（不到达上游）
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r1/w/badkey", strings.NewReader("sdp")))
	if rec.Code != 404 {
		t.Fatalf("假 key 应 404，实际 %d", rec.Code)
	}
	// 真 key → 反代且带 grant
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/providers/r1/w/"+key, strings.NewReader("sdp")))
	if rec.Code != 201 || gotGrant == "" {
		t.Fatalf("应 201 且带 grant: %d grant=%q", rec.Code, gotGrant)
	}
}
```

- [x] **Step 2: 跑测试确认失败**

Run: `cd server && go test ./internal/api/ -run 'TestProviderDispatch|TestWhipPerAlias' -v`
Expected: 编译失败 / 404 断言失败（旧路由还在）

- [x] **Step 3: 实现**

`RegisterProxies` 整体替换为：

```go
func (a *API) RegisterProxies(r chi.Router) {
	r.Handle("/providers/*", http.HandlerFunc(a.serveProvider))
}

// serveProvider 按 alias + 子路径分发。URL 解析用 net/url，仅反代剥前缀用字符串。
func (a *API) serveProvider(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/providers/")
	alias, sub, _ := strings.Cut(rest, "/")
	inst := a.instance(alias)
	if inst == nil {
		writeErr(w, http.StatusNotFound, "服务实例不存在")
		return
	}
	sub = "/" + sub
	switch {
	case sub == "/voice" && inst.Type == TypeEmber:
		a.voiceWS(w, r) // 原 /api/voice 处理器，路径搬家不改逻辑
	case sub == "/w" || strings.HasPrefix(sub, "/w/"):
		if inst.Ingest == nil {
			writeErr(w, http.StatusNotFound, "该实例无推流能力")
			return
		}
		r.URL.Path = sub // WHIP 逻辑与上游都按 /w 形状工作，剥到 /w 后原样复用
		a.serveWHIP(w, r, inst)
	case strings.HasPrefix(sub, "/rtc"):
		if inst.Type != TypeLivekit {
			writeErr(w, http.StatusNotFound, "该实例无信令代理")
			return
		}
		a.proxyTo(w, r, inst, "/providers/"+alias) // 剥前缀反代到 inst.Voice.SignalProxyUpstream
	default:
		writeErr(w, http.StatusNotFound, "未知路径")
	}
}
```

`serveWHIP` = 现有 whipHandler 逻辑，差异仅两点：实例来自路径 alias（不再 `a.ingestProvider(ctx)`）；PATCH/DELETE 的 HasSession 扫描遍历 `a.listInstances` 里有 WHIPServer 的实例。`proxyTo` 即现有 dynProxy/newReverseProxy 的内联版（上游用 `url.Parse(inst.Voice.SignalProxyUpstream(ctx))`，解析失败 502）。

`api.go`：
- Router 删 `r.Get("/api/voice", a.voiceWS)`。
- `signalURL(r, alias string)`：`url.URL{Scheme: ws/wss, Host: r.Host, Path: "/providers/" + alias}`（ws/wss 按 requestScheme）。
- `fillCred` 增加 alias 参数：ember → Path `/providers/ember/voice`，query channel+ticket（`url.Values`）；livekit 等 → `signalURL(r, alias)`。`joinToken` 里改用 `voiceInstance/stageInstance` 拿 alias。
- `ingressURL(r, alias, streamKey)`：`url.URL{Scheme: http/https, Host: r.Host, Path: "/providers/" + alias + "/w/" + streamKey}`；`getIngress/resetIngress` 用 `ingestInstance(ctx)` 的 alias。

- [x] **Step 4: 全量验证**

Run: `cd server && go build ./... && go vet ./... && go test -race ./...`
Expected: 全绿

---

### Task 5: 管理 API — /api/admin/providers CRUD

**Files:**
- Create: `server/internal/api/admin_providers.go`
- Modify: `server/internal/api/api.go`（Router 管理组挂 4 条路由）
- Test: `server/internal/api/admin_providers_test.go`

**Interfaces:**
- Consumes: Task 3 全部
- Produces（web Task 7 依赖的响应形状）:
```go
// GET /api/admin/providers →
// {"instances":[{"alias","type","caps":["voice"|"stage"|"ingest"...],"locked","builtin",
//               "params":{k:v},           // Secret 字段掩码为 ""
//               "params_set":{k:bool}}],  // Secret 字段是否已设置
//  "types":[{"type":"livekit","label":"LiveKit","fields":[rtc.ConfigKey...]}, ...]} // 仅 3 个可注册类型
// POST   /api/admin/providers           {type, alias, params} → 201
// PUT    /api/admin/providers/{alias}   {params} → 204（Secret 字段空串=保留旧值，其余空串=清除）
// DELETE /api/admin/providers/{alias}   → 204
```

- [x] **Step 1: 写失败测试**（handler 级，直接构造 API + httptest；管理员鉴权用 store 里造 is_admin 用户 + CreateSession 拿 token）

覆盖：注册成功 201 → 列表可见；alias 非法（大写/保留名/冲突）400/409；locked/builtin 改删 → 409；删除仍被选择器引用的实例 → 409；PUT 空 secret 保留旧值；DELETE 后实例从注册表消失（`a.instance(alias)==nil`）。

- [x] **Step 2: 跑测试确认失败** → 编译失败

- [x] **Step 3: 实现 `admin_providers.go`**

校验规则（全部在服务端）：`aliasRe` 匹配；`reservedAliases` 与现有实例（含 env 锁定）不冲突；type ∈ 3 个可注册类型；`providerTypeFields` 里非 Secret 字段必填非空（livekit 的 `livekit_url` 例外，可选）；每次增删改成功后 `a.reloadProviders(ctx)`。DELETE 引用保护：三个选择器当前生效值 == 该 alias → 409「先切换选择器」。

Router 管理组加：

```go
r.Get("/providers", a.adminListProviders)
r.Post("/providers", a.adminCreateProvider)
r.Put("/providers/{alias}", a.adminUpdateProvider)
r.Delete("/providers/{alias}", a.adminDeleteProvider)
```

- [x] **Step 4: 跑测试 + 全量验证**

Run: `cd server && go test -race ./internal/api/ && go build ./... && go vet ./...`
Expected: 全绿

---

### Task 6: dyncfg 收尾 — 选择器选项动态化、全局键收敛

**Files:**
- Modify: `server/internal/api/dyncfg.go`

- [x] **Step 1: 实现**

- `selectorKeys` 三项删掉静态 `Options`（Hint 更新为「取值 = 服务实例 alias」）。
- `adminGetConfig`：三个选择器项的 `Options` 在输出时动态填——voice：`ember` + 所有 livekit 实例 alias；stage：`none` + livekit 实例；ingest：`bellows` + livekit-ingress/bellows-remote 实例（顺序即 `listInstances` 序，内建天然在前）。
- `adminSetConfig`：选择器值用同一份动态列表校验（`none` 仅 stage 合法）。
- `kernelKeys` 收敛为 `ember.ConfigKeys()` + `bellows.ConfigKeys()`（后者 Task 3 后只剩 udp_port/public_ip）；livekitrtc 的键不再是全局表单键。

- [x] **Step 2: 全量验证**

Run: `cd server && go build ./... && go vet ./... && go test -race ./...`
Expected: 全绿

---

### Task 7: web — 管理后台「服务实例」区块 + 选择器数据源

**Files:**
- Modify: `web/src/api.ts`
- Modify: `web/src/views/admin.ts`

- [x] **Step 1: api.ts 加类型与 4 个封装**（风格对齐现有函数）

```ts
export interface ProviderInstance {
  alias: string; type: string; caps: string[]; locked: boolean; builtin: boolean;
  params: Record<string, string>; params_set: Record<string, boolean>;
}
export interface ProviderType { type: string; label: string; fields: ConfigKey[] }
export const listProviders = () => apiFetch<{ instances: ProviderInstance[]; types: ProviderType[] }>("/admin/providers");
export const createProvider = (body: { type: string; alias: string; params: Record<string, string> }) =>
  apiFetch<void>("/admin/providers", { method: "POST", body: JSON.stringify(body) });
export const updateProvider = (alias: string, params: Record<string, string>) =>
  apiFetch<void>(`/admin/providers/${alias}`, { method: "PUT", body: JSON.stringify(params) });
export const deleteProvider = (alias: string) =>
  apiFetch<void>(`/admin/providers/${alias}`, { method: "DELETE" });
```

（`ConfigKey`/`apiFetch` 用 admin.ts/api.ts 现有定义对齐，字段名以 Task 5 响应为准。）

- [x] **Step 2: admin.ts 服务实例 UI**

- 「服务参数」区上方新增「服务实例」区：实例表格（alias / 类型 / 能力 / 来源标记「内建」「环境锁定」「DB」），内建与锁定行无操作按钮，DB 行有「编辑」「删除」。
- 注册表单：类型下拉（来自 `types`）→ 按 `fields` 渲染输入框（Secret 用 password 输入），alias 输入框（ hint：小写字母数字与 -）。
- 三个选择器下拉直接用 `adminGetConfig` 已动态下发的 Options（后端 Task 6 已做，前端无需特判，确认渲染正常即可）。
- CSS 复用现有类，不加新主题。

- [x] **Step 3: 验证**

Run: `cd web && npx tsc --noEmit && npm run build`
Expected: 通过

---

### Task 8: 文档同步

**Files:**
- Modify: `README.md`、`CLAUDE.md`、`AGENTS.md`、`docs/plan-bellows-upnp.md`

- [x] **Step 1: 改写要点**

- README：内核选择表格改为「实例注册制」说明；Bellows 远端形态一节的推流地址示例改 `/providers/{alias}/w/...`；管理后台描述加「服务实例」区块。
- CLAUDE.md：rtc 内核插件模型一节——选择器值改为实例 alias、注册表语义、env 锁定/内建规则；入场判定一节 `/w POST` 改述为 `/providers/{alias}/w POST`（判定逻辑不变）；已知坑不变。
- AGENTS.md：与 CLAUDE.md 同样段落同步（顺带修正该文件遗留的 pion 旧命名措辞，本次选择器语义重写后必须一致）。
- plan-bellows-upnp.md：「hearth 的 /w 会反代」改述为新路径。

- [x] **Step 2: 检查无残留旧路径引用**

Run: `grep -rn '"/w"\|/lk/\|api/voice' server/ web/src/ README.md CLAUDE.md AGENTS.md | grep -v providers`
Expected: 只剩注释里讲历史/上游路径的部分（上游 livekit/ingress 的 /rtc、/w 是上游协议路径，保留）

---

### Task 9: 冒烟验证（两进程）

- [x] **Step 1: 构建起两进程**

```bash
cd server && go build -o ../out/smoke/hearth ./cmd/server && go build -o ../out/smoke/bellows ./cmd/bellows
# bellows：BELLOWS_SHARED_SECRET=smoke LIVEKIT_API_URL=http://127.0.0.1:7880 LIVEKIT_API_KEY=d LIVEKIT_API_SECRET=d \
#   BELLOWS_ADDR=127.0.0.1:18090 BELLOWS_UDP_PORT=47730 BELLOWS_PUBLIC_IP=127.0.0.1
# hearth：ADDR=127.0.0.1:18080 DB_PATH=./hearth.db（不配任何 PROVIDER env，验证默认 ember/none/bellows 零依赖可用）
```

- [x] **Step 2: 验证清单**

1. 零配置启动 → 语音默认 ember：注册/登录/建频道/joinToken → voice.url = `ws://127.0.0.1:18080/providers/ember/voice?...`。
2. 管理 API 注册 bellows-remote 实例（alias=r1，remote_url 指向 18090）→ 选择器 `ingest_provider=r1`。
3. `/api/ingress` 拿地址 = `http://127.0.0.1:18080/providers/r1/w/{key}`；CRLF SDP offer POST → 201；bearer 模式 → 201；假 key → 404。
4. 直打 bellows：同 grant 改一字节 offer → 401（grant 从管理端无法直接拿，用 python 以 shared secret 自签，同上次冒烟脚本）。
5. 重置密钥 → 旧 key 404 + bellows 日志「会话结束」。
6. 禁言用户 POST → 403 且 bellows 无新会话。
7. 删实例：被选择器引用 → 409；切换选择器回 bellows 后删除 → 204，再推 → `/providers/r1/w/...` 404。

- [x] **Step 3: 清理**

`out/smoke` 删除；后台进程全部停掉。

---

## Self-Review 记录（写完计划后已核对）

- spec 覆盖：实例模型(T3)/能力矩阵(T2,T3)/路径(T4)/迁移(T1,T3)/管理 API(T5)/UI(T7)/rtc 改动(T2,T3,T6)/测试(T1–T7)/冒烟(T9)/文档(T8) —— 全覆盖。
- 类型一致：`ProviderInstance`/`providerTypeFields`/`voiceInstance` 等签名在 T3 定义，T4/T5/T7 引用一致；`IngressKeys`/`RemoteKeys` 在 T2/T3 定义，T5 注册表单复用。
- 无占位符：关键新文件均含完整或近完整代码；机械性搬迁（voiceWS 等）给了精确目标位置。
