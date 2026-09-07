# 计划：通行密钥（Passkey / WebAuthn）快速登录与登录后推荐

状态：**设计定稿（2026-09-07），待实施。** 本文自包含。

## 目标与判据

- 登录页多一个「使用通行密钥登录」：一次触摸/面容/指纹即登录，**不用输用户名**（可发现凭证，resident key）。
- 已登录用户可在设置「账号」里添加/重命名/删除通行密钥；密码登录仍可用，是兜底。
- **密码登录成功后**，若该账号没有任何通行密钥，且本设备未跳过，弹一次推荐卡片：「用通行密钥下次一键登录」→ 立即添加 / 稍后（7 天内不再提）/ 不再提示（本设备）。
- 访客（`role=guest`）不参与：不能添加、不弹推荐（转正后再来）。
- 反代（nginx 终止 TLS）与本地开发（`http://localhost:*`）都能用；RP ID/origin 可配。

## 已核实的事实

- Go 1.27，`server/go.mod` 尚无 WebAuthn 依赖 → 用 `github.com/go-webauthn/webauthn`（标准实现，含 `webauthn.User` 接口、`SessionData`）。
- 会话签发入口 `server/internal/api/api.go` `issueSession(w, r, u)`（登录 `login` 在 405 行附近调用它）；`requestScheme(r)` 已尊重 `X-Forwarded-Proto`。
- 登录页 `web/src/views/login.ts`（vanilla TS，`mode: 'login' | 'register'`，表单 `#login-form`）。
- 设置「账号」pane 由 `web/src/views/account-pane.ts` 渲染（改密卡片 + `renderSessions(host)` 会话卡片）。
- 审计动作常量在 `store.AuditActions`（v0.9.11），`a.audit(...)` 写入。
- `sessions`/`devices` 表与 `X-Device-Id` 校验（v0.9.13）与本功能正交；通行密钥登录签出的会话不绑设备。

## 设计

### RP ID 与 origin
- dyncfg 键（Group `admin`）：
  - `passkey_rp_id`：留空 = 取请求 `Host` 去端口（反代下即公开域名；本地即 `localhost`）。
  - `passkey_origins`：逗号分隔的允许 origin 列表；留空 = 只允许 `requestScheme(r) + "://" + Host`。部署者若前端与 API 不同源或有多域名，在这里列全。
- WebAuthn 配置按请求构造（`webauthn.New(&webauthn.Config{RPID, RPDisplayName: 站点名, RPOrigins})`），因为 RP ID 在留空时依赖 Host；实现上做一个按 `(rpID, origins)` 键的小缓存避免每次 New。
- **注意**：RP ID 一旦变了，旧凭证全部失效（浏览器按 RP ID 绑定）。Hint 里写清；`hearth.example.com` 与 `example.com` 是不同 RP ID。

### 存储（迁移 `00007_passkeys.go`，照 `00006` 写法，不改 baseline）
表 `passkeys`：`id`、`user_id`、`credential_id BLOB/VARBINARY 唯一`、`public_key BLOB`、`sign_count`、`aaguid`、`transports`（逗号分隔）、`backup_eligible`、`backup_state`、`name`（用户可改，默认按创建时的 UA 生成如「Chrome · macOS」）、`created_at`、`last_used_at`。

### 挑战态
注册与登录的 `SessionData` 存在**服务端内存**的短时表（TTL 2 分钟，随机 `ceremony_id` 作键，用后即删；进程重启即失效，可接受——用户重来一次）。登录 begin 是未鉴权公开接口，要**限流**（复用 `clientLog` 的按 IP/账号限频思路，如每 IP 每分钟 20 次）。

### 端点
| 方法/路径 | 鉴权 | 说明 |
| --- | --- | --- |
| `POST /api/auth/passkey/login/begin` | 无 | 返回 `{ceremony_id, options}`（`allowCredentials` 为空 → 可发现凭证；`userVerification: preferred`） |
| `POST /api/auth/passkey/login/finish` | 无 | body `{ceremony_id, credential}`；校验 → 找到凭证与用户 → 用户被停用/已过期访客 → 拒；更新 `sign_count`/`last_used_at`；走 `issueSession` 返回 `{token,user}` |
| `GET /api/account/passkeys` | 登录，非 guest | 列表（id、name、created_at、last_used_at、backup_state） |
| `POST /api/account/passkeys/begin` | 登录，非 guest | `excludeCredentials` 列出已有；`residentKey: required`、`userVerification: preferred`、`attestation: none` |
| `POST /api/account/passkeys/finish` | 登录，非 guest | body `{ceremony_id, credential, name?}`；写入；审计 `passkey_add` |
| `PATCH /api/account/passkeys/{id}` | 登录 | 改名 |
| `DELETE /api/account/passkeys/{id}` | 登录 | 删除；审计 `passkey_remove`。密码始终存在，所以不需要"最后一个不能删"的约束 |
| `GET /api/me` | 登录 | 响应加 `passkey_count`（推荐卡片依赖它，不必再打一次列表） |

