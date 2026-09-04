# 计划：分层权限与访客（系统角色阶梯 + 频道角色 + 设备绑定的临时用户）

状态：**阶段一（角色阶梯与频道角色）、阶段二（频道访客）已落地（2026-09-04）**；阶段三（注册邀请的访客模式与升级）未开工。
三个阶段各自可上线。

## 动机与边界

现状只有两档身份（`is_admin` 布尔）和一档频道身份（`channels.created_by`）：建频道、发邀请都是「管理员或所有人」二选一，
房主没法找人帮忙管房，没账号的朋友进不了房。目标是把「谁能做什么」收成一张明确的表，并允许**不注册的访客**凭链接进指定频道。

不做的：
- 细粒度 ACL / 自定义角色。系统侧是一条严格包含的阶梯，频道侧是三档，够用且能画进管理界面。
- 按设备封禁。封禁仍按 user_id；访客过期即删，再来是新 uid，靠邀请链接一次性来兜。
- 访客的 OBS 推流令牌。推流令牌不绑设备、跨会话有效，与访客「浏览器绑定、到期即删」的性质冲突。
- 多个超级管理员。只有一个，只能经 CLI 转移。

## 一、角色模型

### 系统角色（`users.role`，严格阶梯，高档包含低档能力）

| 档 | 值 | 能力增量 |
|---|---|---|
| 访客 | `guest` | 进入被授予的频道（含开放频道，见三·2）、说话/摄像头/投屏、聊天、改自己的展示名。**不能**建频道、发邀请、持有任何频道角色、拿推流令牌、改密码（没有密码）。设备绑定、有 `expires_at`、过期清理。 |
| 普通用户 | `user` | 注册账号；进开放频道与白名单频道；可被授予频道角色；可接受频道转让。 |
| 高级用户 | `power` | 创建频道；发**注册**邀请（产出 `user`）。 |
| 管理员 | `admin` | 授予/收回 `power`；管理后台全部现有能力（用户停用/删除、频道删除、服务实例、配置）；在任何频道隐含**频道主**权限。 |
| 超级管理员 | `super` | 授予/收回 `admin`；不可被停用/删除/降级。全站恰好一个。 |

规则：
- **只能操作比自己低档的人**：`admin` 不能动 `admin`/`super`，`super` 不能被任何人动。升降级都受此约束。
- **超级管理员是角色值不是 id**。首个账号（`adduser` CLI 或 open 注册的第一人）落 `super`；启动时若表里没有 `super`，把 id 最小的 `admin` 提为 `super` 并打日志（老库迁移）。转移只经 CLI：`hearth promote <用户名>`，旧 super 降为 `admin`。
- 注册产出的默认档由设置 `cfg_reg_default_role`（`user`/`power`，默认 `user`）决定；`admin` 发注册邀请时可在邀请上指定 `role`（`user`/`power`），`power` 发的固定 `user`。
- 对外 JSON：`User` 增加 `role`，**保留 `is_admin`（派生：role ≥ admin）一个版本**供前端过渡，下个版本删。

### 频道角色（`channel_members.role`，三档 + 空）

| 档 | 值 | 能力 |
|---|---|---|
| 频道主 | `owner` | 转让频道（给 `user` 及以上，旧主自动降为 `moderator`）；授予/收回 `moderator`；开关邀请制；删除频道；下面全部。 |
| 频道管理员 | `moderator` | 发**访客**邀请；踢出/封禁/解封/禁言/解禁；管理白名单成员；查看参与者。 |
| 成员 | `member` | 邀请制频道的白名单（现有语义不变）。 |
| （无） | — | 开放频道任何非访客用户可进。 |

规则：
- `owner` 从 `channels.created_by` 迁入 `channel_members`（`created_by` 列保留只作历史）。**权威在 `channel_members`**，`Channel.OwnerID` 改为从它读取。
- 系统 `admin`/`super` 在任何频道隐含 `owner`（现状 `requireModerator` 已把管理员当房主用，正式化）。
- `guest` 不能持有 `owner`/`moderator`；被授予时拒绝。`member` 行可以是访客（这就是访客的频道授予，见下）。
- 删除用户时：其 `owner` 频道转给执行删除的管理员（不再是级联删频道，避免误伤活跃频道）；其 `moderator`/`member` 行删除。

### 判定收口

新增包 `server/internal/perm`：

```go
func SysAtLeast(u *store.User, r store.Role) bool
func CanActOn(actor, target *store.User) bool         // 阶梯高低（super 对谁都 true，对 super 都 false）
func ChannelRole(ctx, st, c *store.Channel, u *store.User) store.ChannelRole // 系统 admin+ 直接返回 owner
```

