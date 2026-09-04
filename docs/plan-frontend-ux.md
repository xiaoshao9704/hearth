# 计划：前端体验补全（第一轮）

状态：**第一轮四个工作包已落地（2026-09-04）**，审阅无阻塞项。本文档是这一轮的设计与分工依据，后续轮次沿用第四节的 backlog 往下推。

## 动机与边界

功能层面语音、投屏、推流、管理已经齐了，但「用起来顺不顺」这一层一直没系统补过。这次先做一次全量审计
（房间页 / 轻页面与公共层 / 管理后台与样式系统三条线），再按「一轮能落地、互不打架」的原则切工作包。

首要项来自使用反馈：**有人进出房间没有任何提示**——名册刷新是无声的，不看成员面板根本不知道谁来了谁走了。

本轮**不做**：
- 后端改动（登录页按 `reg_policy` 显示注册入口需要一个公开的站点元信息端点，排到下一轮）；
- 结构性重构：CSS token 化（圆角/字号/间距/z-index）、近似容器类归并、暗色调板去重、settings-panes 六个 pane 的 Solid 迁移；
- PWA manifest / 图标集、字体自托管（改为非阻塞加载先止血）；
- 聊天的分页历史、@提及、消息合并；聚焦布局的节点搬家问题（需要把两个 `<For>` 合一，单独一轮）。

## 一、审计摘要

三份审计的原文很长，这里只留结论。编号沿用审计里的，方便对照。

### 房间页（`views/room.tsx` + 引擎）

- **状态反馈**：移动端看不到连接状态（A1）；重连文案没有「立即重试」入口（A2/C13）；被禁言只弹一次 toast、麦克风按钮无禁言态（A5）；被踢/频道删除只有 1.5s toast（A6）；静音全部是隐形状态（A7）；聊天断线只写在 placeholder（A8）。
- **错误处理**：聊天在 ws 未连上时静默丢消息、无 maxlength（B1，服务端上限 2000）；重连路径采集失败无提示（B5）。
- **交互**：零快捷键（C1）、零 ARIA（C2）、踢出/禁言/离房零确认零加载态（C3）、媒体开关无 inflight 保护（C4）、触屏够不到 hover 控件（C5）、长按后仍触发点击（C6）。
- **生命周期**：连接期间离开房间，清理监听尚未注册 → 引擎/麦克风/定时器全部残留（H1，高）。
- **引擎**：livekit connect 无超时（G1）；自动播放被拦截时全场无声零提示（G2）；ember 重连会给同一 identity 累积死 `<audio>`（G4）。

### 轻页面与公共层（`main.ts` / `shell.ts` / `ui.ts` / `api.ts` / `lobby` / `login` / `join`）

- **路由**：异步渲染无代际令牌，快速切页会擦掉新视图或抛 TypeError（A1，高）；守卫跳转用 push，返回键死循环（A2，高）；没有登录后回跳（A3）；未知 hash 静默当大厅并弄坏大厅轮询（A4）；`document.title` 全站不变（A5）。
- **登录/加入**：网络失败把英文 `Failed to fetch` 直显（B2，高）；请求无超时（B3）；不记住用户名（B4）；已登录点邀请链接会静默顶掉会话（B8）；网络错误被误报成「邀请不存在」（B9）；过期态没有出口（B10）。
- **大厅**：创建频道无校验无防重（C1）；卡片无房主管理入口（C2）；卡片是 div+click 键盘不可达（C3）；「服务器在线」写死（C4）；列表失败无重试（C5）；轮询不管后台标签页（C6）。
- **api.ts**：Error 丢状态码，房间页靠中文子串匹配分支（D1，高）；401 静默跳转（D3）；`getUser()` 的 JSON.parse 未防护能白屏（D5）。
- **ui.ts**：无 confirm 原语，管理后台用原生 `confirm()`（E1，高）；toast 无上限/去重/手动关闭/aria-live（E2/E3）；无焦点管理（E4）。
- **外壳**：退出登录藏在设置浮层最底部（F1）；主题三态无文字反馈（F2）；侧栏拉取失败永久「加载中…」（F3）；房内侧栏在线数冻结（F4）。
- **index.html**：Google Fonts 阻塞式外链，内网部署首屏白屏（G1，高）。

