# 计划：2026-09 产品功能批次（四条 worktree 并行，最后合并）

状态：**设计定稿（2026-09-06，用户拍板），四批并行实施。** 本文自包含：每个实施 agent 只读「共享约定」+ 自己那一批；合并 agent 读「合并顺序」。

## 共享约定（所有批次必须遵守，这是防冲突的全部依据）

### 代码落点：新功能进新文件，热点文件只加接线
- `web/src/views/room.tsx` 是所有前端功能的交汇点。**禁止**在里面写新功能逻辑：新组件放 `web/src/views/room/<name>.tsx`（目录不存在就建），新工具放 `web/src/chat/<name>.ts`、`web/src/notify.ts` 等；`room.tsx` 里只允许 `import` + 一两行接线（挂组件、传信号、调用函数）。接线尽量贴着已有的相关代码放，不要重排已有代码、不要整段格式化。
- 服务端同理：新端点新文件（`server/internal/api/<feature>.go`），`api.go` 只加路由行；store 新逻辑新文件（`server/internal/store/<feature>.go`），`models.go` 只加字段/结构。
- 样式加在 `web/src/style.css` **末尾**，以 `/* ---- <批次名> ---- */` 分段，避免与他人改同一区域。
- 设置面板（`web/src/views/settings-panes.ts`）新开关加在各自 pane 的**末尾**。

### 提前分配的编号与名字（不得改、不得抢）
| 项 | 归属 | 值 |
| --- | --- | --- |
| store 迁移 | 第 2 批 | `00005_chat_v2.go`（`messages` 加 `reply_to INTEGER NULL`、`deleted_at TEXT NULL`；新表 `message_reactions(message_id, user_id, emoji, created_at, PRIMARY KEY(message_id,user_id,emoji))`） |
| store 迁移 | 第 4 批 | `00006_account_audit.go`（新表 `audit_log(id, at, actor_uid, action, target_uid NULL, channel_id NULL, detail TEXT)`；`sessions` 表若缺 `user_agent`/`created_at`/`last_seen` 则补列） |
| dyncfg 键 | 第 2 批 | `chat_retention_days`（Group `chat`，默认 `30`，`0` = 永久） |
| dyncfg 键 | 第 4 批 | `audit_retention_days`（Group `admin`，默认 `180`）；`notify_*` 不做服务端键（纯前端偏好） |
| 偏好键（prefs.ts） | 第 1 批 | `afkMinutes`（默认 10，0=关） |
| 偏好键 | 第 2 批 | `mentionCue`（默认 true） |
| 偏好键 | 第 3 批 | `theaterAutoHide`（默认 true） |
| 偏好键 | 第 4 批 | `notifyMessages`/`notifyMentions`/`notifyJoins`（默认 true/true/false） |
| 数据通道 topic | 第 2 批 | 仍用 `chat`，但载荷改为**信封** `{ "t": "message"|"delete"|"reaction", ... }`（见第 2 批）；`chat-file` 不变 |

### 消息 JSON（第 2 批扩展，其它批只读不改）
```json
{"id":1,"channel_id":1,"uid":7,"username":"a","kind":"text","content":"hi","created_at":"...",
 "reply_to":null,"deleted":false,"reactions":[{"emoji":"👍","uids":[1,2]}]}
```

### 每批的产出形式
- 在自己的 worktree/分支上提交，**不 push、不打 tag、不部署**；分支名报告给协调者。
- 每批完成时 `cd server && go build ./... && go vet ./... && go test ./...`（仅当改了服务端）与 `cd web && npx tsc --noEmit && npm run build` 必须通过。
- 提交信息中文、末尾 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。
- 仓库公开：注释/提交信息/测试数据不得出现任何个人部署信息（域名、IP、机器代号、路由器/分流软件）。
- 前端铁律（CLAUDE.md）：Solid 状态走信号/memo，不引第二真相源；引擎产的媒体元素用 ref 挂载不重建。

---

## 第 1 批：小而高频（纯前端）→ v0.9.11