`api.go` 的 `requireOwner`/`requireModerator` 改成调 `perm.ChannelRole`，`requireAdmin` 改成 `SysAtLeast(admin)`；
新增 `requireRole(r)` 与 `requireChannelRole(cr)` 两个中间件，所有 handler 不得再手写 `IsAdmin`/`OwnerID` 比较
（现状 `api.go` 的 kick 里有一处散落判断，一并收掉）。**入场判定仍只在 `admission.go`**：访客的三条约束（未过期、设备匹配、频道在授予范围内）加在 `admitUser` 里，不在别处散落。

## 二、数据模型改动

一份迁移文件 `server/internal/store/00003_roles.go`（只加列/加表，不改 baseline）：

- `users`：`role varchar(16) not null default 'user'`；`expires_at` 可空（仅访客）；`invite_id` 可空（访客来源邀请，升级/审计用）。
- `channel_members`：`role varchar(16) not null default 'member'`。
- `sessions`：`device_id varchar(32) not null default ''`（非空即绑定设备；非访客会话留空不绑定）。
- `invites`：`kind varchar(16) not null default 'register'`（`register`/`guest`）；`channel_id` 可空（`guest` 必填）；`role varchar(16) not null default 'user'`（`register` 产出的档）；`guest_ttl_sec int not null default 0`（`guest` 产出的访客寿命）；`allow_guest int not null default 0`（`register` 邀请是否允许「先以访客进入」，见阶段三）。

数据迁移（api 层 `settings.migration_version` 游标，按既有先例）：
1. `is_admin=1` → `role=admin`；其中 id 最小者 → `super`。
2. 其余用户：拥有任何频道的 → `power`（不让现有房主一夜之间失去建频道能力），否则 `user`。
3. 每个频道按 `created_by` 写一行 `channel_members(role=owner)`（若已在白名单则升级该行）。
4. `is_admin` 列保留一个版本不再写入，下个版本迁移删列。

## 三、访客

### 1. 进入流程

```
频道主/频道管理员 → 生成访客邀请（频道、寿命 1h/24h/7d、次数）→ 链接 #/join/<code>
访客打开 → 页面识别 kind=guest → 只填一个「展示名」→ POST /api/invites/{code}/guest {username, device_id}
   → 建 users(role=guest, expires_at=now+ttl, invite_id) + channel_members(role=member)
   → 建 sessions(device_id 绑定) → 返回 token 与目标频道 → 直接进房（不经大厅）
```

- 展示名就是 `username`，全表唯一，冲突即提示换一个（保持「username 只做展示」的既有铁律，不另加 display_name 列）。
- 邀请消耗与并发同现有 `ConsumeInvite`。
- 访客的 `rtc.Meta` 增加 `guest bool`，名册与聊天里显示「访客」标签。

### 2. 约束（全部落在 `admitUser` 与 `auth` 中间件）

- **设备绑定**：`auth` 对 `sessions.device_id` 非空的会话要求请求头 `X-Device-Id` 一致（聊天 WS 与进房用 query `device_id`，与现有 `joinToken` 的字段同名）；不一致按 401 处理。前端 `api.ts` 的 `req()` 一律带上 `deviceId()`，非访客会话服务端忽略。
- **过期**：`auth` 遇到 `expires_at` 已过的访客直接 401；后台每 10 分钟扫一次删除过期访客（复用 `adminDeleteUser` 的级联，含会话、设备档案、成员/封禁/禁言行、消息保留但 `username` 显示「已离开的访客」——消息表 JOIN users 取名，删用户后 JOIN 落空，改为 LEFT JOIN + 兜底文案）。在房的访客最多再留 10 分钟（凭证 TTL），到期重签失败即被弹出。
- **频道范围**：访客只能进 `channel_members` 里有自己行的频道；开放频道对访客同样不开放（否则「只可以访问指定频道」不成立）。`CanJoin` 加这一条，且顺序在封禁之后、邀请制之前。
- **能力裁剪**：建频道、发邀请、推流令牌三个接口对 `guest` 返回 403；账户接口只开放改名（无密码可改）；不能出现在「授予频道角色」的候选里。

### 3. 与注册邀请的关系（阶段三，可选）

注册邀请可勾选「允许先以访客进入」（`allow_guest=1`）。此时 `#/join/<code>` 给两个入口：直接注册，或先以访客进入（访客寿命 = 邀请剩余有效期，不绑频道，能进开放频道，即「普通用户能进的地方」——这是访客里唯一的例外，由 `invites.kind=register` 区分）。这类访客在**邀请有效期内**可以升级：`POST /api/account/upgrade {username?, password}` 把 `role` 改成邀请的 `role`、清 `expires_at`、解除设备绑定；**user_id 不变**，聊天记录与管制状态自然延续。升级不再消耗邀请名额（进入时已消耗）。频道访客（`kind=guest`）没有这条路。

## 四、接口变更

