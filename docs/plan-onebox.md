# 计划：单文件开箱即用（三系统原生启动、内置 TLS、DDNS、自签名、自检）

状态：**阶段三已实现（2026-09-04）**——DDNS 四个提供方（DuckDNS/Cloudflare/DNSPod/阿里云，去重+退避+状态落盘）、
`service install|uninstall|start|stop|status` 三系统服务化（macOS LaunchAgent / Linux systemd 用户级与 --system /
Windows SCM + 防火墙规则）、服务模式日志轮转（`<data>/hearth.log` 10MB×5）、`hearth version` 与管理后台新版本提示
（GitHub Releases，可关）。Cloudflare/DNSPod/阿里云无凭证未实测（请求构造与签名有单测）；Windows 服务形态只做了
交叉编译验证。阶段二已实现——进程内 TLS（off/acme/selfsigned 热切换）、80/443 纳入 PortWants、
首启向导 `#/setup`、`/api/admin/netcheck` 自检与管理后台「网络」页均已落地；ACME 路径只做到编译与配置生效，未在真实公网域名下实测。
**阶段二、三的 TLS 与 DDNS 实现将返工**：改用 CertMagic + libdns，并新增 IP 证书能力，见第七节（2026-09-04 决策，实施以该节为准）。
目标版本与内核收敛（ember/bellows 退场，只留进程内 LiveKit）同批；本文档只写「一个文件、双击能用、朋友能连上」这条线，内核收敛另起文档。

## 动机与边界

今天的完整形态是 docker 镜像 + 卷；裸机跑要自己准备前端产物目录（`STATIC_DIR`）、反代与证书（`deploy/Caddyfile.template`），
Windows/macOS 没有发布产物。而浏览器对 `getUserMedia`/`getDisplayMedia` 的要求是 **HTTPS 或 localhost**，所以「没有证书」
等于「只能自己听自己」。要把门槛压到：下载一个文件 → 双击 → 浏览器打开向导 → 填一次 → 把链接发给朋友。

不做的：
- 代码签名与公证（Windows SmartScreen / macOS Gatekeeper 的提示只能靠文档说明，签名证书是采购问题不是代码问题）。
- 托盘图标 / GUI 窗口。保持控制台程序 + 服务安装子命令，向导在浏览器里。
- CGNAT / 上游不可控线路的穿透（与 `plan-portmap.md` 的边界一致，不做 TURN）。
- 自有 DNS 服务器、`.local` 广播。

## 一、现状盘点（差在哪）

| 项 | 现状 | 缺口 |
|---|---|---|
| 前端产物 | 独立目录，`STATIC_DIR` 指过去 | 未编进二进制，「单文件」不成立 |
| 发布矩阵 | 只有 `linux/{amd64,arm64}` | 无 windows / darwin 产物 |
| TLS | 进程只监听 HTTP；HTTPS 靠外置 Caddy | 无证书 = 无麦克风/投屏 |
| 域名 | `PUBLIC_URL` 手填 | 无 DDNS，家宽 IP 变了链接就失效 |
| 端口可达 | 自动端口映射已完成（PCP/NAT-PMP/UPnP，级联，v6 pinhole） | 只映射了 HTTP 与媒体端口；80/443 没有纳入 |
| 首次配置 | `adduser` CLI + `.env` | 无向导；Windows 用户不会开终端 |
| 数据目录 | `DB_PATH` 默认当前目录 `hearth.db` | 双击启动时工作目录不确定 |
| 守护 | docker restart | 三系统无服务安装 |
| 自检 | `/healthz` 只报活；portmap 日志有诊断行 | 没有「朋友能不能连上」的可见结论 |

进程内已有的可复用件：`portmap.Mapper`（申请/续租/级联/v6 pinhole）、`lite.Announcer`（STUN 探测出的公网 IPv4/IPv6，`ExternalIPv4s`）、
`dyncfg`（管理后台落库即生效 + env 锁定）、`publicBase/requestScheme`（邀请链接与信令地址按请求推导，HTTPS 下自动变 wss）。

## 二、目标形态

