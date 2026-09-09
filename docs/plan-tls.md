# 计划：TLS 监听（合并模式同端口双协议 / 分开模式双端口，证书三来源）

状态：**设计定稿（2026-09-09，用户拍板），待实施。** 本文自包含。对应 `docs/roadmap.md` 节点 1。

## 目标与判据

家庭自托管的两个硬条件里，媒体入站已由端口映射解决，缺的是「一个能用 https 打开的地址」。浏览器的麦克风与投屏采集、通行密钥、离线推送、PWA 全部要求安全上下文，只有 `localhost` 例外。

判据：**下载即用**。没域名的人起进程、朋友装一次根证书就能用全部功能；有域名的人把外部工具签出来的证书交给 hearth 即可；放反代后面的人什么都不用改。

## 决策（用户拍板）

- **两种监听模式，由两个 env 决定**（2026-09-09 定稿，取代之前的双端口与同端口两版）：
  - `HTTPS_ADDR` 留空（默认）或与 `ADDR` 相同 → **合并模式**：`ADDR`（默认 `:8080`）一个端口同时接明文与 TLS，连接第一个字节是 TLS 握手记录（`0x16`）走 TLS，否则走明文；`http://` 与 `https://` 同一个地址。
  - `HTTPS_ADDR` 另填一个端口 → **分开模式**：`ADDR` 只收明文，`HTTPS_ADDR` 只收 TLS。
  - 两个 env 只在启动时读，不进动态配置。没有单独的「关 TLS」开关：合并模式下 TLS 只是额外接受，明文照常。
- **合并模式的嗅探用成熟依赖 `github.com/soheilhy/cmux` v0.1.5**（用户倾向成熟依赖少踩坑）：etcd 至今用它在一个端口上分 gRPC 与 HTTP；唯一传递依赖 `golang.org/x/net` 已在依赖图里；核心约 800 行、2021 年后冻结无新版本，API 只用 `New`/`Match(TLS())`/`Match(Any())`/`SetReadTimeout`/`Serve` 五个。不手写嗅探。
- **不做**：ACME 签发/续期、DDNS、HTTP→HTTPS 跳转、HSTS、`.mobileconfig`、TLS 关闭开关。签发交给 acme.sh / lego / certbot / Caddy / `tailscale cert`；域名解析交给路由器或 ddns-go。
- **证书三来源**，dyncfg 键 `tls_cert_source`（Group `server`，Options `self|file|upload`，默认 `self`，保存即生效）：
  - `self`：hearth 自己当根 CA 自签；
  - `file`：`tls_cert_file` / `tls_key_file` 指向磁盘上的 PEM（可由 env `TLS_CERT_FILE` / `TLS_KEY_FILE` 锁定），外部工具续期后文件一变自动热换；
  - `upload`：管理后台上传证书与私钥，hearth 存进数据目录。

## 服务端设计

### 新包 `server/internal/tlscert/`

- `Store`：持有当前 `*tls.Certificate`（`atomic.Pointer`），对外只暴露 `GetCertificate` 给 `tls.Config`；解析失败保留旧证书并打日志，永不因坏文件把 HTTPS 端口拉死。
- 三个 loader 对应三来源，由 `tls_cert_source` 切换；后台循环每 60 秒跑一次当前 loader 的 `Check()`：`file`/`upload` 看 mtime 变化即重读；`self` 看重签条件。
- `self` 细节：
  - 根 CA：ECDSA P-256，10 年，`<data>/tls/ca.crt` + `ca.key`（0600）。只在文件不存在时生成，**之后永不重签**（朋友设备上的信任靠它）。
  - 叶证书：`<data>/tls/self.crt` + `self.key`，有效期 397 天（Apple 对所有 TLS 叶证书的 825 天上限之内），到期前 30 天重签；`ExtKeyUsage ServerAuth`、SHA-256、SAN 必填（Apple 不认 CN）。
  - SAN 集合 = `localhost` + `127.0.0.1` + 本机全部非回环单播 IP（v4/v6） + `lite.Announcer.Snapshot()` 的外部地址 + `tls_self_hosts`（逗号分隔的额外主机名/IP，默认空）。集合变化即重签，根不动；私网 IP 变化与公网 IP 变化同样处理。
  - `tls_cert_source` 不是 `self` 时不生成任何文件。
  - **根 CA 防滥用**（用户要求）。装到朋友设备上的根是全局信任锚，必须把它的能力钉死在「只为这台 hearth 签证书」：
    1. 私钥 `ca.key` 永不经任何接口读出（没有导出、备份、显示接口；后台状态接口只给指纹），文件 0600；`hearth` 的日志不打印私钥路径以外的任何内容。
    2. 根证书带 **名称约束**（RFC 5280 `nameConstraints`，critical）：`PermittedIPRanges = [0.0.0.0/0, ::/0]`，`PermittedDNSDomains = ["localhost"] + tls_self_hosts 里的 DNS 名`，`PermittedEmailAddresses`/`PermittedURIDomains` 各给一个不可能匹配的占位（如 `invalid`）以关掉这两类。效果：即使私钥泄漏，用它签出的任何域名证书（`example.com` 之类）都会被 Chrome/Firefox/Apple/Android 拒绝，只剩「冒充按 IP 访问的站点」这一条几乎没有实际价值的路。`KeyUsage` 只有 `CertSign|CRLSign`，`MaxPathLen = 0`（不能再签中间 CA）。
    3. `tls_self_hosts` 新增了根约束里没有的 DNS 名时，不悄悄放宽：状态接口返回 `ca.constraint_stale = true`，后台卡片提示「新增主机名需要重新生成根证书，朋友设备要重装」。有域名的人应改用 `file`/`upload` 拿真证书，页面上写明这一点。
    4. 后台「重新生成根证书」按钮（`POST /api/admin/tls/ca/rotate`，admin+，需二次确认）：删旧根与叶重签，用于私钥可能泄漏（数据目录备份丢失）或约束过期的情况；响应里带新指纹。
    5. 根证书有效期 10 年不变；叶证书 397 天、只签 `ServerAuth`。
