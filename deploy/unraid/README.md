# Unraid

[`hearth.xml`](hearth.xml) 是 Unraid Community Applications 的容器模板。

## 手动导入（不用等上架）

1. 把 `hearth.xml` 放到 Unraid 的 `/boot/config/plugins/dockerMan/templates-user/my-hearth.xml`。
2. Docker 页 → **ADD CONTAINER** → 顶部 **Template** 下拉里选 `hearth`。
3. 确认端口没被占，Apply。

装 Community Applications 插件的话也可以走「Docker → Add Container → Template repositories」加上本仓库地址，
或直接在 CA 里搜（上架后）。

## 装之前：appdata 目录属主

镜像以 uid 65532（distroless nonroot）运行，而 Unraid 建出来的 appdata 目录是 `nobody:users`（99:100），
容器会写不动数据库、起来就退。**先在 Unraid 终端里跑一次**：

```bash
mkdir -p /mnt/user/appdata/hearth && chown -R 65532:65532 /mnt/user/appdata/hearth
```

这里没有用命名卷：Unraid 的备份、快照与「Appdata Backup」插件都按 `/mnt/user/appdata/<名字>` 组织，
换成命名卷会让容器数据脱离这套惯例，比多跑一条 `chown` 更容易出问题。

## 装完之后

Unraid 终端里建第一个账号（自动成为超级管理员）：

```bash
docker exec -it hearth /app/hearth adduser alice change-me
```

然后点容器图标 → **WebUI**，或直接打开 `http://<Unraid 地址>:8080`。

**局域网里其它设备第一次要打开 `https://<Unraid 地址>:8080/ca` 装一次根证书**（用 http 也能打开），
装完把地址换成 https 打开，麦克风、投屏、通知这些要安全上下文的权限浏览器才给。

## 端口

| 端口 | 用途 |
|---|---|
| `8080/tcp` | Web / API / 信令 / WHIP，同端口默认也收 https |
| `47720/udp` | 媒体端口（语音与投屏共用），**必须**放行 |
| `47720/tcp` | ICE-TCP 兜底，只在 UDP 被中间设备接管的网络才用得上 |

Web 端口的宿主侧随便改；**媒体端口的宿主侧必须与容器侧同号**——进程内的媒体内核把自己监听的端口
写进 ICE 候选，不同号会让候选指向一个连不通的端口。
