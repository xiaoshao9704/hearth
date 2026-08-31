# Hearth

低延迟、高码率的浏览器直播 / 聊天室（KOOK/Discord 式频道 + LiveKit 音视频 + OBS WHIP 推流）。

## 目标

- **低延迟**：端到端延迟目标 < 1s（WebRTC 场景可到 300–500ms）
- **高码率**：支持 1080p60、8Mbps+ 推流（投屏码率/帧率/分辨率可调）
- **纯浏览器**：推流与观看均无需插件/客户端
- **实时聊天**：频道内 WebSocket 文字聊天
- **频道管理**：房主踢出/封禁、邀请制白名单

## 技术选型

| 模块 | 方案 | 理由 |
|------|------|------|
| 媒体服务器 | LiveKit + Ingress（WHIP） | 高码率转发，浏览器/OBS 均可推流 |
| 后端 | Go + sqlite（`modernc.org/sqlite`，无 cgo） | 单二进制，可交叉编译 ARM64 |
| 前端 | Vite + 原生 TypeScript | 无框架，体积小 |
| 聊天 | WebSocket | 简单可靠 |

## 目录结构

```
hearth/
├── server/   # Go：REST API + 聊天 WebSocket + LiveKit 令牌/Ingress/房间管理（sqlite 存储）
├── web/      # 浏览器端：Vite + 原生 TypeScript，livekit-client 接入
├── deploy/   # 自托管：中央 .env + 配置模板 + init.sh + docker-compose.yml
├── Dockerfile# 一体化镜像（前端构建 + Go 构建 + 精简运行时）
└── docs/     # 设计与调研文档
```

## 自托管快速开始（docker-compose）

前置：一台有公网 IP 的主机（80/443 及媒体端口可达），已安装 Docker 与 envsubst（gettext）。

```bash
# 1. 中央配置：填 DOMAIN 与 LiveKit 密钥
cd deploy
cp .env.example .env
$EDITOR .env

# 2. 生成 livekit / ingress / Caddy 配置（输出到 deploy/generated/，已 gitignore）
./init.sh

# 3. 构建并启动（默认 hearth + livekit 两服务）
docker compose up -d --build

# 4. 创建第一个账号（注册接口默认关闭）
docker compose exec hearth /app/hearth adduser <用户名> <密码>
```

然后访问 `http://<主机>:8080` 登录使用。hearth-server 内置反向代理（`/lk` 信令、`/w` WHIP、Web、API 同端口），不强制需要 Caddy/nginx，内网可裸 HTTP 使用。

要点：

- **TLS（可选）**：`docker compose --profile caddy up -d` 启用 Caddy 自动 Let's Encrypt（需 80/443 公网可达、域名已解析、`.env` 填好 DOMAIN）；自有证书写法见 `deploy/Caddyfile.template` 末尾注释
- **OBS 推流（可选）**：`.env` 取消注释 `LIVEKIT_CONFIG` 与 `INGRESS_UPSTREAM_URL` 两行，重新 `./init.sh`，然后 `docker compose --profile ingress up -d`；房间页「OBS」按钮获取推流地址
- **数据库（可选）**：默认 sqlite（`/data` 卷）；`.env` 设 `DATABASE_URL` 可切 MySQL/Postgres
- **媒体端口**：`.env` 的 `RTC_UDP_PORT`/`RTC_TCP_PORT` 需在防火墙/安全组放行（不经过反代）
- **ARM64**：compose 全部镜像含 arm64 变体，树莓派可用

## 开发

前置：一台可访问的 LiveKit 服务器（可用上面的 compose 起一套，或对接既有实例）。

```bash
# 1. 后端（终端一）
cd server
cp .env.example .env    # 填入 LiveKit 地址与密钥
go run ./cmd/server     # 监听 :8080（须在 server/ 目录下运行，.env 按工作目录加载）

# 2. 前端（终端二）
cd web
cp .env.example .env    # VITE_SERVER_URL=http://localhost:8080
npm install
npm run dev             # http://localhost:5173
```

浏览器打开 `http://localhost:5173`：登录 → 创建/进入频道 → 麦克风/摄像头/**投屏（1080p60、8Mbps、h264 优先）**，右侧为参与者列表与文字聊天；房主额外有「频道设置」与踢出/封禁入口。

交叉编译（如树莓派 linux/arm64）：

```bash
cd server
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o hearth-server-arm64 ./cmd/server
```

详见 `server/README.md` 与 `web/README.md`。

## 里程碑

1. **MVP**：多频道聊天室 + LiveKit 音视频 + 高码率投屏 + WebSocket 聊天 + OBS WHIP 推流（当前）
2. 频道管理（踢出/封禁/邀请制，已实现）、码率自适应（simulcast/SVC）
3. 水平扩展：SFU 级联、聊天分片

## License

MIT © 2026 shaokunyin，详见 [LICENSE](LICENSE)。