- `upload`：`POST /api/admin/tls/upload`（multipart，`cert` + `key`，admin+），先解析校验成对（`tls.X509KeyPair`），通过才写入 `<data>/tls/upload.crt` / `upload.key`（0600）并把 `tls_cert_source` 落成 `upload`；失败返回 400 且什么都不改。
- 状态：`GET /api/admin/tls`（admin+）。这是 CLAUDE.md 「网络诊断回显走管理接口」的兑现点，`/healthz` 语义不变。响应形状（前后端按此并行实施，不得改动）：
  ```json
  {"source":"self","mode":"merged","http_addr":":8080","https_addr":"",
   "cert":{"subject":"CN=hearth","sans":["localhost","127.0.0.1","192.168.1.10"],"not_after":"2027-10-01T00:00:00Z","fingerprint_sha256":"AB:CD:..."},
   "ca":{"fingerprint_sha256":"12:34:...","not_after":"2036-09-09T00:00:00Z","constraint_stale":false},
   "cert_file":"","key_file":"",
   "external":{"addresses":["203.0.113.5"],"probed_at":"2026-09-09T08:00:00Z"},
   "portmap":{"mode":"auto","diagnosis":"ok","detail":"...","v6_detail":"","pinholes":[]}}
  ```
  `mode` 是 `merged|split`；`ca` 在来源不是 `self` 时为 `null`；`cert` 在当前没有可用证书时为 `null`；`portmap` 字段直接映射 `portmap.Status`，`mode=off` 时 `diagnosis` 为 `off`。来源切换与路径修改走现有的动态配置保存接口（`tls_cert_source`/`tls_cert_file`/`tls_key_file`/`tls_self_hosts` 四个 dyncfg 键），不另开写接口；上传是 `POST /api/admin/tls/upload`（multipart 字段 `cert`、`key`），成功返回与 GET 相同的形状。

### 监听

- `server/cmd/server/main.go`，合并模式：`net.Listen` 得到根 listener → `m := cmux.New(root)`；`m.SetReadTimeout(5 * time.Second)`（嗅探期间不发字节的连接不能无限占用，必设）；`tlsL := tls.NewListener(m.Match(cmux.TLS()), tlsCfg)`，`plainL := m.Match(cmux.Any())`。同一个 `http.Server{Handler: r}`（**`srv.TLSConfig` 保持 nil**，这样 `srv.Serve` 会自动挂好 HTTP/2 处理器）分别 `go srv.Serve(tlsL)`、`go srv.Serve(plainL)`，再 `m.Serve()`。`tlsCfg = &tls.Config{GetCertificate: store.GetCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}`，HTTP/2 由此协商出来。
- 分开模式：`srv` 照旧 `ListenAndServe` 于 `ADDR`；再起 `srvTLS := &http.Server{Addr: httpsAddr, Handler: r, TLSConfig: tlsCfg}` 并 `ListenAndServeTLS("", "")`（HTTP/2 自动配置）。两个 server 同一个 handler、同一个优雅关闭。
- 优雅关闭顺序：`srv.Shutdown(ctx)`（分开模式再 `srvTLS.Shutdown`）→ 关根 listener（合并模式下 `m.Serve` 以 `cmux.ErrServerClosed`/`ErrListenerClosed` 返回，两者都视为正常退出，不打错误日志）。cmux 的子 listener 不单独 Close。
- 启动日志一行写清模式：`监听于 :8080（http 与 https 同端口）` 或 `http 监听于 :8080，https 监听于 :8443`。
- `requestScheme` 已按 `r.TLS != nil` 判 https，`tls.Server` 包过的连接 `r.TLS` 会正确置位，通行密钥 RP ID（Host 去端口）、邀请链接、推送联系地址不需要改。会话不走 cookie（实施时 grep `SetCookie` 确认为零），没有 Secure 属性问题。
- `PortWants`：分开模式把 `HTTPS_ADDR` 端口以 `tcp` 非 Strict 加入映射（Desc `hearth https`）；合并模式不变。Windows `service install` 的防火墙规则同理。`selfcheck`/`healthcheck` 子命令一律走 `ADDR` 明文。
- ICE-TCP 默认开：`lkembed_tcp_port` 默认值改为与 UDP 同号 `47720`，`PortWants` 已会一并申请。注意 ffmpeg 9 的 whip muxer 遇到 answer 里的 TCP 候选会失败，这是测试工具限制：验收配方改用 scratchpad 的 pion `whipverify`，或临时把它设回 `0`。