**A1 手机一键翻转摄像头**：房间控制栏摄像头按钮旁加"翻转"（仅触屏/窄屏显示，`matchMedia('(pointer: coarse)')`）；实现为 `AVEngine.flipCamera()`：用 `facingMode: { exact: next }` 重新采集并替换发布轨（livekit-client 的 `LocalVideoTrack.restartTrack({ facingMode })` 优先），失败回退到设备列表切换。新文件 `web/src/views/room/camera-flip.tsx`。

**A2 投屏声音来源与回音**：`web/src/engine/livekit.ts` 的 `screenOptions` 里 `audio` 对象加 `restrictOwnAudio: true`（剔除本页面自己播放的声音——别人的语音——避免回音；不支持的浏览器忽略）；`ScreenShareCaptureOptions` 加 `systemAudio: 'include'`、`selfBrowserSurface: 'exclude'`、`preferCurrentTab: false`（只在类型允许时；用 `getSupportedConstraints()` 判定后再传，避免 TypeError）。设置面板「共享系统声音」的 Hint 补两条引导：只想带某个标签页声音时在浏览器对话框选"标签页"；若某平台仍有回音，把「扬声器设备」切到与系统默认不同的输出设备即可让采集不含语音。

**B5 图片发送前压缩**：新文件 `web/src/chat/compress.ts`：`compressImage(file): Promise<File>`——`image/jpeg|png|webp` 且（>1.5MB 或长边 >1920）时用 canvas 缩到长边 1920、导出 `image/webp` q=0.85（不支持 webp 则 jpeg 0.85）；GIF 与其它类型原样；保留原名改后缀。在 `room.tsx` 的 `sendFiles` 入口调用（一行接线）。

**B6 未读定位与分割线**：打开聊天抽屉时若 `unread>0`，滚到第一条未读并在其上方渲染「以下为新消息」分割线；分割线在用户滚动到底或 30s 后消失。新文件 `web/src/views/room/unread-divider.tsx`，`appendMessage` 处只记录"第一条未读 id"信号。

**C3 AFK 状态**：新文件 `web/src/afk.ts`：页面隐藏或无输入（pointer/key）超过 `afkMinutes` 分钟 → 通过 LiveKit 参与者属性（`localParticipant.setAttributes({afk:'1'})`）广播；有操作即清。名册项显示"离开"灰标；`AVEngine` 加 `setAttribute(key, value)` 与 `EPart.afk` 字段（从 `participant.attributes` 读）。偏好 `afkMinutes`，设置面板个人 pane 末尾加输入。

**E1 OBS 推流状态可见**：名册里 `ingest` 参与者已可识别；补：徽标「推流中」、悬停/点击显示码率、丢包、分辨率（复用 `remoteVideoStats(identity,'screen')`，每 2s 刷新，只在悬停时轮询）；推流参与者离开时房间事件流一条「推流已停止」。新文件 `web/src/views/room/ingest-badge.tsx`。

验收：tsc/build；手机 Chrome/Safari 翻转前后摄像头；桌面无翻转按钮；投屏时 `getDisplayMedia` 约束含 `restrictOwnAudio`（DevTools 里看）；粘 4MB 截图发出 <1.5MB；打开抽屉定位到未读；10 分钟不动名册出现"离开"；OBS 推流时名册出徽标与码率。

---

## 第 2 批：聊天数据模型（先服务端后前端）→ v0.9.12

**服务端**（`server/internal/api/chat_messages.go` 扩展 + 新文件 `chat_reactions.go`、`store/chat.go`、迁移 `00005_chat_v2.go`）：
- `POST /api/channels/{channel}/messages` body 加可选 `reply_to`（必须是同频道存在且未删的消息 id，否则 400）。
- `DELETE /api/channels/{channel}/messages/{id}`：作者本人，或频道 `moderator/owner`（`perm.ChannelRole`）可删；软删（`deleted_at`），`content` 与 `file` 在响应/历史中清空、`deleted:true`。
- `PUT/DELETE /api/channels/{channel}/messages/{id}/reactions/{emoji}`：emoji 只允许固定集合 `👍 ❤️ 😂 😮 😢 🔥 👀 🎉`；每人每条每种一次；响应返回该消息最新 `reactions`。
- `GET .../messages` 返回值带 `reply_to`、`deleted`、`reactions`（一次查询聚合，避免 N+1）。
- 保留策略：`chat_retention_days`（默认 30）；启动时与每小时一次删除超期消息（含其 reactions）；`DELETE /api/channels/{channel}/messages`（owner/admin+）清空频道并写审计（若第 4 批的 `audit_log` 尚不存在则先记 `log.Printf`，合并后由合并 agent 接到审计表——**合并顺序保证第 4 批在后，因此这里只写日志**）。
- 测试覆盖：删除权限矩阵、reply_to 校验、reactions 去重、保留策略删除、历史聚合形状。

