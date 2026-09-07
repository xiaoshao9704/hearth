# 计划：推流入口重做 + 频道寻址渐进切 id（第一批）

状态：**设计定稿（2026-09-07），待实施。** 本文自包含。

## 背景与动机

推流令牌是**每用户一把、不分频道**，频道只出现在 WHIP 地址最后一段。现在设置「推流」页把「令牌管理」（账号级）和「选频道拼地址」（频道级）揉在一页：频道用下拉、默认第一个、从房间打开设置也不预选当前频道；房间内没有任何推流入口。频道一多下拉不可用，用户在房间里想推流要跑去设置翻列表，路径反了。

同时确立一条设计规范：**频道寻址逢改必往 id 靠**（不立项一次性大改；数据层/store/perm 已全是 `channel_id`，名字只活在 HTTP 路径段、LiveKit 房间名、前端路由三层薄皮）。这次是第一个机会，做前三步；LiveKit 房间名与 hash 路由不动。

## 目标与判据

1. 房间内一键拿到**这个频道**的完整 WHIP 地址与令牌（复制即用），附 OBS 填法。
2. 设置「推流」只留账号级内容：令牌查看/复制/重置、设备标签、Bearer 说明；不再有频道下拉。从房间打开设置时仍顺手显示当前频道地址（兜底）。
3. 新生成的 WHIP 地址用**频道 id**；OBS 里已存的名字地址**永久有效**（服务端双接受）。
4. `POST /api/token` 接受 `channel_id`，前端进房改传 id；`channel`（名字）继续接受。
5. 新建频道不允许纯数字名（避免与 id 段歧义）；既有纯数字名的频道靠 id 优先、名字兜底继续工作。

## 已核实的事实

- 频道解析三处 `ChannelByName`：`server/internal/api/api.go` `channelOf`（约 302 行，24 条 `/api/channels/{channel}/*` 路由的中间件都经它）、`joinToken`（约 561 行，请求体 `channel`）、`server/internal/api/admission.go` `admitIngest`（约 125 行，WHIP 路径段）。
- WHIP 路径解析 `server/internal/rtc/rtc.go` `WHIPToken`（`/w/{channel}[/{token}]`），保留字 `sessions`/`revoke`（`api.go` 约 532 行）；频道名规则 `channelNameRe = ^[a-zA-Z0-9_-]{1,64}$`（约 465 行）——**允许纯数字**。
- 令牌接口 `server/internal/api/ingest.go`：`writeIngestToken` 回 `{token, tag, base, enabled}`，`base` = `/providers/{alias}/w/` 绝对地址；`ingestTokenGet/Reset/Tag`。
- 前端推流页 `web/src/views/settings-panes.ts` `renderStream`（约 975–1150 行）：`channels = chs.map(c => c.name)`、`current = channels[0]`、`serverAddr = base + current`；pane id `stream`，访客不显示（`settings.tsx:56`）。
- 房间页 `web/src/views/room.tsx` 顶栏（约 2255–2300 行）：频道管理按钮 `openSettings('channel', settingsCtx)` 旁可加入口；`settingsCtx` 带 `channel`（名字）；角色探测处有 `chs.find(c => c.name === channel)`，即房间页能拿到 `Channel.id`。名册推流角标 `room/ingest-badge.tsx` 只显示实测数据。
- `web/src/api.ts` `Channel` 已有 `id: number`；`fetchJoinCredentials(channel: string)` body `{channel, device_id}`。

## 设计

### 服务端：路径段/请求体双接受（一处 helper）
`server/internal/api/channel_ref.go`（新）：
```go
// channelByRef 频道引用：纯数字先按 id，找不到再按名字；其余按名字。
// 既有纯数字名的频道靠名字兜底继续工作；新建频道已禁止纯数字名。
func (a *API) channelByRef(ctx context.Context, ref string) (*store.Channel, error)
```
- `channelOf`、`admitIngest` 改调它；`joinToken` 请求体加 `channel_id int64`（>0 优先），否则 `channel` 走 `channelByRef`。
- `createChannel`：`channelNameRe` 不变，另加「纯数字名不允许」校验，错误文案「频道名不能是纯数字」。
- `WHIPToken` 不动（它只切路径段，不解析）。
- 测试：id/名字/纯数字名兜底/不存在 → 404、`channel_id` 优先于 `channel`、纯数字名创建被拒。

