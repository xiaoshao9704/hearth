# 计划：入口整理（频道菜单、设置去混入、账户入口、触屏长按）

状态：**设计定稿（2026-09-08），待实施。** 本文自包含，纯前端。

## 问题（已核实）

1. **个人设置与"我在本频道"的东西混在一起**：静音本频道的开关放在设置「语音与视频」页、只在从房间打开时出现；推流页显示"当前频道地址"也靠打开时的频道上下文。设置浮层的内容取决于"从哪里点进来"，用户分不清哪些是账号级、哪些是频道级。
2. **控制栏两个"齿轮"**：「投屏画质」（`room.tsx` 约 2551 行，`openSettings('screen')`）与「设置」（约 2587 行，`openSettings('av')`）开的是同一个浮层，只差默认页。
3. **登出与账户入口藏着**：左侧栏底部的头像/名字是账户菜单触发器（`shell.ts` `#acct-trigger`：账户设置 / 外观 / 退出登录），但没有视觉提示；手机上侧栏是抽屉、房间内看不到，等于没有入口。
4. **右键在触屏无效**：全站 5 处 `onContextMenu`——聊天消息（`room.tsx` 约 2133）、名册成员行（约 2617）、视频卡片（约 2466，**已有**长按）、聊天优先模式头像条（`room/chat-first.tsx` 42）、悬浮名册（`room/floating-roster.tsx` 123）。除视频卡片外都没有长按，也没有触屏可见的替代按钮。

## 目标：四层入口，边界清楚

| 层 | 内容 | 入口 |
|---|---|---|
| 个人（账号/设备） | 语音视频设备、投屏画质、外观、通知总开关与离线推送、账号与通行密钥、推流令牌与设备标签、我的设备、邀请 | 设置浮层（控制栏「设置」、账户菜单「账户设置」）——**内容不再随打开位置变化** |
| 我在本频道 | 静音/取消静音、OBS 推流地址、复制邀请链接、离开频道 | **频道菜单**（房间顶栏频道名旁；大厅卡片菜单同一份） |
| 频道管理 | 成员/黑白名单/管理员/转让/邀请制 | 频道菜单里的「频道管理…」（owner/moderator 可见）→ 现有 `manage.tsx` / 设置浮层的「频道」分区 |
| 服务器 | 管理后台 | 账户菜单里的「管理后台」（admin+ 可见）与既有跳转 |

## 设计

### A. 频道菜单
- 新文件 `web/src/views/room/channel-menu.tsx`（Solid）：菜单项按顺序
  1. 「静音本频道 / 取消静音」（调现有 `PUT/DELETE /api/channels/{channel}/mute`，成功后派 `hearth:channels` 事件，与现有静音实现同一条路）
  2. 「OBS 推流地址…」（打开现有 `room/ingest-panel.tsx`；访客不显示）
  3. 「复制邀请链接」（`canInvite` 为真时；调现有邀请创建/复制逻辑，`lobby.ts`/`manage.tsx` 里已有）
  4. 分隔线；「频道管理…」（`canModerate()` 为真时，`openSettings('channel', settingsCtx)`）
  5. 分隔线；「离开频道」（现有离开逻辑，走 `nav.ts` 的离开确认）
- 房间顶栏：频道名 `<h1>` 变成带下拉箭头的按钮（`#channel-menu-trigger`，`aria-haspopup="menu"`），点开菜单；**删除**顶栏独立的「频道管理」（约 2390）与「OBS 推流」（约 2400）两个图标按钮；「连接质量读数」芯片保留。
- 大厅频道卡片：现有 `card-bell` / `card-gear` 两个按钮合并成一个「…」（`data-menu`），点开同一份菜单（离开项在大厅不出现；未加入的频道只出静音与邀请）。`lobby.ts` 是 vanilla，菜单组件要能从 vanilla 调：导出一个 `openChannelMenu(anchor: HTMLElement, channel: Channel, opts)` 的命令式入口（内部 `render` 到自建宿主 div，关闭时 dispose），Solid 房间页也用它。

### B. 设置浮层去混入
- `settings-panes.ts`「语音与视频」：删 `#notify-mute-row` 整块。
- `settings-panes.ts`「推流」：删"当前频道地址"块与 `renderStream` 的 `channel` 参数链路，改为一行说明「完整地址在房间顶栏频道名的菜单里」；令牌/标签/重置不动。
- `settings.tsx`：`PersonalHost` 不再向个人 pane 透传 `channel`（「频道」分区仍按 `ctx.channel` 出，那是频道管理层，不动）。
- 控制栏：删「投屏画质」按钮（约 2551），只留「设置」。手机 560px 断点的四列网格随之少一格，确认布局不空洞。