`webauthn.User` 的 `WebAuthnID` 用 `user_id` 的 8 字节大端（稳定、不含用户名）；`WebAuthnName`/`DisplayName` 用用户名（改名后新注册的凭证跟新名，旧凭证在认证器里显示旧名——可接受，Hint 提一句）。

### 前端
- `web/src/passkey.ts`：`isSupported()`（`window.PublicKeyCredential` 且 `isUserVerifyingPlatformAuthenticatorAvailable()`）、`loginWithPasskey()`（begin → `navigator.credentials.get` → finish → `saveSession`）、`registerPasskey(name?)`（begin → `create` → finish）；用 `PublicKeyCredential.parseCreationOptionsFromJSON/parseRequestOptionsFromJSON` 与 `credential.toJSON()`（Chrome 120+/Safari 17+；不支持的浏览器手工 base64url 转换，写一个小 helper 兜底）。
- 登录页：表单下方加「使用通行密钥登录」按钮（`isSupported()` 才显示）；可选：`mediation: 'conditional'` 让密码框 autofill 里出现通行密钥（Chrome/Safari 支持时才启用，失败静默）。
- 登录后推荐：密码登录成功且 `me.passkey_count === 0` 且 `role !== 'guest'` 且 `localStorage.hearth_passkey_nudge` 不是 `never` 且距上次 `later` ≥ 7 天 → 大厅顶部弹一张卡片（不是模态）：三个按钮「立即添加」（走 `registerPasskey`，成功后 toast + 卡片消失）/「稍后」（记时间戳）/「不再提示」（记 `never`）。通行密钥登录本身不弹。
- 设置「账号」pane：在会话卡片旁加「通行密钥」卡片：列表（名字、创建、最近使用、是否已同步备份）、重命名、删除（二次确认）、「添加通行密钥」。访客看不到这张卡片。

### 安全要点
- `finish` 时校验 origin 在允许列表、RP ID 一致、challenge 与 ceremony 匹配、`sign_count` 单调（回退则拒并审计 `passkey_replay`）。
- 登录 begin/finish 限流；ceremony 一次性。
- 凭证公钥/ID 不进日志；诊断上报（`clientlog`）脱敏规则已覆盖 token，凭证 JSON 不要发到 `client-log`。

## 改动清单
| 位置 | 改动 |
| --- | --- |
| `server/go.mod` | `github.com/go-webauthn/webauthn` |
| `server/internal/store/00007_passkeys.go`、`store/passkeys.go`、`models.go` | 表、CRUD、`CountPasskeys` |
| `server/internal/api/passkey.go`（新） | 端点、ceremony 内存表、限流、RP 配置缓存 |
| `server/internal/api/api.go`、`dyncfg.go` | 路由行；`passkey_rp_id`/`passkey_origins`；`/api/me` 加 `passkey_count` |
| `web/src/passkey.ts`（新）、`web/src/api.ts` | 客户端流程与类型 |
| `web/src/views/login.ts`、`web/src/views/lobby.ts`（推荐卡片）、`web/src/views/account-pane.ts`（管理卡片）、`style.css` 末尾 | UI |
| `README.md` | 部署段：RP ID/origin 两个键与"换域名旧凭证失效"的提醒 |

## 验收
1. Go 全套 + tsc/build。服务端测试：begin/finish 用 `go-webauthn` 的测试工具或构造好的固定向量走通注册与登录；origin 不在列表 → 拒；ceremony 重放 → 拒；sign_count 回退 → 拒；guest 调注册端点 → 403；停用用户凭证登录 → 拒；限流生效。
2. 真机：Mac Safari/Chrome 与手机各注册一枚，退出后一键登录；删除后无法登录；改名生效；密码登录后无凭证弹推荐、点「稍后」7 天内不再弹、「不再提示」永久不弹。
3. 反代部署（域名）与本地 `localhost` 各走通一次（RP ID 留空的默认推导正确）。

## 不做
- 不做 attestation 校验与认证器白名单；不做无密码账号（密码始终保留）；不做跨设备"用手机扫码登电脑"的额外 UI（浏览器自带的 hybrid 流程已经覆盖）。