| 接口 | 变化 |
|---|---|
| `POST /api/channels` | 需 `power+` |
| `POST /api/admin/users/{id}/role` | 新增：`{role}`，受 `CanActOn` 与「只能授到自己以下」约束 |
| `POST /api/admin/invites` → `POST /api/invites` | 迁到 `power+`（`register` 类）；`admin+` 可带 `role`；`GET /api/invites` 列自己发的，`admin+` 列全部 |
| `POST /api/channels/{ch}/invites` | 新增：`moderator+` 发 `guest` 类邀请（`ttl`、`max_uses`） |
| `POST /api/invites/{code}/guest` | 新增：公开，访客入场 |
| `POST /api/channels/{ch}/transfer` | 新增：`owner`，`{user_id}`，目标须 `user+` |
| `POST/DELETE /api/channels/{ch}/moderators` | 新增：`owner`，目标须 `user+`；`GET` 列出 |
| `GET /api/channels` | 每条带 `my_role`（`owner/moderator/member/none`），前端据此显示入口；访客只返回被授予的频道 |
| `GET /api/me` | 带 `role`、访客的 `expires_at`、`upgradable` |
| `POST /api/account/upgrade` | 阶段三 |
| `GET /api/site` | 新增公开：注册策略、站点名（前端 backlog 第 1 条顺带解决） |
| CLI `hearth promote <用户名>` | 新增：转移 super |

## 五、前端

- **管理后台 · 用户**：角色列 + 下拉（候选按 `CanActOn` 由服务端 `GET /api/admin/users` 每行返回 `can_set_roles` 决定，前端不推导阶梯）；访客行带过期时间与「来源邀请」；邀请页移到「高级用户及以上」可见，管理员可选产出档。
- **频道管理**（`manage.tsx`，浮层与直达页共用）：新增「管理员」分区（授予/收回，候选从参与者与白名单里选，排除访客）、「转让频道」（确认对话框，二次输入频道名）、「访客邀请」（生成链接、列表、撤销）。分区按 `my_role` 显隐：`moderator` 看不到管理员与转让。
- **大厅**：非 `power+` 不显示创建表单；卡片角标区分「我的频道 / 我管理的」；访客不经大厅，直达房间，顶栏常驻「访客 · N 小时后过期」chip（阶段三时附「注册以保留」）。
- **加入页** `#/join/<code>`：按 `inviteInfo` 返回的 `kind` 切换表单：`guest` 只填展示名；`register` 且 `allow_guest` 时给两个按钮。
- **房间**：名册与聊天里访客带「访客」标签；右键菜单对访客不出现「授予管理员」。
- 前端不做任何权限推导：所有「能不能」以服务端返回的 `my_role`/`can_*` 字段为准，前端只做显隐。

## 六、分阶段

**阶段一：角色阶梯与频道角色**（可独立上线）
1. `store`：迁移 00003（列）+ 游标迁移（数据）+ `Role`/`ChannelRole` 类型与查询。
2. `perm` 包 + 中间件替换 + 手写判断清零（`go vet` 后 grep `IsAdmin`/`OwnerID` 只剩 perm 包）。
3. 接口：角色授予、频道转让、频道管理员、邀请下放到 `power`、`GET /api/site`。
4. 前端：管理后台角色列、频道管理两个新分区、大厅入口显隐、登录页按 `/api/site` 显示注册入口。
5. 验收：老库启动迁移后原管理员仍能进后台、原房主能建频道、频道转让后旧主变管理员并能踢人。

**阶段二：频道访客**（依赖阶段一的 `moderator`）
1. `invites` 扩列、访客邀请接口、访客入场接口、`sessions.device_id` 与 `auth` 校验、`admitUser` 三条约束、过期清理协程、消息 LEFT JOIN。
2. 前端：`req()` 带设备头、加入页访客表单、访客 chip、名册标签、频道管理「访客邀请」分区。
3. 验收：同一链接在第二个浏览器打开被拒；换设备用同一 token 被 401；过期后自动消失且房间里的人收到离开提示；访客打不开非授予频道。

**阶段三：注册邀请的访客模式与升级**（可选）
1. `allow_guest`、升级接口、加入页双入口、「注册以保留」入口。
2. 验收：升级后 user_id 不变、聊天记录仍归属、设备绑定解除、原邀请不再被二次消耗。

## 七、风险与取舍

- **现有部署的行为变化**：普通用户默认不能再建频道。迁移把「已拥有频道的人」提成 `power` 缓解，其余人由管理员按需提升；`cfg_reg_default_role=power` 可恢复旧行为。
- **`is_admin` 双写期**：一个版本内 `role` 是权威、`is_admin` 只读派生，前端切到 `role` 后删列。
- **访客名字占用**：访客过期删除后名字释放；活跃期内占用 `username` 唯一性，可能挡住同名注册，接受。
- **设备绑定的边界**：绑定的是前端 `localStorage` 里的 `device_id`，清站点数据即失效、需重新拿链接；这正是「跟浏览器走」的预期语义，不做恢复。
- **管理员隐含房主**：与现状一致，但「转让给管理员」会让该频道的 owner 行落在管理员名下，降级该管理员时频道不跟着变，需要在降级时提示「其名下仍有 N 个频道」。
