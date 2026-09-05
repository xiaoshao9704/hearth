# 计划：聊天与文件走 LiveKit 数据通道，hearth 不维护聊天长连接、不经手文件

状态：**方案定稿（2026-09-05，用户拍板），待实施。** 本计划自包含，实施会话读完即可开工。

## 决定（用户拍板）

1. 聊天改走 **LiveKit 数据通道**，拆掉 hearth 自己的聊天 WebSocket hub——不再自己维护长连接、重连、心跳。
2. 文件/图片**字节走 LiveKit 数据通道**（Data Streams），经 SFU 扇出，**优先走 sandbox 那条线**；hearth 既不落盘也不占内存。
3. 聊天**历史必须保住**（现有的最近 50 条回放与翻历史是功能，不能回退）：文本与文件"卡片"仍由 hearth 落库，字节不落。

## 现状（已核实）

- 聊天：`web/src/chat.ts` ↔ `server/internal/chat/hub.go` 的 `/api/chat` WS（`main.go:160` 注册，`api.go:35` 持有 `hub`，`api.go:672` 踢人时 `CloseUserChannel`）。消息 `store.Message{ID, ChannelID, UID, Username, Content, CreatedAt}`，`Content` text ≤2000；`AddMessage`/`RecentMessages(limit 50)`。
- 进房票：`server/internal/lktoken/lktoken.go:28` 的 `VideoGrant{CanPublish: !gagged, CanPublishData: true(写死)}`；`admission.go:37` 产出 `CanPublish: !gagged`。`server/internal/lkroom/lkroom.go:156` 的权限更新路径设 `CanPublishData: true`。
- 前端两条线：`room.tsx:181` `stageEngine() = combined ? voiceLine.engine : stageLine?.engine`；`lineFor(role)`。每个成员在拆分形态下同时连 voice（bj lkembed）与 stage（sandbox）两个 LiveKit 房间（诊断日志已证实 role=voice/stage 两个 PC）。
- `AVEngine`（`web/src/engine/types.ts`）没有数据通道方法；`livekit-client 2.22.1` 具备 `sendText/sendFile/streamBytes` 与 `registerTextStreamHandler/registerByteStreamHandler`（`topic`、`destinationIdentities`）。fork 服务端 v1.13.6 ≥ Data Streams 所需版本。

## 设计

### 数据线（data line）

**定义**：`stageEngine()` 非空即用它（拆分形态 = sandbox；合并形态 = 同一条），否则回落 `voiceLine.engine`。聊天文本与文件都走这一条。选 stage 线的理由：sandbox 打洞/上行正常、把扇出带宽从上行有限的 bj 挪走；成员本来就都连着它。

### 文本消息：先落库、再广播

```
发送方  POST /api/channels/{channel}/messages {content}
        → hearth: 鉴权 + admitUser（禁言 403）+ AddMessage → 返回 Message{id,...}
        → 发送方 sendText(JSON(Message), {topic:"chat"})   ← 经数据线 SFU 扇出
接收方  registerTextStreamHandler("chat") → 解析 Message → 按 id 去重 → 渲染
进房/重连  GET /api/channels/{channel}/messages?after=<最大已知 id>&limit=50 → 补齐
```

- **权威在 DB**（架构铁律：业务状态权威在 store，内核只是现场执行器）：POST 成功才广播，广播里带 DB id，接收方按 id 去重；数据线断了也不丢消息，重连时 `after=` 补齐。
- 发送方本地直接渲染 POST 返回的 Message（SDK 不回显自己的数据）。
- 引擎 `onReconnected` → 触发一次 `after=` 补齐；进房首次 `GET` 取最近 50 条（与现状一致）。
- 不做轮询。数据线不在（尚未连上/正在重连）时 POST 照常落库、跳过广播，UI 不阻塞发送。

### 文件/图片：字节走 Data Streams，hearth 只存卡片

```
发送方  POST /api/channels/{channel}/messages {kind:"file", file:{name,mime,size}}
        → hearth 校验（大小上限、禁言）+ 落库卡片 → 返回 Message{id, kind:"file", file:{...}}
        → sendText(JSON(Message), {topic:"chat"})                  ← 卡片先到，接收方先显示"传输中"
        → sendFile(File, {topic:"chat-file", name, mimeType, attributes:{message_id}})  ← 字节经 SFU 扇出
接收方  registerByteStreamHandler("chat-file") → 读全部块 → Blob → 按 attributes.message_id 挂到卡片
        图片：<img src=BlobURL> 内联预览；其它：文件卡片 + 下载（a[download]=BlobURL）
```

- **hearth 不经手字节**：没有上传端点、没有内存 blob 存储、`/data` 无新增文件、DB 只有元数据。
- **在线才有字节**：晚进房的人看到卡片但没有字节，显示"已过期（发送时不在线）"；这是设计边界，如实呈现，不假装可离线补收。
- **大小上限**：配置键 `chat_file_max_mb`（Group `chat`，默认 25）；服务端在 POST 卡片时校验 `size`（超限 413），客户端选文件时先拦。扇出成本 = 大小 × 在线人数，落在数据线的 SFU 上（sandbox），文档写明。
- 发送进度：`sendFile` 的 `onProgress` 驱动发送方卡片进度；接收方 handler 按块累计给进度。
- 安全：下载一律用 `a[download]`（Blob URL），非图片不内联渲染；图片按 `mime` 白名单（png/jpeg/gif/webp）内联，其它一律当文件。