### 公开入口

- `GET /ca.crt`：无鉴权，明文与 TLS 都能取；`Content-Type: application/x-x509-ca-cert`，`Content-Disposition: attachment; filename="hearth-ca.crt"`。来源不是 `self` 时 404。
- `GET /ca`：无鉴权的安装说明页（服务端渲染的静态 HTML，一个内嵌模板，不进 SPA 路由）。页面必须把「装的是什么、为什么、能做什么、不能做什么、怎么卸载」讲清楚，顺序固定：
  1. **这是什么**：「这是这台 Hearth 自己生成的根证书。装上它，你的浏览器才会信任 `https://<地址>`，麦克风、投屏、通知才能用。」并给出根证书的 SHA-256 指纹，写明「装之前可以和站长核对这串指纹，不一致就不要装」。
  2. **它能做什么、不能做什么**：只能让这台 Hearth 的地址显示为安全；带名称约束，不能用来冒充任何域名网站；私钥只在服务器上，网页下载的是公开的证书文件。
  3. **分系统安装步骤**，每一步写清会看到的系统提示原文与该点什么：macOS（下载 → 双击 → 钥匙串访问里找到「Hearth CA」→ 显示简介 → 信任 → 「使用此证书时」选「始终信任」→ 输密码）；Windows（下载 → 双击 → 「安装证书」→ 当前用户 → 「将所有证书放入下列存储」→ 浏览选「受信任的根证书颁发机构」→ 会弹「安全警告」问是否安装，选「是」）；Android（下载 → 设置 → 安全/加密与凭据 → 安装证书 → CA 证书 → 会提示「你的数据将不再是私密的」，这是系统对所有用户 CA 的固定提示 → 仍然安装 → 选文件）；iOS/iPadOS（用 Safari 打开链接 → 系统提示「此网站正尝试下载一个配置描述文件」选「允许」→ 设置 → 顶部「已下载描述文件」→ 安装 → 输锁屏密码 → 再点「安装」→ 然后**必须**去 设置 → 通用 → 关于本机 → 证书信任设置 → 打开「Hearth CA」的完全信任，否则 Safari 仍然报不安全）。
  4. **装完打开**：给出要访问的 https 地址（合并/分开模式各自的形态），并强调「地址前面要带 https，少打一个 s 麦克风权限就点不开」。
  5. **怎么卸载**：四个系统各一行（钥匙串删除 / certmgr 删除 / 设置 → 安全 → 清除凭据 / 设置 → 通用 → VPN 与设备管理 → 移除描述文件）。
  6. **站长须知**（折叠区）：根证书私钥在服务器数据目录，备份泄漏时到后台「重新生成根证书」，朋友需要重装。页面写明装完后访问的地址：合并模式是 `https://<本机地址>:8080`（**同一个端口，只是前面带 `https://`**，少打一个 `s` 就是不安全上下文、麦克风权限点不开），分开模式是 `https://<本机地址>:<HTTPS 端口>`；并列出当前 SAN 里的地址供复制。来源不是 `self` 时显示「当前使用外部证书，无需安装」。

## 前端