### 管理后台与样式系统（`admin.tsx` / `manage.tsx` / `style.css`）

- **管理后台**：实例表单不计入脏态、后退/刷新绕过未保存拦截（A1）；无 in-flight 可重复提交（A2）；五处破坏性操作零确认（A3）；alias 与必填项零前端校验、params 键名不显示（A7）；资源监控一次性加载永不刷新、温度当百分比画条（A8）；**内联样式压死移动断点**（A9，根因统一：topbar/body/sidebar 的内联 padding/width 让媒体查询够不着，表格列宽 20+ 处内联）。
- **样式系统**：`a:hover` 特异性压过 `.btn-primary`；无 `color-scheme`（深色下原生控件/滚动条是浅色）；零 `:focus-visible`；零 `prefers-reduced-motion`；`--text-2` 对 `--bg-1` 约 3.1:1、`--text-3` 约 2.0:1 不达 AA；`.disabled` 无 `pointer-events:none`、无加载态原语；无对话框原语；`.chip` 无默认色。

## 二、本轮工作包

分工原则：**按文件划界，互不重叠**；公共层先行，三个业务包并行；每个包只做审计里「高」与「中/小」的项，
「大」规模项一律进 backlog。所有包共享的验收线：`cd web && npx tsc --noEmit && npm run build` 通过。

### WP-0 公共层（先行，其余包依赖它的签名）

文件：`ui.ts`、`api.ts`、`style.css`。

- `confirmDialog(opts): Promise<boolean>`：原生 `<dialog>`，Esc/遮罩取消，默认焦点在取消，关闭还焦点。
- `toast()` 升级：`role=status`、上限 3 条、同文案去重、点击关闭、淡出。
- `ApiError { status }`：HTTP 错误带状态码；网络失败/超时（15s）统一中文文案，`status=0`；401 → 记 `sessionStorage.hearth_next` + toast + `location.replace('#/login')`；`getUser()` 防护；GET 不发 Content-Type。
- CSS：`:where(a)`、`color-scheme`、`:focus-visible`、`prefers-reduced-motion`、`hover:none`、text-2/text-3 提到 AA、`.btn:disabled`/`.btn.loading`、`.dialog*`、`.topbar-lg`/`.page-body`/`.sidebar-admin`/`.table-actions`/`.cell-ellipsis`/表格 `--col-N` 列宽变量、`.chip` 默认色、`.state-block`、`.conn-chip`。

### WP-A 房间页（依赖 WP-0）

文件：`views/room.tsx`、`audio.ts`、`prefs.ts`、`engine/*.ts`、`views/settings-panes.ts`（仅加一个开关）。

1. **进出房间提示**（首要项）。在房间层做名册差分，引擎中立：
   - 以 uid 为单位（多设备同账号只在第一台进、最后一台走时提示）；排除自己；推流参与者（`kind=ingest`）单独文案「X 的 OBS 开始/停止推流」。
   - 提示形式：toast（`'X 进入了房间'` / `'X 离开了房间'`）+ 短提示音（WebAudio 合成两音，无资源文件）+ 聊天面板里一条系统行（非服务端消息，客户端本地事件流，按时间与聊天消息合并渲染）。
   - 首次进房与每次重连后的第一份名册只重置基线不提示；连接断开期间不比较。
   - 提示音受偏好 `joinCue` 控制（默认开，设置「语音与视频」pane 里一个开关）；页面不在前台时也响。
   - 基线快照是派生缓存不是真相源：用 Solid `createEffect(on(roster, (cur, prev) => …))` 之类拿上一次值，不要另起一个手工同步的名册副本。