```
hearth.exe / hearth（macOS, Linux）
  ├─ 前端 embed 进二进制
  ├─ 首启：data 目录不存在 → 建目录、生成密钥、监听 → 打开浏览器 https://localhost:<port>/#/setup
  ├─ 向导：管理员账号 → 域名与 DDNS（可跳过）→ 证书方式（ACME / 自签名）→ 自检结果 → 邀请链接
  ├─ 运行：HTTP :8080（仅重定向到 HTTPS 与 ACME HTTP-01）+ HTTPS :8443；portmap 把外部 80→8080、443→8443
  └─ 子命令：service install|uninstall|start|stop、adduser、promote、healthcheck、cert export-ca
```

外部端口固定用 80/443 是为了链接不带端口、ACME 两种挑战都能过；内部监听高位端口是为了三系统都不需要 root
（Linux/macOS 绑 <1024 要特权）。映射不可用时（无 UPnP、CGNAT）向导明确告知「只能局域网使用」并给出自签名路线。

## 三、分项设计

### 1. 前端内嵌与三系统构建

- `server/internal/webui`：`//go:embed all:dist` + `Handler()`。Go 的 embed 不能引用模块外目录，CI 与 `Dockerfile` 在
  `go build` 前把 `web/dist` 复制到 `server/internal/webui/dist`（gitignore，留 `dist/.keep`）；目录为空时 `Handler()` 返回 nil，
  `main.go` 回落到现有 `STATIC_DIR` 逻辑（开发期 vite dev server 不受影响）。
- `release.yml` 矩阵：`linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64`，`CGO_ENABLED=0`。
  依赖树里 sqlite 是纯 Go（`modernc.org/sqlite`），pion 与 LiveKit fork 纯 Go；`portmap` 已有 `gateway_{linux,darwin,windows,other}.go`。
  **起手第一件事是把六个目标 `go build` 一遍**（见第六节验收 0），有不通过的平台先修编译再谈功能。
- 产物：`hearth_<ver>_<os>_<arch>.{tar.gz,zip}`，里面只有一个二进制（web 目录不再随包发）；Windows 用 zip。
- Windows：保持控制台子系统（不要 `-H windowsgui`，否则没日志）；`os.Interrupt` 已处理，`syscall.SIGTERM` 常量在 Windows 上无害。
- 数据目录：新增 `--data <dir>` / `HEARTH_DATA`；默认**可执行文件旁的 `data/`**（便携优先，与 docker 的 `/data` 语义一致），
  写不进去（Program Files、`/usr/local/bin`）再回落到用户目录（`%LOCALAPPDATA%\Hearth`、`~/Library/Application Support/Hearth`、`$XDG_DATA_HOME/hearth`）。
  `DB_PATH` 默认改为 `<data>/hearth.db`，证书、DDNS 状态、日志都在 `<data>` 下；`.env` 仍从工作目录读，再从 `<data>/.env` 读一次。

### 2. TLS（进程内）

新增包 `server/internal/tlsx`，三种模式落 `dyncfg` 键 `tls_mode`（`off` / `acme` / `selfsigned`，默认首启向导决定；env `TLS_MODE` 可锁定）：

- **`acme`**：`golang.org/x/crypto/acme/autocert`，缓存目录 `<data>/certs`，HTTP-01 与 TLS-ALPN-01 都开（autocert 内建）；
  域名来自 `site_domain`；账户邮箱可空。ACME 目录可配（`acme_directory`，默认 Let's Encrypt，留给 ZeroSSL / 内网 step-ca）。
  HTTP :8080 的 handler = autocert 的 `HTTPHandler(redirect)`，其余全部 301 到 HTTPS。
  DNS-01 不做在第一阶段；DDNS 提供方带 TXT 接口的（Cloudflare、DuckDNS）在第二阶段接进来，解决 80 口被运营商封的线路。
- **`selfsigned`**：首启生成一张本地 CA（`<data>/certs/ca.{crt,key}`，10 年）与一张叶子证书（1 年，自动续签），
  SAN = `localhost`、`127.0.0.1`、本机全部 LAN 地址、Announcer 探测到的公网 IPv4/IPv6、`site_domain`（若填）；地址集合变化即重签（挂在 `RefreshAnnounce` 之后）。
  管理后台「网络」页给 **「下载 CA 证书」**（`GET /api/admin/tls/ca.crt`）与三系统安装说明，装了就不再有警告；不装也能用，只是每台设备点一次「继续访问」。
  这是**局域网/无域名**场景的正解，不是 ACME 的降级。
