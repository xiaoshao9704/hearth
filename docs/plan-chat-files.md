# 计划：聊天发送图片/文件（只传输，不落服务器硬盘）

状态：**设计草案（2026-09-05），待选定方案后实施。** 需要用户在「方案对比」里拍板走哪条路。
本计划自包含。

## 目标与判据

给频道聊天加"发图片/文件"，但**不让 hearth 把文件字节持久化到硬盘**（`/data` 只放数据库，上行有限的小服务器硬盘吃不消）。判据：

- 发一张截图/一个文件，接收方能看到（图片内联预览、文件给下载）。
- 文件字节**不写 hearth 磁盘**、**不进聊天数据库**（DB 也在磁盘上，且要随库备份，塞不下）。
- 不引入必须常驻的重依赖（redis/对象存储）作为默认形态——保持"单文件开箱即用"。
- 传输失败不影响文本聊天与语音/舞台。

## 现状（已核实）

- 聊天独立于媒体：`web/src/chat.ts` ↔ `server/internal/chat/hub.go` 的 `/api/chat` WebSocket；连接参数 `channel`+`token`，与是否进语音房无关。
- 消息模型 `store.Message{ID, ChannelID, UID, Username, Content, CreatedAt}`；`Content` 是 `text`、≤2000 字符；`AddMessage` 落库、`RecentMessages` 取最近 50 条。
- Hub 内存维护 `rooms[channelID] → set(client)`，每条消息落库后 `broadcast`；出站走每个 client 的 `send` channel + `writeLoop`（nhooyr 不允许并发写）。
- 传输帧是 `wsjson`（文本 JSON）。没有任何上传/multipart/对象存储代码，也没有文件相关配置键。

## 方案对比（三条路，按契合度排序）

### 方案 A：Hub 内存中转，即传即弃（推荐默认）

发送方把文件字节交给 hearth，hearth **只在内存里短暂持有**、广播一条"文件消息"给频道在线成员，接收方按 id 拉取；TTL 到期或全部在线成员取过即从内存驱逐。**不写磁盘、不进 DB。**

- **上传**：`POST /api/chat/blob?channel=<id>`（Bearer token，复用 `admitUser` 的频道成员判定），body 是字节流；服务端流式读入一个**有界**缓冲，超过单文件上限即 413。返回 `{blob_id, name, mime, size}`。
- **消息**：沿用聊天 WS，`clientMsg` 加可选结构化字段（`kind:"file"` + `blob_id/name/mime/size`）；服务端广播为一条 `Message`，`Content` 存**元数据 JSON**（不是字节，几十字节，可落库当历史记录，也可选择 `kind=file` 的消息不落库只广播——见「待定」）。
- **下载**：`GET /api/chat/blob/<blob_id>`（同样 token+成员判定），从内存流式吐出，`Content-Type` 用记录的 mime、`Content-Disposition` 用原名。
- **内存治理**（关键，小服务器）：全局在途字节上限（如 64MB）、单文件上限（如图片 10MB / 任意文件 25MB，配置键）、每 blob TTL（如 10 分钟）、每用户在途数上限；超总量时拒绝新上传（429/507）而不是 OOM。驱逐条件：TTL 到 || 已被在线成员全取 || 上传者断线且无人取。
- **优点**：零磁盘、零外部依赖、不碰 NAT/媒体路径、契合"只传输不管理"。图片/截图这类小文件体验好。
- **代价/边界**：**要求收发双方在 TTL 内同时在线**（无离线补收——离线成员回来看到的是"文件已过期"占位）；大文件受内存上限约束；服务端进程内存峰值上升（需背压与硬上限兜底）。

### 方案 B：外部对象存储 + 预签名直传（离线/大文件的升级路，非默认）

hearth 只签 URL、**永不经手字节**：客户端拿预签名 PUT 直传到对象存储（S3/R2/MinIO 等），把对象 URL 发进聊天，接收方直连存储 GET。hearth 磁盘与 DB 都不碰文件。

- **优点**：支持离线补收、大文件、成员错峰；hearth 侧几乎零负载。
- **代价**：引入必须配置的外部存储（与"单文件开箱即用"冲突，只能做可选形态）；有存储成本与生命周期管理（靠 bucket lifecycle 过期）；多一套凭证与 CORS 配置。
- **定位**：作为方案 A 之上的**可选增强**（配了对象存储就走 B，没配就走 A 或禁用文件），不是默认。