**数据通道信封**（前端 `web/src/chat/protocol.ts` 新文件，发送与解析都在这里）：
```json
{"t":"message","m":{...Message}}
{"t":"delete","id":123,"by":7}
{"t":"reaction","id":123,"emoji":"👍","uid":7,"on":true}
```
接收方对 `delete`/`reaction` 直接改本地消息状态；历史与 `after=` 补齐仍以服务端为准。**兼容**：解析到不带 `t` 的旧载荷按 `message` 处理（升级过渡期旧页面）。

**前端**：
- **B1** 消息右键/长按菜单「撤回」（自己）/「删除」（版主）→ `DELETE` → 广播 `delete` → 卡片显示「消息已撤回」。新文件 `web/src/views/room/msg-menu.tsx`。
- **B2** 输入框 `@` 触发名册补全（新文件 `web/src/chat/mentions.ts`：解析 `@用户名` → 渲染高亮；被@判定按 `uid`，用名册把用户名映射到 uid）；被@时提示音用 `playCue('mention')`（`audio.ts` 加一档，两声上行）+ 偏好 `mentionCue`；`C1` 到位后被@优先系统通知（本批只留一个 `onMention` 回调钩子）。
- **B3** 卡片下方反应条：点击加/取消；显示 emoji+计数，自己点过的高亮。新文件 `web/src/views/room/reactions.tsx`。
- **B4** 「回复」进入引用态，发送时带 `reply_to`；卡片顶部显示被引用者与摘要（≤60 字），点击滚到原消息。新文件 `web/src/views/room/reply.tsx`。
- **D3** 管理后台「聊天」分组加 `chat_retention_days`；频道管理页加「清空聊天记录」（二次确认）。

验收：Go 全套 + tsc/build；两端互发：撤回/删除对方即时消失并留占位；反应实时同步；引用跳转；@ 高亮与提示音；保留策略测试；旧载荷兼容。

---

## 第 3 批：剧场模式与布局 → v0.9.13

**A3 剧场模式 + 全屏**：投屏画面占满内容区（侧栏、名册卡片、聊天抽屉全部让位），控制栏在无操作 3s 后隐藏、鼠标移动/触摸出现（偏好 `theaterAutoHide`）；「全屏」用 `document.documentElement.requestFullscreen()`，退出全屏自动退出剧场；快捷键 `T` 切剧场、`F` 切全屏（沿用 `onHotkeyDown`，注意 `inTypingTarget`）。新文件 `web/src/views/room/theater.tsx`；`room.tsx` 只加信号 `theater()` 与挂载。

**A4 剧场浮动名册**：剧场模式下右上角半透明浮窗（`backdrop-filter`、透明度 0.75），列出参与者头像/名字，说话者高亮（复用 `speakers()`），可拖到四角（拖拽位置存 prefs），可折叠成一个小按钮。新文件 `web/src/views/room/floating-roster.tsx`。

**A5 画中画**：优先 **Document Picture-in-Picture**（`documentPictureInPicture.requestWindow`，Chrome 116+）：把投屏 `<video>` 与浮动名册一起搬进 PiP 窗口（媒体元素用 ref 搬运，不重建）；不支持时退到 `video.requestPictureInPicture()`（只画面）；两者都不支持则不显示按钮。关闭 PiP 把元素搬回。新文件 `web/src/views/room/pip.ts`。

**B7 无投屏时聊天为主**：没有任何投屏/推流轨时，布局切换为「聊天区占主体 + 顶部一行频道用户列表（头像+名字+说话高亮）」；一旦有人开始投屏自动切回原布局（剧场/非剧场按用户上次选择）。实现为 `room.tsx` 顶层的一个 `layoutMode()` memo（`'chat' | 'stage' | 'theater'`）与三套容器类；新文件 `web/src/views/room/chat-first.tsx`（顶部用户行）。**本批是布局大改，务必保证所有既有控件在三种布局下都可达。**