- **`off`**：现状（反代在前的部署，`X-Forwarded-Proto` 已被 `requestScheme` 尊重）。
- 监听：`https_addr` 默认 `:8443`，`ADDR` 继续是 HTTP。`tls_mode≠off` 时 `PortWants` 追加 `tcp 8443→外部 443` 与 `tcp 8080→外部 80`
  （portmap 已支持内外端口不同）。`publicBase` 在有 `site_domain` 时用 `https://<域名>`（外部 443 不带端口）。
- 证书状态（模式、域名、到期、上次续签错误）进 `adminOverview`，后台可见。

### 3. DDNS

新增包 `server/internal/ddns`：

```go
type Provider interface {
    Name() string
    Update(ctx context.Context, host string, v4 netip.Addr, v6 netip.Addr) error // 任一为零值表示不更新该记录
}
```

- 内置实现按「零成本先行」排：**DuckDNS**（只要 token，免费子域，最适合没有域名的人）、**Cloudflare**（API token + zone）、
  **DNSPod（腾讯云）**、**阿里云**；`dyndns2` 通用协议（No-IP、dynv6 等）第二阶段。
- 配置走 `dyncfg`：`ddns_provider`（选择器，`off` 默认）、`ddns_host`、`ddns_zone`（可空）、各提供方的凭证键（`ddns_duckdns_token`、`ddns_cf_token`…，Secret）。
- 触发：与 `RefreshAnnounce` 同节拍——探测结果里的公网地址变了才调 `Update`，不变不打 API；失败退避重试，状态（上次更新时间、当前记录值、错误）进 `adminOverview`。
- 公网地址来源就是 `lite.Announcer` 的 STUN 结果 + portmap 网关给的外部地址（映射成功但网关报私网地址时以 STUN 为准，与 `plan-portmap.md` 的口径一致）。
- 选了 DDNS 且 `site_domain` 为空时自动填 `ddns_host`，向导里 TLS 默认切到 `acme`。

### 4. 首启向导与自检

- 路由 `#/setup`：仅当 `users` 表为空时可用（服务端 `GET /api/site` 返回 `needs_setup`），走完即失效；未登录访问其它路由在 `needs_setup` 时一律跳向导。
  四步：管理员账号（落 `super`，见 `plan-roles-guests.md`）→ 域名与 DDNS（可跳过）→ 证书方式（有域名默认 ACME，否则自签名）→ 自检与邀请链接。
- 首启自动开浏览器（`xdg-open` / `open` / `rundll32 url.dll,FileProtocolHandler`），`--no-browser` 关闭；服务模式下不开。
- 自检 `GET /api/admin/netcheck`（管理后台「网络」页常驻 + 向导第四步）：
  - 端口映射：复用 `portmap.Status`（方法、外部地址、级联跳数、`stallReason`）。
  - 公网地址：Announcer 快照（STUN 结果、映射结果、显式配置）。
  - DDNS：记录当前解析值（用系统 resolver 查 `site_domain`）与探测到的公网地址是否一致。
  - 证书：模式、SAN、到期。
  - **外部可达**：向 `https://<公网地址或域名>:443/healthz` 发一次请求。家宽路由多数不支持 hairpin，失败不能判死，
    结论分三档：可达 / 不确定（本机回环打不到，需用手机流量验证）/ 明确失败（映射未建立或证书域名不匹配）。不接任何第三方探测服务。
- `/healthz` 语义不变（只报活、无副作用），自检全部走管理接口。

### 5. 三系统服务化

- `hearth service install|uninstall|start|stop|status`：
  - Windows：`golang.org/x/sys/windows/svc` 注册服务，同时 `netsh advfirewall firewall add rule` 放行 HTTP/HTTPS/媒体 UDP（卸载时删）；
  - macOS：写 `~/Library/LaunchAgents/com.hearth.server.plist`（用户级，不需要 sudo）；
  - Linux：`systemd --user` 单元优先，`--system` 参数写系统单元。