### 前端：房间内入口
- `web/src/views/room/ingest-panel.tsx`（新）：Solid 小面板（用现有弹层/卡片样式，非全屏），内容：
  - 服务器地址 `${base}${channel.id}` + 复制按钮；
  - 令牌（默认打码，眼睛切换，复制按钮）+ Bearer 说明；两条各一个「复制」；
  - 三行 OBS 填法（服务：WHIP；服务器：上面地址；Bearer 令牌：上面令牌）；
  - 「令牌重置与设备标签在设置里」链接 → `openSettings('stream', settingsCtx)`；
  - `enabled=false`（推流入口关闭）时显示原因，地址照给。
  - 数据来自 `getIngestToken()`，打开时拉一次；访客不渲染入口。
- `room.tsx` 顶栏频道管理按钮旁加一个 `btn-icon`（icon `stream`，title「OBS 推流」），点开上面面板；只在 `!isGuest` 时显示。频道 id 从已有的 `chs.find(...)` 结果取；拿不到（列表未加载）时面板显示「加载中」。
- `renderStream`（设置推流页）瘦身：删频道下拉与 `channels`；地址区改为：有 `ctx.channel` 时显示该频道地址（需要 id：`listChannels()` 找名字 → id），否则一句「完整地址在房间顶栏的『OBS 推流』里复制」。令牌/标签/重置/说明照旧。
- `api.ts`：`fetchJoinCredentials(channel: string, channelID?: number)` 传 `channel_id`；房间页进房处传 id（拿不到 id 时仍传名字，服务端兜底）。
- 文案里不出现任何个人部署信息。

### 文档
`README.md` 推流段：地址形态改为 `…/w/<频道 id>`，注明旧的 `…/w/<频道名>` 继续有效；`CLAUDE.md` 架构铁律「推流入口」一段补一句「路径段 id 优先、名字兜底」。

## 改动清单
| 位置 | 改动 |
|---|---|
| `server/internal/api/channel_ref.go`（新）+ `_test.go` | `channelByRef` |
| `server/internal/api/api.go` | `channelOf`/`joinToken`/`createChannel` 接线 |
| `server/internal/api/admission.go` | `admitIngest` 用 `channelByRef` |
| `web/src/views/room/ingest-panel.tsx`（新） | 房间内面板 |
| `web/src/views/room.tsx` | 顶栏入口 + 进房传 `channel_id` |
| `web/src/views/settings-panes.ts` | `renderStream` 瘦身 |
| `web/src/api.ts` | `fetchJoinCredentials` 签名 |
| `web/src/style.css` 末尾 | 面板样式（复用 card/copy-line） |
| `README.md`、`CLAUDE.md` | 文案 |

## 验收
1. `cd server && go build ./... && go vet ./... && go test ./internal/api/`；`cd web && npx tsc --noEmit && npm run build`。
2. 本地烟测（scratchpad 构建、`ADDR=:8090`、干净 DB、`reg_policy=open`）：curl `GET /api/channels/{id}/messages` 与 `/api/channels/{name}/messages` 都 200；`POST /api/token {channel_id}` 200；`POST /api/channels {name:"123"}` 400；WHIP `POST /providers/lkembed/w/<id>` 无令牌 → 401/403（非 404），`/w/不存在` → 404。
3. 浏览器：房间顶栏出「OBS 推流」，面板地址以 `/w/<数字>` 结尾，复制按钮可用；设置推流页无下拉；从房间打开设置推流页显示当前频道地址。

## 不做
- 前端其余 22 处 REST 调用仍传名字（下一批）；`#/room/` 路由与 LiveKit 房间名不动；不做改名。