### C. 账户入口
- `shell.ts` 用户栏：触发器右侧加一个 `chevron`（`ui.ts` 若无则加 icon），hover/focus 有背景高亮，让它看起来可点。
- 房间顶栏与大厅顶栏右侧加账户入口 `#acct-entry`（小头像圆点），点开**同一个**账户菜单（账户设置 / 外观 / 管理后台（admin+）/ 退出登录）。把 `shell.ts` 里的菜单构建抽成可复用函数 `openAccountMenu(anchor)`（放 `web/src/account-menu.ts`），侧栏与顶栏都调它，不复制菜单 HTML。
- 桌面宽度下顶栏入口可以隐藏（侧栏已可见）；≤860px 必须显示。

### D. 触屏长按
- 新文件 `web/src/longpress.ts`：`wireLongPress(el, handler, {ms = 500})`，`touchstart` 起计时、`touchmove` 超过 10px 或 `touchend` 取消，触发后标记一次 `longPressFired` 让紧随的 `click` 忽略；返回解绑函数。把视频卡片现有的长按逻辑（`room.tsx` 约 2475）换成它。
- 五处统一：聊天消息、名册成员行、视频卡片、聊天优先头像条、悬浮名册——每处 `onContextMenu` 旁加长按，弹**同一个**菜单。Solid 组件里用 `ref` + `onCleanup` 绑定；列表项用事件委托（在容器上一次绑定，按 `closest` 找行），避免每行一个定时器。
- 触屏可见按钮：聊天消息行的「…」（`msg-menu` 的触发按钮现在 hover 才显示）在 `(hover: none)` 媒体查询下常显；名册成员行已有「更多操作」按钮（约 2632/2662）确认触屏可见；聊天优先头像条与悬浮名册体积小，靠长按即可。

### E. 文档
- `CLAUDE.md` 前端一节：「设置的三个维度」改成上表的四层；加一句「右键菜单必配长按（`longpress.ts`）与触屏可见入口」。
- `README.md` 若有描述"顶栏 OBS 推流按钮"的句子改成"频道菜单"。

## 改动清单
| 位置 | 改动 |
|---|---|
| `web/src/views/room/channel-menu.tsx`（新）、`web/src/account-menu.ts`（新）、`web/src/longpress.ts`（新） | 三个共用件 |
| `web/src/views/room.tsx` | 顶栏频道名按钮与菜单接线、删两个图标按钮、删投屏画质、账户入口、五处长按接线 |
| `web/src/views/room/chat-first.tsx`、`floating-roster.tsx` | 长按 |
| `web/src/shell.ts`、`web/src/views/lobby.ts` | 用户栏 chevron、菜单抽取、卡片「…」 |
| `web/src/views/settings-panes.ts`、`settings.tsx` | 去混入 |
| `web/src/ui.ts`、`style.css` | icon、`(hover: none)` 规则、菜单样式复用 `.user-menu` |
| `CLAUDE.md`、`README.md` | 文案 |

## 验收
1. `cd web && npx tsc --noEmit && npm run build && npm test`；服务端零改动，但仍跑 `cd server && go build ./...` 确认嵌入无误。
2. 本地烟测（scratchpad 构建、`ADDR=:8094`、干净 DB、`reg_policy=open`）：桌面与 375px 手机视口各走一遍——
   - 房间顶栏：频道名可点开菜单，项按角色显隐；静音切换后 `GET /api/channels` 的 `muted` 翻转；「OBS 推流地址…」打开面板且地址以 `/w/<id>` 结尾；顶栏不再有独立的频道管理/OBS 图标。
   - 控制栏无「投屏画质」；「设置」打开的浮层不含静音行，推流页不含频道地址块，从大厅与从房间打开内容一致。
   - 账户：侧栏用户栏有箭头；手机视口顶栏有账户入口，两步内退出登录（confirm 桩后回到登录页）。
   - 长按：手机视口用 `touchstart/touchend` 模拟 550ms 长按，五处各弹出菜单；`touchmove` 超 10px 不弹；长按后不触发 click。
3. 报告里写清每个菜单/入口的 DOM id 与 class。

## 不做
- 不改任何服务端接口；不动通知/推送逻辑本身；不改管理后台。