- 服务模式下日志写 `<data>/hearth.log`（轮转 5×10MB），控制台模式仍打 stdout。
- 不做自更新；`hearth version` 打印版本，管理后台「服务状态」页显示版本并按 GitHub Releases 的 `latest` 提示新版本（一次请求，可关）。

## 四、配置键汇总（全部走 dyncfg，env 同名锁定）

| 键 | 组 | 默认 | 说明 |
|---|---|---|---|
| `site_domain` | site | 空 | 公开域名；填了即替代按请求推导的 `publicBase` |
| `tls_mode` | tls | 向导决定 | `off` / `acme` / `selfsigned` |
| `https_addr` | tls | `:8443` | HTTPS 监听 |
| `acme_directory` / `acme_email` | tls | LE / 空 | ACME 目录与账户邮箱 |
| `ddns_provider` | ddns | `off` | 选择器：`off` / `duckdns` / `cloudflare` / `dnspod` / `aliyun` |
| `ddns_host` | ddns | 空 | 要更新的主机名 |
| `ddns_zone` | ddns | 空 | DNS zone；建议显式填写，留空才逐级猜测 |
| `ddns_*_token` 等 | ddns | 空 | 各提供方凭证（Secret） |

`PUBLIC_URL` 保留为 env 锁定形态（有它就不看 `site_domain`）。

## 五、分阶段

**阶段一：单文件三系统**（不碰 TLS）
1. 六目标交叉编译通过；`webui` embed；数据目录规则；发布矩阵与 zip/tar 产物；README 增「下载即用」段。
2. 验收：三系统各下载一个文件双击起来，`http://localhost:8080` 能登录、能自己进房（localhost 例外允许麦克风）。

**阶段二：TLS 与向导**
1. `tlsx` 两种模式 + 80/443 纳入 `PortWants` + 向导页 + 自检接口与「网络」页。
2. 验收：有域名走 ACME 一次成功、续签日志正常；无域名走自签名，下载 CA 装到手机后无警告；朋友用手机流量能进房投屏。

**阶段三：DDNS 与服务化**
1. `ddns` 四个提供方 + 状态回显；`service` 子命令三系统；日志轮转；版本提示。
2. 验收：断开重拨换 IP 后 3 分钟内记录更新、在途会话不断；重启机器后服务自起。

**阶段四（可选）**：DNS-01（Cloudflare/DuckDNS）、`dyndns2` 通用协议。

## 六、验收与风险

- **验收 0**：`for os in linux windows darwin; for arch in amd64 arm64; CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build ./cmd/server` 全绿，
  这是阶段一的入口条件。**写方案时已试编：`windows/amd64` 通过，`darwin/arm64` 失败**——`github.com/livekit/protocol/utils/hwstats/cpu_all.go`
  调用 `go-osstat/cpu` 的 `Stats/Get`，而 go-osstat 的 darwin 实现只在 cgo 版（`cpu_darwin_cgo.go`）里定义它们，`cpu_darwin_nocgo.go` 是不含这两个符号的空壳，`CGO_ENABLED=0` 下编不过。
  两条路：(a) CI 的 darwin 目标改在 `macos-latest` runner 上 `CGO_ENABLED=1` 原生编译（产物仍是单文件，但 CI 多一台 runner、失去「全程 linux 交叉编译」的整齐）；
  (b) `livekit/protocol` 也走 replace 到自己的 fork，给 `hwstats` 加一个 `darwin && !cgo` 的降级文件（返回零值，LiveKit 只用它做负载统计，不影响转发）。
  推荐 (b)：改动是一个文件，且 `livekit-server` 已经在 replace 自家 fork，多一个同源 fork 不增加心智负担。
- **80/443 映射不到**：运营商封 80 的线路 HTTP-01 必失败，TLS-ALPN-01 只要 443 通即可，autocert 会自动试第二种；两者都不通再等阶段四的 DNS-01。
- **自签名的公网 IP SAN**：IP 变了叶子证书重签，已建立的连接不受影响，新连接要重新「继续访问」，可接受。
- **Windows 防火墙**：非服务模式首次监听时系统会弹一次询问，用户点「允许」即可；服务安装时用 `netsh` 写规则，避免服务账号弹不出对话框。
- **macOS Gatekeeper**：未签名二进制首次运行要右键「打开」，README 写清；不做公证。
- **端口冲突**：8080/8443 被占用时启动报错并给出 `--addr` 提示，不自动换端口（换了端口邀请链接就对不上映射）。
- **与内核收敛的耦合**：`PortWants` 里媒体端口随 ember/bellows 退场减少，本计划只增加 80/443 两条 TCP，互不影响。