### 禁言 = 也禁数据

- `lktoken.Sign`：`CanPublishData: boolPtr(canPublish)`（与 `CanPublish` 同源，两条线的票都如此）。
- `lkroom` 权限更新路径：翻 `CanPublish` 时同步翻 `CanPublishData`（`MuteUserAudio` 契约"禁言 = 禁全部媒体发布"自然延伸到数据）。
- hearth `POST messages` 对禁言用户 403——两层各自独立成立。
- 踢出/封禁：现有 kick 已把人移出 LiveKit 房间，数据通道随之断；`CloseUserChannel` 与 hub 一起删除。

### 消息模型

`store.Message` 加两列（迁移文件 `server/internal/store/00004_chat_kind.go`，不改 baseline）：
- `kind TEXT NOT NULL DEFAULT 'text'`（`text` | `file`）
- `meta TEXT`（`kind=file` 时为 JSON `{name,mime,size}`；text 为空）

JSON 形状（前后端契约）：

```json
{"id":123,"channel_id":1,"uid":7,"username":"a","kind":"text","content":"hi","created_at":"..."}
{"id":124,"channel_id":1,"uid":7,"username":"a","kind":"file","content":"","file":{"name":"x.png","mime":"image/png","size":12345},"created_at":"..."}
```

### 前端引擎抽象

`AVEngine` 加：

```ts
sendText(topic: string, text: string): Promise<void>;
sendFile(file: File, topic: string, attrs: Record<string,string>, onProgress?: (p: number) => void): Promise<void>;
// 回调
onText?(topic: string, text: string, fromIdentity: string): void;
onFile?(topic: string, info: { name: string; mime: string; size: number; attrs: Record<string,string> }, bytes: Uint8Array, fromIdentity: string): void;
```

`livekit.ts` 用 `localParticipant.sendText/sendFile` 与 `room.registerTextStreamHandler/registerByteStreamHandler` 实现；handler 在 `connect` 前注册（SDK 要求）。注册表只剩 `livekit` 一个实现，其它实现按接口补即可。

## 改动清单

| 位置 | 改动 |
| --- | --- |
| `server/internal/store/00004_chat_kind.go`（新）、`models.go`、`store.go` | `kind`/`meta` 列；`AddMessage` 扩展为可带 kind+meta；`RecentMessages` 加 `after` 游标变体 |
| `server/internal/api/chat_messages.go`（新） | `GET /api/channels/{channel}/messages?after=&limit=`、`POST /api/channels/{channel}/messages`；鉴权 + `admitUser`（禁言 403）+ 大小上限（413）+ 文本长度（≤2000）；响应即 Message JSON |
| `server/internal/api/dyncfg.go` | `chat_file_max_mb`（Group `chat`，Default `25`） |
| `server/internal/lktoken/lktoken.go`、`server/internal/lkroom/lkroom.go` | `CanPublishData` 跟随 `canPublish` |
| `server/internal/chat/hub.go`、`server/cmd/server/main.go`、`server/internal/api/api.go` | **删除** hub、`/api/chat` 路由、`hub` 字段与 `CloseUserChannel` 调用 |
| `web/src/engine/types.ts`、`web/src/engine/livekit.ts` | 数据通道方法与回调 |
| `web/src/chat.ts` | 重写：`fetchHistory(channel, after?)`、`postMessage(channel, body)`；去掉 WS |
| `web/src/views/room.tsx`、`style.css` | 用数据线收发；进房/重连补齐；发送框加图片/文件入口（选择、粘贴、拖拽）；图片预览、文件卡片、进度、"已过期"占位 |

## 验收标准

1. `cd server && go build ./... && go vet ./... && go test ./internal/...`；`cd web && npx tsc --noEmit && npm run build`。
2. 服务端测试：POST 文本落库并返回 id；禁言用户 POST 403；`kind=file` 超 `chat_file_max_mb` 413；`GET after=` 只返回更新的消息；迁移后旧消息 `kind=text`。`lktoken` 测试：gagged → `CanPublishData=false`。
3. 真机（两台设备同频道）：A 发文本 B 即时收到且刷新后仍在历史里；A 发图 B 内联预览、发文件 B 可下载；B 中途刷新重进，看到卡片显示"已过期"；禁言 A 后 A 发不出（POST 403 且 SDK 发数据被拒）。
4. 全程 hearth `/data` 无新增文件；`chat_messages` 只有元数据；bj 出站流量不随文件大小增长（字节走 sandbox）。
5. `server/internal/chat` 目录与 `/api/chat` 路由不复存在；`grep -rn "connectChat\|/api/chat\b" web/src server` 为空。

## 不做 / 风险

- 不做离线补收文件、不做服务端存字节、不做 P2P DataChannel、不做轮询。
- 文件扇出成本落在 sandbox 的上行（大小 × 人数），上限键兜底；文档写明。
- 数据线 = stage 线意味着 `stage_provider=none` 的纯语音部署回落到 voice 线，行为一致只是走 bj。
- 隐私铁律：文档、注释、提交信息不出现任何个人部署信息。