验收：tsc/build；剧场进出、全屏进出、控制栏自动隐藏；浮动名册拖拽/折叠/说话高亮；Chrome 下 Document PiP 能把画面+名册弹出、关闭后恢复；无投屏时聊天为主，投屏开始自动切回；手机竖屏三种布局不溢出。

---

## 第 4 批：账号、管理、通知 → v0.9.14

**服务端**（新文件 `api/account.go`、`api/audit.go`、`store/sessions.go`、`store/audit.go`、迁移 `00006_account_audit.go`）：
- **D1** `POST /api/me/password` `{old,new}`：校验旧密码，新密码 ≥8；成功后作废该用户**其它**会话。
- **D2** `GET /api/me/sessions`（当前/其它，含 UA、创建与最近活跃时间）、`DELETE /api/me/sessions/{id}`；登录时记录 UA，请求经鉴权中间件时每 5 分钟更新一次 `last_seen`（节流，别每次都写库）。
- **D4** `audit_log`：在禁言/解禁/踢出/封禁/解封/改频道角色/删他人消息/清空频道 这些路径落一条（`actor_uid, action, target_uid, channel_id, detail`）；`GET /api/admin/audit?channel=&actor=&action=&after=&limit=`（admin+）；`audit_retention_days` 定时清理。**注意第 2 批的删消息/清空频道处只写了 `log.Printf`，合并后由合并 agent 改成调用本批的 `store.Audit(...)`。**
- 测试：改密后其它会话 401、会话列表/踢会话、审计写入与筛选、保留清理。

**前端**：
- **D1/D2** 设置浮层「个人」加「账号」pane（新文件 `web/src/views/account-pane.ts`）：改密表单、会话列表与「下线」按钮。
- **D4** 管理后台加「审计」tab（新文件 `web/src/views/admin-audit.tsx`）：表格 + 筛选 + 分页（`after=` 游标）。
- **C1** 系统通知：新文件 `web/src/notify.ts`：`Notification.permission` 管理（首次收消息或首次开麦后请求，不在进页面时弹）；页面 `hidden` 时对「新消息 / 被@ / 有人进房」各按偏好发通知（点击通知聚焦窗口并打开聊天抽屉）；`room.tsx` 接线：`appendMessage(live)` 与名册 join 差分处各一行。设置个人 pane 末尾加三个开关。
- **C2** PWA：`web/public/manifest.webmanifest`（名字、图标用现有 `docs/icon.svg` 转 192/512 PNG 放 `web/public/icons/`）、`index.html` 加 `<link rel=manifest>` 与 `theme-color`；`web/src/sw.ts` 最小 service worker：只处理 `install/activate`，**不缓存任何资源**（避免旧前端顶包），vite 里用 `?url` 注册。iOS 后台保活不在本批。

验收：Go 全套 + tsc/build；改密后旧标签页 401 跳登录；会话踢下线；审计表能查到禁言/踢人记录；页面在后台时收到系统通知、点击回到页面；Chrome/Android 出现"安装"入口且安装后可用。

---

## 合并顺序与合并 agent 的职责

1. 顺序固定 **第 1 批 → 第 2 批 → 第 3 批 → 第 4 批**，逐个 `git merge --no-ff` 进 `main`（或先合到一条集成分支再快进）。
2. 每合一批：跑 Go 全套 + tsc/build；冲突只按「共享约定」解——新文件不会冲突，`room.tsx`/`api.go`/`dyncfg.go`/`style.css`/`settings-panes.ts` 的冲突按"两边都保留、接线行各自归位"处理，**不改任何一方的设计**。
3. 第 4 批合入后，把第 2 批里删消息/清空频道处的 `log.Printf` 审计占位改成调用第 4 批的 `store.Audit(...)`，并补一条测试。
4. 全部合完再跑一次全套；报告每批的分支名、合并提交、冲突文件与解法。**不 push、不打 tag、不部署**——由协调者验收后处理。
