# Hearth Server

Go 后端：用户体系（bcrypt + Bearer 会话）、频道管理、LiveKit 令牌签发、频道聊天 WebSocket、内核接入反代（`/providers/{alias}` 下的信令与 WHIP）。

- 存储：sqlite 默认（`modernc.org/sqlite`，纯 Go 无 cgo），`DATABASE_URL` 可切 MySQL/Postgres；可交叉编译到 linux/arm64
- 路由：chi（`github.com/go-chi/chi/v5`），认证/房主校验为中间件
- 聊天 WS：`nhooyr.io/websocket`

## 开发

```bash
cp .env.example .env   # 按需修改
go run ./cmd/server    # 默认监听 :8080（须在 server/ 目录下运行，.env 按工作目录加载）
```

## 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/register` | 注册，返回 `{token, user}` |
| POST | `/api/login` | 登录，返回 `{token, user}` |
| POST | `/api/logout` | 注销当前会话（Bearer） |
| GET | `/api/me` | 当前用户 |
| GET | `/api/channels` | 频道列表 |
| POST | `/api/channels` | 创建频道 `{name}`（1-64 位 `[a-zA-Z0-9_-]`，唯一） |
| POST | `/api/token` | 获取频道对应的 LiveKit JWT `{channel}`，grant 带 canPublish/canSubscribe/canPublishData，24h 过期；频道名即 room 名 |
| GET | `/api/chat?channel=xx&token=xx` | 聊天 WebSocket，进房推最近 50 条历史，消息落库并广播 |
| POST | `/api/ingress` | 获取（首次自动创建）当前用户在该频道的 OBS WHIP 推流地址 `{channel}` → `{url, stream_key}`；每用户每频道一个 |
| POST | `/api/ingress/reset` | 删除旧 ingress 并重建，返回新 `{url, stream_key}`，旧地址立即失效 |
| POST | `/api/channels/{channel}/kick` | （房主）踢出 `{username}`：LiveKit 侧移除其全部设备 + 断开聊天 WS |
| POST | `/api/channels/{channel}/ban` | （房主）封禁 `{username}`：加入黑名单并立即踢出；被封后 `/api/token` 与聊天 WS 均 403 |
| POST | `/api/channels/{channel}/unban` | （房主）解除封禁 |
| GET | `/api/channels/{channel}/bans` | （房主）黑名单列表 |
| POST | `/api/channels/{channel}/invite-only` | （房主）邀请制开关 `{enabled}`；开启后仅房主与白名单可进 |
| GET | `/api/channels/{channel}/members` | （房主）白名单列表 |
| POST | `/api/channels/{channel}/members` | （房主）加白名单 `{username}`（用户须存在） |
| DELETE | `/api/channels/{channel}/members` | （房主）删白名单 `{username}` |

`GET /api/channels` 响应中每个频道带 `invite_only` 与 `is_owner`（对当前用户）。房主 = 频道创建者，不能对自己执行管理操作。

除注册/登录外均需 `Authorization: Bearer <token>`（WS 走 query 参数）。

聊天 WS 协议：服务端下发 `{"type":"history","messages":[...]}` 与 `{"type":"message","message":{...}}`；客户端上行 `{"content":"..."}`。

## 构建

```bash
# 本机
go build -o hearth-server ./cmd/server

# linux/arm64（静态链接无 cgo）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o hearth-server-arm64 ./cmd/server
```

## 环境变量（.env）

见 `.env.example`：`ADDR` 监听地址、`DB_PATH` 数据库文件、`CORS_ORIGIN` 跨域来源、`STATIC_DIR` 可选的前端产物托管目录（部署时指向 `../web/dist` 可单二进制运行）。LiveKit / Ingress 相关 env 是服务实例的锁定来源：`LIVEKIT_API_URL`/`LIVEKIT_API_KEY`/`LIVEKIT_API_SECRET`（`LIVEKIT_API_URL` 缺省从 `LIVEKIT_URL` 推导）任一设置即合成 alias=`livekit` 的锁定实例，`INGRESS_UPSTREAM_URL` 合成 alias=`livekit-ingress`（管理后台「服务实例」里只读）；不配 env 则可在后台注册 DB 实例（同类型可多个）。
