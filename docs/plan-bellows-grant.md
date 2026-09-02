# 计划：Bellows 改为 hearth 单向签发通行证（去掉回调）

状态：已实施（含单测与本地两进程冒烟，全过）。2026-09-02。基线 v0.3.3（`main` 已含 Bellows 远端形态 + 回调接口）。

## 动机

v0.3.3 的远端形态里，Bellows 每次收到 WHIP POST 都要**回调** hearth 的
`/api/internal/ingest/resolve`（共享密钥 Bearer）反查推流密钥归属并做入场判定。这带来：

- Bellows 有一条出站依赖（`HEARTH_URL`），且必须能访问 hearth 的公网入口；
- hearth 侧 `/w` 的 `canPublishByStreamKey` 是 fail-open，最终裁决落在回调上，两段判定语义不一致；
- 远端形态下「重置推流密钥」掐不断正在推的旧会话（hearth 没有通道去管远端会话）。

目标：**hearth 单向管理 Bellows**。信令本来就经 hearth 反代到 Bellows，hearth 在反代前做完
`admitUser`，把结果签成短时效通行证（grant）塞进请求头，Bellows 本地验签即可——与 LiveKit
的 join token 同一模型（hearth 签、内核用同一 secret 验），也符合铁律「凭证是短时效入场券」。
Bellows 变成无状态、无出站依赖（只连 LiveKit）的被动执行器；决策点仍只有 `admitUser` 一处。

## 设计

### 通行证（grant）

- 传递：反代请求头 `X-Bellows-Grant: <base64url(payload)>.<base64url(HMAC-SHA256)>`。
- payload（JSON）：`{"v":1,"op":"publish","key":"<推流密钥>","room":"<频道名>","user":"<用户名>",
  "offer":"<sha256(offer body) hex>","exp":<unix 秒>}`；`exp = now + 60s`。
- 签名密钥：复用 `bellows_shared_secret`（对称 HMAC）。Bellows 已持有 LiveKit API secret、
  能向任意房间发布，本就是完全受信的一端，非对称在此挡不住真实威胁；接口形状保留可换算法
  的余地（`v` 字段），需要时再上 Ed25519（hearth 私钥 / Bellows 公钥）。
- 验证（Bellows 侧）：常量时间比对签名 → `exp` 未过 → `key` 与请求里解析出的令牌一致 →
  `offer` 与请求体哈希一致（绑定 SDP，防重放挪用）。任一不过 → 401，不建会话。
- 绑定 offer 后，同一 grant 不能被拿去建第二个会话；hearth→Bellows 这一跳走私网，重放面可忽略。

### hearth 侧（接入层）

- `rtc.go` 新增可选能力接口（中性命名，不泄漏实现）：
  ```go
  // WHIPGrantIssuer 远端 WHIP 网关的通行证签发：接入层做完入场判定后签发，网关本地验签。
  type WHIPGrantIssuer interface {
      IssueWHIPGrant(ctx context.Context, streamKey, room, username string, offer []byte) (header, value string, err error)
      RevokeRemoteSessions(ctx context.Context, streamKey string) error // 通知远端掐断该密钥的会话（尽力）
  }
  ```
  `bellows.Gateway` 实现它（知道 secret 与 remote_url）。
- `proxy.go` 的 `/w` POST 分支改为：
  1. 取令牌（`rtc.WHIPToken`）；
  2. 若当前推流入口 `ProxyUpstream != ""` 且实现了 `WHIPGrantIssuer`（= 远端 Bellows）：
     读取并回填请求体（`io.ReadAll` + `MaxBytesReader` 256KB，再放回 `req.Body`），
     `ingressOwner` + `admitUser` **definitive**：密钥不存在 404、不许推 403、查询出错 503，
     不再 fail-open；通过则 `IssueWHIPGrant` 并 `req.Header.Set(header, value)`，然后反代；
  3. 其他上游（livekit ingress）保持现有 `canPublishByStreamKey` fail-open 拦截；
  4. 进程内 Bellows 不变（直接 `ServeWHIP`，resolve 闭包照旧）。
- `api.go` 的 `deleteOldEndpoint`：归属内核实现了 `WHIPGrantIssuer` 时，删除记录后再调
  `RevokeRemoteSessions(key)`（3s 超时、失败只记日志），补上「重置密钥掐断远端旧会话」。
- **删除** `/api/internal/ingest/resolve` 与 `server/internal/api/ingest_remote.go`；`Router` 里对应路由一并删。
- CLAUDE.md「入场判定」一节：执行点从四个改回三个（`/w` POST 在 hearth 侧做完判定并签发）。

### Bellows 侧（`rtc/bellows` + `cmd/bellows`）

- `Gateway` 增加远端模式的验签路径：构造时不再传 `ResolveFunc` 而是传 secret
  （建议 `bellows.NewRemote(cfg)`，`handlePost` 里 `g.resolve == nil` 即走验签）；
  `ResolveFunc` 仅保留给进程内形态。