---

## 七、修订：CertMagic 迁移、libdns 与 IP 证书（2026-09-04 决策）

阶段二、三已按前文实现（`autocert` + 手写四家 DDNS）。本节是对那两段的**返工决定**，实施前以本节为准；
前文相应段落保留作背景，不再是执行依据。

### 7.1 为什么返工

三件事指向同一个依赖组合：

- **手写签名不可回归**：阿里云那份 HMAC-SHA1 签名（签名串拼接、RFC3986 编码、时间戳与 nonce）只能靠离线复算固定向量验证，
  我们永远拿不到凭证做真实回归。DNSPod 的记录查找也已发现两个确定的分支缺口。
- **DNS-01 在 autocert 上做不了**：`autocert` 只支持 HTTP-01 与 TLS-ALPN-01，阶段四要自己写整套 DNS-01 挑战流程。
- **没有域名的部署只能自签名**：现状要求每台设备装本地 CA，手机上尤其劝退，这是整个「双击就能用」链路里最差的一环。

`CertMagic`（Caddy 的 TLS 自动化内核，ACME 客户端是 `acmez`）+ `libdns`（Go 生态事实标准的 DNS 提供方接口，
`RecordGetter`/`RecordAppender`/`RecordSetter`/`RecordDeleter`/`ZoneLister`，已发 v1，记录类型化为 `libdns.Address`/`libdns.TXT` 等）
一次解决三条：DNS-01 与 DDNS 共用同一套 provider 配置（用户只配一次凭证），签名交给上游维护，
并且具备签发 IP 证书所需的 ACME profile 能力。

### 7.2 IP 证书（新增能力，取代「无域名只能自签名」）

Let's Encrypt 的 **6 天（160 小时）短时效证书与 IP 地址证书已于 2026-01-15 正式可用**，
经 ACME `shortlived` profile 选用，IPv4 与 IPv6 都支持。对本项目的意义：**有公网 IP 但没有域名的部署不再需要装 CA**，
手机、别人的电脑直接打开 `https://<公网IP>` 即可，是「把链接发给朋友」这条主线上最大的一块补齐。

约束（写进向导文案，不要让用户自己撞）：

- **IP 标识只能用 HTTP-01 或 TLS-ALPN-01 验证**，DNS-01 按 RFC 8738 对 IP 标识不适用。
  所以「运营商封 80」的线路仍要靠 443 走 TLS-ALPN-01，两个端口都不通就没有 IP 证书，回落自签名。
- **必须是公网可直连的地址**，CGNAT 线路不可用。判定复用 `netcheck` 的三档结论，不另起探测。
- **6 天寿命意味着续期必须可靠**：机器离线超过一周回来就是过期证书，续期失败必须**回落到自签名继续提供 HTTPS**，
  绝不能让 HTTPS 端口变成握手失败（这正是当前实现的自锁问题之一）。
- **IP 变了要重签**：触发点已经现成——`Announcer` 的探测刷新与 `Mapper.OnChange` 回调，与 DDNS 同节拍。
  要给重签加去抖与失败退避，避免 IP 抖动打穿 ACME 速率限制。

### 7.3 TLS 实现改为 CertMagic

- `tlsx` 内部把 `autocert` 换成 `CertMagic`；对外的 `tls_mode`（`off`/`acme`/`selfsigned`）与 `Sync` 热切换语义不变。
- `acme` 模式下的签发标识：`site_domain` 非空用域名；为空且 `netcheck` 判定公网可直连，用探测到的公网 IP + `shortlived` profile。
- 新增 `acme_profile` 键（留空 = CA 默认；签 IP 证书时自动用 `shortlived`）。
- **续期失败一律回落自签名**，`selfsigned` 从「用户选项」升格为「永远在场的兜底」；证书状态里明确显示当前实际在用哪一种。
- **待确认（实施第一步就验）**：Caddy 侧曾有 IP 证书签发不通的报告（caddyserver/caddy#7399，2025-12 提出、现已关闭，
  关闭原因未核实）。CertMagic/acmez 对 IP 标识与 `shortlived` profile 的支持要用 Let's Encrypt **staging** 环境实测一次，
  通过再动 `tlsx`；不通就先只做 libdns 那半，IP 证书延后。

