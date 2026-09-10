# CasaOS / ZimaOS

[`docker-compose.yml`](docker-compose.yml) 是带 `x-casaos` 扩展的 compose，按
[CasaOS AppStore v2 规范](https://github.com/IceWhaleTech/CasaOS-AppStore)（`docs/specs/compose-and-x-casaos.md`）写。

## 自定义安装（不用等上架）

1. CasaOS 桌面右上角「**+**」→「**安装自定义应用**」。
2. 弹窗右上角「**导入**」（Import），把 `docker-compose.yml` 全文粘进去。
3. 确认端口没被占用，点安装。

## 提交到官方 AppStore

把本目录的 `docker-compose.yml` 放到 CasaOS-AppStore 仓库的 `Apps/Hearth/docker-compose.yml`，提 PR。
图标与截图用的是本仓库 `raw.githubusercontent.com` 的直链，不需要另外拷贝资源文件。

## 装完之后

```bash
docker exec -it hearth /app/hearth adduser alice change-me
```

首个账号自动成为 super。打开 `http://<主机>:8080` 即用；局域网里其它设备先打开
`https://<主机>:8080/ca` 装一次根证书，装完用 https 打开才有麦克风、投屏与通知权限。

## 为什么用命名卷而不是 `/DATA/AppData`

镜像以 uid 65532（distroless nonroot）运行，CasaOS 建出来的 `/DATA/AppData/<id>` 属主是 root，
bind-mount 过去容器写不动数据库、装完起不来。命名卷由 docker 创建、属主随容器，装完直接能用。

代价是数据不出现在 CasaOS 的文件管理器里。要备份就导出卷：

```bash
docker run --rm -v hearth-data:/data -v "$PWD:/backup" alpine tar czf /backup/hearth-data.tar.gz -C /data .
```

真想挂宿主目录，先在宿主上执行 `mkdir -p /DATA/AppData/hearth && chown -R 65532:65532 /DATA/AppData/hearth`，
再把 compose 里的卷换成 bind。
