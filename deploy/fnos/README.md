# 飞牛 fnOS

**这是手动导入形态。** 截至 2026-09-10 查证：fnOS 有官方开发者文档与 `.fpk` 应用包格式
（`fnpack` CLI，`docker-project` 资源类型可把 compose 打进包里，见
[developer.fnnas.com](https://developer.fnnas.com/docs/guide)），但**没有公开的第三方应用商店提交仓库或模板规范**
——上架走人工申请渠道。所以这里不打 `.fpk`，只给一份直接粘贴就能跑的 compose。

## 一、Compose 项目（推荐）

1. 打开 **Docker** → **Compose** 标签页 → **新增项目**。
2. 填「项目名称」（例如 `hearth`），选一个「路径」。
3. 「来源」选 **创建 docker-compose.yaml**，把 [`docker-compose.yml`](docker-compose.yml) 全文粘进去。
4. 勾选「创建项目后立即启动」，点 **完成**。

## 二、一条 docker run（SSH 里）

fnOS 开了 SSH 的话，不建项目也行：

```bash
docker run -d --name hearth --restart unless-stopped \
  -p 8080:8080 -p 47720:47720/udp -p 47720:47720/tcp \
  -v hearth-data:/data \
  ghcr.io/xiaoshao9704/hearth:latest
```

## 装完之后

```bash
docker exec hearth /app/hearth adduser alice change-me
```

首个账号自动成为 super（全站最高权限）。然后打开 `http://<NAS 地址>:8080`。

**局域网里其它设备第一次要打开 `https://<NAS 地址>:8080/ca` 装一次根证书**（用 http 也能打开），
装完把地址换成 https 打开，麦克风、投屏、通知这些要安全上下文的权限浏览器才给。

## 端口与目录

| 端口 | 用途 |
|---|---|
| `8080/tcp` | Web / API / 信令 / WHIP，同端口默认也收 https |
| `47720/udp` | 媒体端口（语音与投屏共用），**必须**放行 |
| `47720/tcp` | ICE-TCP 兜底，只在 UDP 被中间设备接管的网络才用得上 |

Web 端口的宿主侧随便改；**媒体端口的宿主侧必须与容器侧同号**——进程内的媒体内核把自己监听的端口
写进 ICE 候选，不同号会让候选指向一个连不通的端口。

持久化用命名卷：镜像以 uid 65532（distroless nonroot）运行，而 fnOS 不会替你把挂载目录 chown 成
容器用户，直接挂 `/vol1/<uid>/docker/hearth` 这类宿主目录属主对不上，容器写不动数据库。
真要挂宿主目录，先在 SSH 里执行一次：

```bash
mkdir -p /vol1/<uid>/docker/hearth && chown -R 65532:65532 /vol1/<uid>/docker/hearth
```

再把 compose 里的 `hearth-data:/data` 换成 `/vol1/<uid>/docker/hearth:/data`。