2. **H1** 生命周期：`myHash` 与清理注册提到 `await connectLines` 之前，`connectLines/connectLine` 在 `leaving` 置位后提前返回并释放半成品引擎。
3. **A5/A7**：麦克风按钮从 `roster()` 派生禁言态（红色斜杠 + title「已被禁言」）；静音全部按钮有明确的开启态与一次 toast。
4. **B1/C9**：聊天输入 `maxlength=2000`；ws 未连上时不清空输入框并 toast「聊天未连接，稍后再发」；打开聊天面板时聚焦输入框。
5. **C3**：踢出/禁言/踢出全部走 `confirmDialog`；投屏中点离开先确认；操作 `await` 期间按钮加 `.loading`。
6. **A1/A2/C13**：顶栏加 `.conn-chip`（连接中 / 已连接 / 重连中·第 N 次）；`stage-status` 文案旁给「立即重试」按钮（调用已有的 `retryNow`）。
7. **D1**：`handleCredsError` 改按 `ApiError.status` 分支（404/403 → bounce，0 → 重试，401 已由 api.ts 处理），去掉中文子串匹配。
8. **G1**：livekit `connect` 加 15s 超时（`Promise.race`），超时后 `disconnect()` 收干净。
9. **G2**：自动播放被拦截 → 舞台区出一条可点击横幅「点击以开启声音」。引擎接口加可选回调 `onAudioBlocked?()` 与方法 `resumeAudio(): Promise<void>`：livekit 接 `RoomEvent.AudioPlaybackStatusChanged` + `room.startAudio()`；ember 在 `el.play()` reject 时回调，`resumeAudio` 对所有已挂音频元素重放。

### WP-B 轻页面与外壳（依赖 WP-0）

文件：`main.ts`、`shell.ts`、`theme.ts`、`views/lobby.ts`、`views/login.ts`、`views/join.ts`、`index.html`。

1. **A1**：`route()` 维护代际号，`renderLobby/renderJoin` 每个 `await` 之后校验，过期即 return。
2. **A2/A3/A4/A5**：守卫跳转改 `location.replace`；未登录时记 `sessionStorage.hearth_next`，登录成功后读取并清除；未知 hash → `replace('#/lobby')`；按路由设置 `document.title`。
3. **A6**：先 `route()` 再后台 `fetchMe()`，成功后广播 `hearth:user`。
4. **B4/B5/B7**：记住上次用户名（`localStorage hearth_last_user`，回填后焦点落密码框）；失败时 `.field.bad` + 清空密码框；`<label for>`、`autocapitalize=off`、`enterkeyhint`。
5. **B8/B9/B10/B12**：已登录打开邀请链接先问「用这条邀请另建账号，还是直接进大厅」；`ApiError.status===404` 才显示「不存在」，网络错误显示「暂时连不上」+ 重试；过期态与表单态补「已有账号？去登录」；成功后给「立即进入」。
6. **C1–C6**：创建频道前端正则 + busy + 成功 toast；`is_owner` 卡片加齿轮（`openSettings('channel', { channel })`）；卡片改 `<a href>`；「服务器在线」由请求成败驱动；错误态用 `.state-block` + 重试；轮询只在 `visibilityState==='visible'` 时跑。
7. **F1–F5**：用户栏点头像/名字弹小菜单（账户设置 / 外观 / 退出登录，退出走 `confirmDialog`）；主题切换后 toast 当前模式，三处切换逻辑收成 `theme.ts` 一个 helper；侧栏首次拉取失败画「加载失败，点击重试」；侧栏频道轮询搬进 `shell.ts` 自管（进房也刷新在线数），lobby 只保留卡片刷新；抽屉 Esc 关闭 + `aria-expanded`。
8. **index.html**：字体 `<link>` 改非阻塞（`media="print" onload`）；`#app` 放静态「正在启动…」骨架；`<noscript>`；`theme.ts` 切换时同步 `<meta name="theme-color">`。

### WP-C 管理后台与频道管理（依赖 WP-0）

文件：`views/admin.tsx`、`views/manage.tsx`。