### 7.4 DDNS 改用 libdns（部分）

四家现成模块成熟度不一，按现状分别处置，不做一刀切：

| 提供方 | 现成模块 | 决定 |
|---|---|---|
| 阿里云 | `libdns/alidns` v1.0.7（2026-04，已跟上 libdns v1） | **换**。删掉手写签名，收益最大 |
| Cloudflare | `libdns/cloudflare`（pkg.go.dev 显示 v0.2.2 / 2025-10 非模块最新版） | **换**，顺带解决 zone 列表不翻页；引入前确认其 v1 适配版本 |
| DNSPod | `libdns/dnspod` v0.0.3（2021-08，未适配 libdns v1，已被 caddy-dns 从构建剔除） | **不换**，自持实现并修掉已知缺陷 |
| DuckDNS | `libdns/duckdns` v0.3.0（2025-04，未到 v1） | **不换**，它只是一个 GET，不值得加依赖 |

- **zone 由用户显式给出**：libdns 的 provider 一律要求调用方传 zone，不从主机名反查。新增 `ddns_zone` 键（留空时退回逐级拆分猜测）。
  这条同时消灭「主域拆分循环」这个已知缺陷来源。
- 我们自己的 `ddns.Provider` 接口保留（`Update(ctx, host, v4, v6)`，零值不动该记录），libdns 模块在适配层里包一下，
  换实现不动 Runner 的去重、退避、状态落盘。
- 阶段四的 DNS-01 直接复用同一份 provider 配置，不再另立一套凭证键。

### 7.5 阶段调整

**阶段五（本节内容）**，接在当前所有阻塞项修复之后：

1. staging 实测 CertMagic 的 IP 证书与 `shortlived` profile（决定 7.3 是全做还是只做 libdns 那半）。
2. `tlsx` 迁到 CertMagic，续期失败回落自签名，证书状态回显实际在用的种类。
3. 阿里云、Cloudflare 换 libdns；新增 `ddns_zone`；DNSPod 与 DuckDNS 保留并修缺陷。
4. 向导第三步改为三选一：域名 + ACME / 仅公网 IP + IP 证书 / 自签名；按 `netcheck` 结论给默认值与不可用原因。
5. 阶段四（DNS-01）降级为 7.4 落地后的小增量。

验收：无域名机器在 staging 上签出 IP 证书、手机不装 CA 直接打开；断网一周后回来能自愈（先自签名再重签）；
换 IP 后新会话用新证书、在途会话不断。

### 7.6 新增风险

- **多一层依赖**：CertMagic 会带入 `acmez` 与 libdns 系列模块。相对收益（删掉手写签名与自研 ACME 挑战）值得，
  但要在 `go.mod` 里盯住间接依赖膨胀。
- **ACME 速率限制**：6 天证书 + IP 抖动 = 签发频率显著高于域名证书。去抖与退避是必须项，不是优化项。
- **staging 与生产目录切换**：`acme_directory` 已是配置键，实测走 staging，发布默认回生产；不要把 staging 目录写死进默认值。

### 7.7 staging 闸门实测结论（2026-09-04）

用 `CertMagic v0.25.4`、`acmez v3.1.6` 与 Let's Encrypt staging 目录做了最小实测：IPv6 IP 标识加
`shortlived` profile 的订单能被 CA 接受并进入 HTTP-01 验证，但当前验证环境的入站连接超时；IPv4 又无法取得可供验证的公网映射。
因此 IPv4、IPv6 都没有完成实际签发，严格闸门判定为**不通过**。

本轮只落地 7.4 的 libdns/DDNS 部分；`tlsx` 迁 CertMagic、DNS-01、IP 证书接线和向导证书模式调整全部延后。
后续必须在 IPv4、IPv6 均公网可直连的环境中让 staging 实际签发成功，才重新打开这些工作项。生产默认 ACME 目录保持不变。
