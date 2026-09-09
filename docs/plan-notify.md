# 计划：通知补全（回复视同 @、Service Worker 通知、Web Push、图标角标、频道静音）

状态：**已实施（原定稿 2026-09-08，2026-09-09 核对）。** 证据：`server/internal/api/push.go`、`web/src/notify.ts`、`web/src/push.ts`。本文自包含。只做浏览器层级，不做任何厂商推送通道或第三方 webhook。

## 现状（已核实）

- 通知走页面内 `Notification` API（`web/src/notify.ts`）：页面在后台时发新消息 / 被 @ / 有人进房，页面可见时只响提示音；三个全局开关 `notifyMessages`/`notifyMentions`/`notifyJoins`（`prefs.ts`）。Android Chrome 不允许页面直接 `new Notification`，代码捕获后静默放弃——**Android 现在没有任何通知**。
- Service Worker（`web/public/service-worker.js`，由 `web/src/sw.ts` 注册）刻意最小：不缓存、不监听 fetch；没有 `push`/`notificationclick`。
- 房间页已有标签页标题未读数（`room.tsx` 约 2160 行 `(n) 频道 · Hearth`）。
- @ 的判定在客户端：`web/src/chat/mentions.ts` 的 `mentionsUser`/`mentionedUids`，服务端不解析 @。`reply_to` 只用于显示引用（`room.tsx` 约 1437/2034 行）。
- 消息落库 `POST /api/channels/{channel}/messages`（`api/chat_messages.go`），实时扇出走 LiveKit 数据通道。
- 会话表 `sessions`（`store/sessions.go`，`DeleteSessionByID`）；退出 `api.go` `logout`。密钥"留空首启生成落库"的先例：`lkembed_api_secret`（`api/lkembed.go` 约 39 行）。下一个迁移号 **00008**。

## 目标与判据

1. **回复视同 @**：别人回复我的消息 → 提示音、桌面通知、离线推送都按 @ 一级处理。
2. **通知经 Service Worker**：Android Chrome 能弹；点通知直达该频道（打开/聚焦窗口并切到 `#/room/<频道名>`）。
3. **Web Push（离线）**：页面关着也能收到 **被 @ / 被回复** 两种事件；普通消息不推。通道由浏览器自己决定（Safari/iOS PWA 走 Apple、Windows Edge 走微软、Firefox 走 Mozilla、Chrome 系走 Google），代码不做任何特判，送不到就静默。
4. **图标角标**：装成 PWA 后，图标上显示未读数（`navigator.setAppBadge`），推送到达时也更新，回到页面清零。
5. **频道静音**：每用户每频道一个开关；静音 = 该频道**不发任何**提示音/通知/推送（含 @ 与回复）。

## 设计

### 服务端
**迁移 `00008_notify.go`**（照 `00007_passkeys.go` 写法）：
- `push_subscriptions`：`id`、`user_id`、`session_id`（关联 `sessions`，会话没了订阅一起删）、`endpoint varchar(1024) unique`、`p256dh`、`auth`、`user_agent`、`created_at`、`last_ok_at`、`fail_count`。
- `channel_mutes`：`user_id`、`channel_id` 复合唯一、`created_at`。

**VAPID 密钥**：dyncfg 键 `webpush_vapid_public`/`webpush_vapid_private`（Group `admin`，留空首启用 `webpush.GenerateVAPIDKeys()` 生成落库，后台只读显示公钥；换密钥会让所有订阅失效，Hint 写明）。依赖 `github.com/SherClockHolmes/webpush-go`。VAPID subject 用 `https://<请求 Host>`（发送时从订阅创建时记录的 origin 取，存进 `push_subscriptions.origin`，或直接用配置键 `webpush_subject` 留空取 `mailto:admin@<host>`——二选一，实现者定，报告写明）。

**端点**（都需登录）：
| 方法/路径 | 说明 |
| --- | --- |
| `GET /api/push/vapid` | `{public_key}` |
| `POST /api/push/subscribe` | body `{endpoint, keys:{p256dh,auth}}`；`INSERT OR REPLACE` by endpoint，绑当前 session |
| `DELETE /api/push/subscribe` | body `{endpoint}` |
| `PUT /api/channels/{channel}/mute` / `DELETE …/mute` | 静音/取消（`requireChannel`） |
| `GET /api/channels` | 每项加 `muted: bool` |

**消息落库时的推送判定**（`chat_messages.go` 发消息 handler 末尾，另起 goroutine，10 秒超时，不阻塞响应）：
- 请求体新增 `mentions: []int64`（客户端用 `mentionedUids` 算好传来，上限 20）。服务端**校验**每个 uid：用户存在且 `@<其用户名>` 确实出现在 `content` 里，不匹配的丢弃（防止拿 uid 列表骚扰任何人）。校验通过的 uid 存进消息 `meta`（已有 `meta` 列，JSON 里加 `mentions`）。
- 被回复者：`reply_to` 指向的消息作者。
- 目标集合 = 校验后的 mentions ∪ 被回复者，去掉发送者自己，去掉对该频道静音的人。
- 每个目标的所有订阅各发一条；payload JSON `{kind:"mention"|"reply", channel, channel_id, message_id, from, preview}`（preview 取 content 前 120 字符，文件消息用文件名）。响应 404/410 → 删该订阅；其他失败 `fail_count++`，≥10 次删除。
- 不做合并/节流：@ 与回复本身低频。