- `/api/site` 增加 `tls_source`（`self|file|upload`）与 `http_port`（`ADDR` 的端口号；合并模式下与页面端口相同）。
- OBS 推流地址面板（`web/src/views/room/ingest-panel.tsx`）：`tls_source=self` 且页面是 https 时，地址改成 `http://<页面主机名>:<http_port>/...`（合并模式下就是同主机同端口换协议），面板加一行说明「自签证书 OBS 不认，推流地址用 http」；`file`/`upload` 或页面本来就是 http 时照旧用页面 origin。分开模式 + 家庭 NAT 把 http 端口改派成别的外部号时这个地址会不对，文档注明「OBS 从局域网推，或用合并模式」，代码不处理。
- 管理后台（`web/src/views/admin.tsx`）「服务器」分区新增「TLS 与对外地址」卡片：来源选择（三选一）、证书摘要（主体、SAN、到期、指纹）、`self` 下的「下载根证书」与安装页链接、根指纹、「重新生成根证书」（二次确认，说明朋友需重装）、`constraint_stale` 时的提示、`file` 下的两个路径输入、`upload` 下的上传表单、对外地址与映射诊断回显。状态接口返回的监听地址只有一个。新组件放 `web/src/views/admin/tls-card.tsx`，`admin.tsx` 只加接线。
- 登录页 / 大厅在**明文且非 localhost** 访问时已有的「没有 https 浏览器不给权限」提示（若没有则加一条）链接到 `/ca` 页。

## 文档

- README / README.en：「三分钟跑起来」写明同一端口 `https://` 即可用、首次装根证书；「放在反代后面」注明反代用 `http://` 指向该端口；配置表加 `tls_cert_source`、`tls_cert_file`、`tls_key_file`、`tls_self_hosts`；常见问题加「朋友手机装根证书」与「地址要带 https」。
- `docs/selfhost-home.md`：三档的 HTTPS 段改写为「默认自签 + 装根证书；有域名用 `file`/`upload`」；文末路线图段删掉已实现的部分。
- `docs/roadmap.md` 节点 1 状态。

## 不改的

`admission.go`、内核选择器、`/providers/{alias}` 路径、`/healthz` 语义、rtc 包。

## 验收

1. 合并模式（默认）新数据目录启动：`curl -s http://127.0.0.1:8080/healthz` 与 `curl -sk https://127.0.0.1:8080/healthz` 都 200；`curl -sk --http2 -I https://127.0.0.1:8080/healthz` 首行 `HTTP/2 200`；`curl -sk https://127.0.0.1:8080/ca.crt | openssl x509 -noout -subject` 是根 CA；`openssl s_client -connect 127.0.0.1:8080 </dev/null | openssl x509 -noout -ext subjectAltName` 含本机 IP；`nc 127.0.0.1 8080` 连上不发字节，5 秒后被服务端断开（嗅探超时生效）。
2. 一台手机装根证书后访问 `https://<局域网 IP>:8080`：无警告，麦克风、投屏观看、PWA 安装、推送订阅可用。
3. SAN 变化：改 `lkembed_public_ip` 或往 `tls_self_hosts` 加 IP 后 60 秒内叶证书 SAN 更新，根指纹不变；往 `tls_self_hosts` 加 DNS 名后状态接口 `constraint_stale=true`、后台出提示；`openssl x509 -text` 看根证书有 critical 的 `Name Constraints`，用根私钥手工签一张 `example.com` 的叶证书，`openssl verify -CAfile ca.crt` 报 `permitted subtree violation`。
3b. 根轮换：后台点「重新生成根证书」后指纹变化、叶证书由新根签发、旧根签的叶被 `openssl verify` 拒绝。
4. `file`：openssl 自造证书指路径 → 生效；覆盖文件后 60 秒内序列号变化；写入坏文件后旧证书仍在、日志有错。
5. `upload`：后台上传生效；证书与私钥不配对返回 400 且原状态不变。
6. 反代：Caddy/nginx 指向 `http://127.0.0.1:8080` 并透传 `X-Forwarded-Proto`，通行密钥注册与登录不受影响。
7. OBS 面板：自签 https 页面下给出的是 http 端口地址；用 ffmpeg 对该地址 WHIP 推流成功（`lkembed_tcp_port` 临时 0）。
8. 分开模式：`HTTPS_ADDR=:8443` 启动，`http://127.0.0.1:8080` 只收明文（`curl -sk https://127.0.0.1:8080/` 握手失败）、`https://127.0.0.1:8443/healthz` 200 且 `HTTP/2`；`PortWants` 多一条 `hearth https`；`HTTPS_ADDR` 与 `ADDR` 相同时等价于合并模式。
9. 明文回归：`http://localhost:8080` 登录、进房、投屏与现状一致；WebSocket 信令在 `http://` 与 `https://` 下都能建连；`go test -race` 覆盖 cmux 接线的关闭路径（Shutdown 后 `m.Serve` 正常返回、无 goroutine 泄漏）。
10. `cd server && go build ./... && go vet ./... && go test ./...`；`cd web && npx tsc --noEmit && npm run build`。
