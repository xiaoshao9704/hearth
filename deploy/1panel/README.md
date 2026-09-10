# 1Panel

按 [1Panel 第三方应用商店规范](https://github.com/1Panel-dev/appstore/wiki/How-to-submit-your-own-application)
组织的应用目录：

```
hearth/
├── data.yml              应用元数据（key / 多语言简介 / 架构 / 类型）
├── logo.png              180x180 图标
├── README.md             中文应用说明（面板里展示）
├── README_en.md          英文应用说明
└── 0.10.0/               版本目录
    ├── data.yml          安装表单（formFields）
    ├── docker-compose.yml
    ├── data/             持久化目录（挂到容器 /data）
    └── scripts/init.sh   起容器前把 data/ 交给 uid 65532
```

## 本地导入（不用等上架）

把 `hearth/` 整个目录放进 1Panel 的本地应用目录，然后在面板里「应用商店 → 本地应用 → 同步/更新应用列表」：

```bash
cp -r hearth /opt/1panel/resource/apps/local/
```

路径以你的 1Panel 安装目录为准。同步后在「本地应用」里就能看到 Hearth，点安装、填 Web 端口即可。

## 提交到官方商店

把 `hearth/` 放到 [1Panel-dev/appstore](https://github.com/1Panel-dev/appstore) 的 `apps/hearth/` 提 PR。

## 两个要点

- **媒体端口固定 47720**（udp + tcp 各一条），没有做成表单项，原因见 `hearth/README.md`。装机前确认这个端口没被占。
- **持久化目录属主**：镜像以 uid 65532（distroless nonroot）运行，1Panel 建出来的应用目录是 root 属主，
  所以版本目录里带了 `scripts/init.sh`，在起容器前把 `data/` chown 成 65532。若你的 1Panel 版本不执行
  `init.sh`，装完发现容器起不来，手动补一次即可：

  ```bash
  chown -R 65532:65532 /opt/1panel/apps/hearth/<应用名>/data
  ```
