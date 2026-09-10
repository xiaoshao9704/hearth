# 部署模板

这里放的是各家 NAS / 面板应用商店的 Hearth 模板。所有模板描述的都是同一件事：
**一个容器、三条端口、一个 `/data` 卷**，没有 redis、没有第二个服务、没有外部媒体服务器。

## 最简形态：一条 docker run

不想要 compose 文件就用这条，效果完全一样：

```bash
docker run -d --name hearth --restart unless-stopped \
  -p 8080:8080 -p 47720:47720/udp -p 47720:47720/tcp \
  -v hearth-data:/data \
  ghcr.io/xiaoshao9704/hearth:latest
```

```bash
docker exec hearth /app/hearth adduser alice change-me   # 首个账号自动成为 super
```

把 `-v hearth-data:/data` 换成宿主目录（`-v /srv/hearth:/data`）时，该目录必须先 `chown 65532:65532`，
详见下面「`/data` 与目录属主」。

## 模板索引

| 路径 | 平台 | 形态 |
|---|---|---|
| [`docker-compose.yml`](docker-compose.yml) | 通用 docker compose | 最小形态，与 README「三分钟跑起来」一致 |
| [`casaos/`](casaos/) | CasaOS | 带 `x-casaos` 扩展的 compose，可自定义安装或提交 AppStore |
| [`1panel/`](1panel/) | 1Panel | 第三方应用商店目录结构（`data.yml` + 版本目录） |
| [`unraid/`](unraid/) | Unraid | Community Applications 的 XML 模板 |
| [`fnos/`](fnos/) | 飞牛 fnOS | Docker「compose 项目」手动导入的 compose |

各目录里的 README 写了该平台的导入步骤。

## 三条端口

| 端口 | 用途 |
|---|---|
| `8080/tcp` | Web / API / 信令 / WHIP 唯一的 HTTP 端口，同端口默认也收 https（合并模式） |
| `47720/udp` | 媒体端口（语音与投屏共用），**必须**在防火墙 / 安全组放行 |
| `47720/tcp` | ICE-TCP 兜底，只在 UDP 被中间设备接管的网络才用得上，默认与 UDP 同号开启 |

docker 的端口发布不能事后热加，创建容器时就要一并写上。媒体端口不经反代，直接放行到宿主。

Web 端口的宿主侧可以随便改（`-p 9000:8080`），媒体端口不行：**宿主端口必须与容器端口同号**，
因为进程内的媒体内核把自己监听的端口写进 ICE 候选，不同号会让候选指向一个连不通的端口。
真要换号，宿主与容器两侧一起改，再到管理后台把「舞台 → 媒体 UDP 端口 / ICE-TCP 端口」改成同一个值。

## `/data` 与目录属主

`/data` 是唯一的持久化边界：数据库、自动生成的密钥、自签 TLS 根证书都在里面，挂载即持久化 / 备份。

镜像以 `USER 65532`（distroless nonroot）运行。**用命名卷没有属主问题；bind-mount 宿主目录时该目录必须对 uid 65532 可写**，
否则容器启动就写不动数据库。宿主上执行一次即可：

```bash
mkdir -p <宿主目录> && chown -R 65532:65532 <宿主目录>
```

各平台模板按自己的惯例二选一，取舍写在对应文件的注释里。

## 装完之后

```bash
docker exec -it <容器名> /app/hearth adduser alice change-me
```

首个账号自动成为 super（全站最高权限），注册默认邀请制。

然后打开 `http://<主机>:8080`。同一个端口默认也收 `https://<主机>:8080`：**局域网里其它设备第一次要打开
`https://<主机>:8080/ca` 装一次根证书**，装完把地址换成 https 打开，麦克风、投屏、通知这些要安全上下文的权限才给。
宿主机本地用 `http://localhost:8080` 不受影响。