### 方案 C：WebRTC DataChannel 点对点（不推荐作为主路）

收发方之间开一条直连 DataChannel，字节纯 P2P、任何服务器都不过。

- **为什么不推荐**：hearth 是 SFU、无 mesh，这会新开一条**独立的 P2P 通道**，直接撞上我们刚花一整晚排查的 NAT/UDP 被分流问题（`docs/plan-client-ice.md`）；且 1 对 N 要建 N 条通道、要额外信令与 TURN 兜底；同样要求双方在线。投入产出比最差。
- 仅在"极大文件 + 点对点 + 能接受复杂度"时才值得,不在本计划范围。

## 推荐

**默认走方案 A**（内存中转、即传即弃），把方案 B 作为"配了对象存储就自动启用"的可选升级，方案 C 明确不做。理由：A 唯一满足"零磁盘 + 零依赖 + 不碰媒体/NAT + 只传输不管理"，正好命中约束；它的短板（要求同时在线、大小受限）对"群里发张截图/小文件"这个主场景可以接受，而离线/大文件用 B 兜。

## 待定（需用户决定）

1. **主方案**：只做 A？还是 A + 预留 B 的可选形态？（C 默认排除）
2. **file 类型消息是否落库**：落库=文件"卡片"（名字/大小/过期状态）留在历史里，字节仍不落库，过期后显示"已过期"；不落库=刷新/重进就看不到该文件消息。倾向落元数据卡片、不落字节。
3. **大小与内存上限**：单图上限、单文件上限、全局在途上限、TTL 的具体默认值（初稿：图 10MB / 文件 25MB / 全局 64MB / TTL 10 分钟，全部做成配置键）。
4. **类型限制**：是否限制可发 MIME（安全上：下载一律带 `Content-Disposition: attachment` + `X-Content-Type-Options: nosniff`，不在浏览器里直接渲染非图片，图片预览用 `<img>` 指向下载端点）。

## 改动清单（按方案 A）

| 位置 | 改动 |
| --- | --- |
| `server/internal/chat/blobstore.go`（新） | 有界内存 blob 存储：`Put(streaming, limits) → id`、`Get(id) → reader`、TTL/总量/驱逐；纯内存、并发安全 |
| `server/internal/api/chat_blob.go`（新） | `POST /api/chat/blob`、`GET /api/chat/blob/{id}`；Bearer + `admitUser` 频道成员判定；流式读写、大小上限、安全响应头 |
| `server/internal/chat/hub.go` | `clientMsg`/`serverMsg` 支持 `kind=file` 与文件元数据；广播路径复用 |
| `server/internal/store`（可选） | 若 file 消息落元数据卡片：`Message` 加 `Kind` 与元数据列（迁移文件），或复用 `Content` 存 JSON + 一个 `kind` 列 |
| `server/internal/api/dyncfg.go` | 配置键：单文件/图片上限、全局在途上限、TTL（Group `chat` 或 `network`） |
| `web/src/chat.ts` | 发送：先 `POST blob` 拿 id、再经 WS 发 file 消息；接收：`kind=file` 渲染 |
| `web/src/views/room.tsx`、`style.css` | 聊天输入区加"发图片/文件"入口（选择/粘贴/拖拽）；消息列表渲染图片预览与文件卡片、过期占位 |

## 验收标准

1. `cd server && go build ./... && go vet ./... && go test ./internal/...`；`cd web && npx tsc --noEmit && npm run build` 通过。
2. 发一张图 → 频道内其他在线成员看到内联预览、点开是原图；发一个文件 → 得到下载。
3. 全程 hearth 的 `/data` 无新增文件、`chat_messages` 无字节（只有元数据）；`du` 对照上传前后无增长。
4. 超单文件上限 → 413；超全局在途上限 → 429/507，且不 OOM（压测并发上传）；TTL 到 → 下载得到"已过期"。
5. 非成员/无 token 访问上传或下载端点 → 403/401；下载响应带 `Content-Disposition: attachment` 与 `nosniff`。
6. 文件传输失败不影响文本聊天与语音/舞台。

## 不做 / 风险

- 不做方案 C（P2P DataChannel）；不把文件字节写入磁盘或数据库；不默认引入对象存储。
- 内存中转的固有边界（要求同时在线、大小受限）如实告知用户，不假装能离线补收；靠总量硬上限与背压防 OOM。
- 隐私铁律：文档、注释、提交信息不出现任何个人部署信息。