- `handlePost`：远端模式从 `X-Bellows-Grant` 取 grant，按上文验证后得到 `room/user`，其余流程不变。
- 新增会话撤销端点：`DELETE /w/sessions/{key}`，同样要求 `X-Bellows-Grant`
  （payload `{"v":1,"op":"revoke","key":..., "exp":...}`），验签后 `closeSessionsByKey(key)` → 204。
  在 `Handler()` 与 `cmd/bellows` 的 mux 里挂上；进程内形态不需要（hearth 直接调 `DeleteEndpoint`）。
- `Gateway.DeleteEndpoint` 远端分支：hearth 侧实现 `RevokeRemoteSessions` = 签 revoke grant 后
  `DELETE {remote_url}/w/sessions/{key}`。
- `cmd/bellows`：删掉 `HEARTH_URL` 与 `resolveVia`；环境变量只剩 `BELLOWS_SHARED_SECRET`、
  `LIVEKIT_API_URL/KEY/SECRET`、`BELLOWS_ADDR`、`BELLOWS_UDP_PORT`、`BELLOWS_PUBLIC_IP`。
- **删除** `bellows_public_url`：纯通行证模型下 OBS 必须经 hearth 才有 grant，直连远端的形态不再存在
  （信令只有几百字节，TLS 也在 hearth，没有保留价值）。`Enabled`（远端）= `remote_url` 且 `secret` 非空。
- `ErrForbidden` 不再需要（判定在 hearth 侧完成），一并删掉；`ErrUnknownKey` 保留给进程内 resolve。

### 配置与部署

- hearth 侧：`INGEST_PROVIDER=bellows`、`BELLOWS_REMOTE_URL`、`BELLOWS_SHARED_SECRET` 不变。
- Bellows 侧 compose：去掉 `HEARTH_URL`，其余不变。
- **两端同一版本一起升级**：新 hearth 不再提供回调接口，旧 Bellows 会回调失败；新 Bellows 只认 grant，
  旧 hearth 不签发。不做跨版本过渡兼容（与 pion 旧值兼容只保留一个版本的取舍一致），停机窗口几秒。
- README「Bellows 远端形态」一节同步：compose 示例去掉 `HEARTH_URL`，说明改为「hearth 签发通行证、
  远端验签，远端不需要访问 hearth」。

## 阶段

1. **rtc/bellows**：grant 签发/验证（独立小函数：`signGrant(secret, payload)` / `verifyGrant(secret, header) (payload, error)`），
   `NewRemote`、`handlePost` 验签分支、`/w/sessions/{key}` 撤销端点、`DeleteEndpoint` 远端分支。
   单测：签验往返；篡改 payload / 错 secret / 过期 / offer 哈希不符 / key 不符各拒；撤销端点验签与幂等。
2. **api**：`WHIPGrantIssuer` 接口、`proxy.go` POST 分支改写（请求体读取回填）、`deleteOldEndpoint` 撤销、
   删回调接口与文件、CLAUDE.md 更新。
3. **cmd/bellows + 文档**：删 `HEARTH_URL`；README/计划文档同步；`docs/plan-bellows-upnp.md` 里
   「bellows_public_url 也可直指内网」一句删掉。
4. **验证**：`cd server && go build ./... && go vet ./... && go test -race ./...`；本地两进程冒烟
   （hearth 远端模式 + `cmd/bellows`，见 v0.3.3 的做法：创建账号/频道/推流密钥，用 CRLF 的 SDP offer
   `POST /w/{key}` 与 bearer 模式各一次 → 201；假密钥 → 404；改 offer 一个字节重发同 grant → 401；
   重置密钥后旧密钥 → 404 且远端会话被掐断）。
5. **发布**：打 tag → 两端同一版本升级 → 生产假密钥探针 404 → 真实 OBS 推一次。

## 验收

- Bellows 进程无任何对 hearth 的出站请求（抓 `cmd/bellows` 代码里再无 `http.Client` 调用 hearth）。
- `/w` POST 在 hearth 侧被封禁/禁言用户直接 403，不再到达 Bellows。
- 重置推流密钥后，远端正在推的会话数秒内结束（Bellows 日志「会话结束」）。
- 单测覆盖上述拒绝分支；`-race` 通过。

## 风险与回退

- 请求体在 hearth 侧读取后回填：注意 `MaxBytesReader` 与反代的 `ContentLength` 一致，不要让反代变成 chunked
  （ffmpeg 的 WHIP muxer 读不了 chunked 响应——这是响应侧的坑，但请求侧也保持显式长度最稳）。
- 时钟：`exp` 60s 依赖两端时钟大致同步（NTP 正常即可）；验证时允许 ±30s 偏差可减少误拒。
- 回退：切回 v0.3.3 两端镜像即恢复回调模型；配置无需改（`HEARTH_URL` 多留一段时间无害，等稳定后再删）。