1. **A3**：三处原生 `confirm()` 换 `confirmDialog`；停用用户、撤销邀请、拉黑、移出白名单、关闭邀请制补确认。
2. **A2**：`saveGroup/submitProvider/make/act` 加 busy 信号，按钮 `.loading` + `disabled`。
3. **A1**：实例表单纳入脏态；脏时 `beforeunload` 提醒。
4. **A7**：alias 前端正则 `^[a-z0-9][a-z0-9-]{0,31}$`、必填项校验（Secret 字段创建时必填、编辑时留空保持）、`fieldInput` 印 `f.name` 键名；三个输入区改成 `<form>` 让 Enter 生效。
5. **A8**：状态页 10s 轮询 + 「更新于 N 秒前」+ `onCleanup`；温度不再当百分比画条。
6. **A9**：内联 `topbar/body/sidebar` 样式换成 `.topbar-lg/.page-body/.sidebar-admin`；表格列宽改 `--col-N` 变量 + 单元格 `data-label`；manage 直达页加菜单按钮；`guardNav` 拦截时关抽屉。
7. **A4**：manage 三个列表初值 `undefined` 区分加载中与空；participants 拉取失败显示出来。
8. **A10/A11**：`.switch` 加 `role=switch aria-checked`、tab 加 `aria-selected`、策略单选 `role=radiogroup`；删掉从未写入的 `data-badge` 徽章。

### 收尾

- 三个业务包完成后统一跑 tsc/build，再由一个审阅 Agent 对照 CLAUDE.md 铁律（第二真相源、媒体元素 ref 挂载、宿主 div、teardown 顺序、隐私）过一遍 diff。
- 提交按工作包分开，提交信息中文。

## 三、实施约束（给每个实施者）

- 只改自己工作包名下的文件；共享的 `style.css` 由 WP-0 一次写好，业务包若确需追加样式，只能在文件里新开一段带本包名的注释区，不改别人的规则。
- 房间页信号纪律：任何新状态先问「能不能从 `roster()`/`videoEntries()` 派生」，能派生就不加信号；引擎产的 `<video>/<audio>` 只用 ref 挂载，遮罩/横幅做成兄弟节点叠加。
- 新增的任何 `render()` 一律挂自建宿主 div，dispose 时 `host.remove()`。
- 房间清理块的两条不变量不得打破：`leaving = true` 先于断连接；Solid `dispose()` 后于引擎 `dispose()`。
- 错误分支按 `ApiError.status` 判，不再匹配服务端中文文案。
- 面向用户的文案中文；不引入新依赖；不加 `any`。

## 四、Backlog（后续轮次）

按收益排序，每条标注来源编号。

1. 登录页按注册策略显示入口：需要后端公开 `/api/site`（策略 + 站点名），前端按 `open/invite/closed` 三态换文案（轻页面 B1）。
2. 聚焦布局节点搬家导致黑帧：`gridTiles/railTiles` 两个 `<For>` 合一，用 CSS `order`/定位切换（房间 D1）。
3. ember 重连音频元素泄漏：`teardown` 经 `onAudioTrackRemoved` 回收 `trackEls`（房间 G4，注意 teardown 顺序坑）。
4. 键盘快捷键：按住说话、M 切麦、D 静音全部、Ctrl+Enter 发送（房间 C1）。
5. 触屏可达性：`.tact`/`.volpop` 在 `hover:none` 下常显；设备子行加「更多」按钮；长按 `preventDefault`（房间 C5/C6）。
6. 聊天：翻历史不被拽回底部 + 「有新消息 ↓」；未读进 `document.title`（房间 E2/C12）。
7. settings-panes：打开「语音与视频」不抢占摄像头（改为按需预览）、`devicechange` 监听、扬声器试听、推流 pane 错误态（F1–F4）。
8. 管理后台：实例「测试连接」（需后端探活端点）、列表分页与搜索覆盖（A7.8/A4.3）。
9. 样式系统结构性收敛：token 化、`.surface/.row-base` 归并、暗色调板去重（`light-dark()` 需确认浏览器基线）。
10. PWA：manifest + 图标集 + `apple-touch-icon`；字体自托管。
11. 多标签页会话同步（`storage` 事件）。
12. 房主标签按 uid 而非用户名比较（频道接口返回 owner uid）。
13. 拆分形态（ember 语音 + livekit 舞台）下舞台线单独重连会先弹「X 的 OBS 停止推流」再弹「开始推流」：名册差分只看语音线是否连着，舞台线的抖动应一并静默（房间层给舞台线也加基线重置）。
14. 右键菜单里的操作按钮在确认对话框弹出时菜单已被外部点击关闭，`.loading` 落在已卸载的按钮上：菜单在 `confirmDialog` 期间应暂停「点外部关闭」。