**会话联动**：`logout`、`DeleteSessionByID`（账号页踢会话）、会话过期清理处，删对应 `push_subscriptions`。`DeleteUser` 级联清两张表。

### 前端
**`web/public/service-worker.js`**（仍不缓存、不监听 fetch）加三个事件：
- `push`：解析 payload；`clients.matchAll({type:'window'})` 里若有 **可见** 客户端就不弹（页面在前台，靠页内提示）；否则 `showNotification(标题, {body, tag:'hearth-chat', icon, data:{channel}})`，并 `navigator.setAppBadge` 若可用（计数：简单地 +1，用 `registration` 上的内存变量，回到页面由页面清零）。
- `notificationclick`：`event.notification.close()`；找已有窗口客户端 → `focus()` 并 `postMessage({type:'open-channel', channel})`；没有则 `clients.openWindow('/#/room/<channel>')`。
- `notificationclose`：无操作。

**`web/src/notify.ts`**：`show()` 改为优先 `navigator.serviceWorker.ready.then(reg => reg.showNotification(...))`，没有 SW 再回落 `new Notification`；通知的 `data.channel` 带上；页面监听 SW 的 `open-channel` 消息 → `location.hash = '#/room/<channel>'`（`main.ts` 或 `notify.ts` 里挂一次）。新增 `mentioned` 的判定入口不变。

**回复视同 @**（`room.tsx` 约 1437 行）：`mentioned = live && kind==='text' && (mentionsUser(...) || replyToMe(m))`，`replyToMe` = `m.reply_to` 指向的消息 `user_id === myUid`（消息列表里找；找不到按不是）。发消息时（约 1642 行）body 带 `mentions: mentionedUids(content, mentionUsers())`。

**静音**：`Channel.muted`；`room.tsx` 的 cue/notify 与 `appendMessage` 判定处：`muted` 则全部跳过（含 @/回复）。入口两处：大厅频道卡片菜单（`lobby.ts` 已有 `menuButtonHtml`）加「静音 / 取消静音」；房间内设置的通知区块加同一开关（设置浮层按 `ctx.channel` 出）。静音的频道在侧栏与大厅卡片上带一个小图标（`bellOff`，`ui.ts` 若没有就加一个）。

**Web Push 订阅**（`web/src/push.ts` 新）：`isSupported()`（`'PushManager' in window && serviceWorker`）、`subscribe()`（权限 → `reg.pushManager.subscribe({userVisibleOnly:true, applicationServerKey: urlBase64ToUint8Array(公钥)})` → `POST /api/push/subscribe`）、`unsubscribe()`、`state()`。设置里通知区块加「离线推送（被 @ 和回复）」开关：不支持时说明原因（iPhone 提示"先添加到主屏幕再开"）；开关状态以 `reg.pushManager.getSubscription()` 为准，不存本地副本。页面每次启动若已有订阅且已登录，静默 `POST /api/push/subscribe` 一次（endpoint 可能被浏览器轮换）。

**图标角标**：房间页更新标题未读数的同一处调用 `navigator.setAppBadge?.(n)` / `clearAppBadge?.()`（`n === 0`）；页面变可见时清零。

### 权限申请
沿用现有原则：不进页面就要；SW 通知路径不改申请时机；离线推送开关是用户手势，直接申请。

## 改动清单
| 位置 | 改动 |
|---|---|
| `server/go.mod` | `github.com/SherClockHolmes/webpush-go` |
| `server/internal/store/00008_notify.go`、`store/push.go`、`store/mutes.go`、`models.go` | 两张表与 CRUD（`MutedChannelIDs(userID)`、`IsMuted`、`SubscriptionsOf(uids)`、`DeleteSubscription*`） |
| `server/internal/api/push.go`（新）+ `_test.go` | 端点、VAPID 生成、`notifyMessageTargets` 判定与发送（HTTP client 可注入，测试用 httptest） |
| `server/internal/api/chat_messages.go`、`api.go`、`dyncfg.go`、`account.go`/`store/sessions.go` | `mentions` 校验落 meta、路由、密钥键、`muted` 字段、会话删除联动 |
| `web/public/service-worker.js`、`web/src/sw.ts` | push / notificationclick、消息通道 |
| `web/src/notify.ts`、`web/src/push.ts`（新）、`web/src/views/room.tsx`、`lobby.ts`、`shell.ts`、`settings-panes.ts`、`api.ts`、`chat.ts`、`style.css` | 上述前端项 |
| `README.md` | 通知一节：能力边界（各浏览器通道、iPhone 须加到主屏幕）、`webpush_vapid_*` 键 |

## 验收
1. Go 全套 + tsc/build。服务端测试：mentions 校验（uid 不在正文里被丢弃）、目标集合（发送者排除、静音排除、reply 作者）、404/410 删订阅、`fail_count` 累计删除、退出登录删订阅、`muted` 字段、静音端点权限。
2. 本地烟测：SW 注册成功；`GET /api/push/vapid` 返回公钥；订阅/退订；发一条带 @ 的消息后服务端日志（或注入的假端点）收到一次推送请求，payload 形状正确。实际浏览器推送投递依赖厂商通道，本地 Chrome 能收到就贴截图，收不到说明原因，不算失败。
3. 浏览器：回复我的消息触发 @ 一级提示；静音频道后 @ 也不响；标题未读数与角标同步（角标只在 PWA 或支持的桌面浏览器可见）。

## 不做
- 不做普通消息的离线推送；不做推送合并/免打扰时段；不做任何非浏览器通道。
