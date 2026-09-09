# 计划：同端口 TLS（一个端口同时接明文与 TLS，证书三来源）

状态：**设计定稿（2026-09-09，用户拍板），待实施。** 本文自包含。对应 `docs/roadmap.md` 节点 1。

## 目标与判据

家庭自托管的两个硬条件里，媒体入站已由端口映射解决，缺的是「一个能用 https 打开的地址」。浏览器的麦克风与投屏采集、通行密钥、离线推送、PWA 全部要求安全上下文，只有 `localhost` 例外。

判据：**下载即用**。没域名的人起进程、朋友装一次根证书就能用全部功能；有域名的人把外部工具签出来的证书交给 hearth 即可；放反代后面的人什么都不用改。

## 决策（用户拍板）

- **一个端口同时接明文与 TLS**（2026-09-09 由双端口改为同端口）。`ADDR`（默认 `:8080`）不变，同一个端口上 `http://` 与 `https://` 都能打开：连接的第一个字节是 TLS 握手记录（`0x16`）就走 TLS，否则走明文。反代、`localhost`、OBS 走 `http://`，浏览器直连走 `https://`。不新增端口、不新增 env、端口映射与防火墙规则不变。
- **嗅探用成熟依赖 `github.com/soheilhy/cmux` v0.1.5**（用户倾向成熟依赖少踩坑）：etcd 至今用它在一个端口上分 gRPC 与 HTTP；唯一传递依赖 `golang.org/x/net` 已在依赖图里；核心约 800 行、2021 年后冻结无新版本，API 只用 `New`/`Match(TLS())`/`Match(Any())`/`SetReadTimeout`/`Serve` 五个。不手写嗅探。
- **不做**：ACME 签发/续期、DDNS、HTTP→HTTPS 跳转、HSTS、`.mobileconfig`、TLS 关闭开关（明文照常，TLS 只是额外接受）。签发交给 acme.sh / lego / certbot / Caddy / `tailscale cert`；域名解析交给路由器或 ddns-go。
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
- `upload`：`POST /api/admin/tls/upload`（multipart，`cert` + `key`，admin+），先解析校验成对（`tls.X509KeyPair`），通过才写入 `<data>/tls/upload.crt` / `upload.key`（0600）并把 `tls_cert_source` 落成 `upload`；失败返回 400 且什么都不改。
- 状态：`GET /api/admin/tls`（admin+）返回来源、主体、SAN、NotAfter、SHA-256 指纹、两个监听地址、`self` 时根 CA 指纹，以及对外地址回显（`Announcer.Snapshot()` 外部地址 + `portmap.Mapper.Snapshot()` 的诊断码与详情）。这是 CLAUDE.md 「网络诊断回显走管理接口」的兑现点，`/healthz` 语义不变。

### 监听

- `server/cmd/server/main.go`：`net.Listen` 得到根 listener → `m := cmux.New(root)`；`m.SetReadTimeout(5 * time.Second)`（嗅探期间不发字节的连接不能无限占用，必设）；`tlsL := tls.NewListener(m.Match(cmux.TLS()), &tls.Config{GetCertificate: store.GetCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})`；`plainL := m.Match(cmux.Any())`。同一个 `http.Server{Handler: r}` 分别 `go srv.Serve(tlsL)`、`go srv.Serve(plainL)`，再 `m.Serve()`。只走 HTTP/1.1，不开 h2（WebSocket 信令与 SPA 用不上，省掉 ALPN 与 `TLSNextProto` 的接线）。
- 优雅关闭顺序：`srv.Shutdown(ctx)` → 关根 listener（`m.Serve` 以 `cmux.ErrServerClosed`/`ErrListenerClosed` 返回，两者都视为正常退出，不打错误日志）。cmux 的子 listener 不单独 Close。
- `requestScheme` 已按 `r.TLS != nil` 判 https，`tls.Server` 包过的连接 `r.TLS` 会正确置位，通行密钥 RP ID（Host 去端口）、邀请链接、推送联系地址不需要改。会话不走 cookie（实施时 grep `SetCookie` 确认为零），没有 Secure 属性问题。
- `PortWants`、Windows 防火墙规则、`selfcheck`/`healthcheck` 子命令：不变（同一个端口）。`healthcheck` 继续用明文。
- ICE-TCP 默认开：`lkembed_tcp_port` 默认值改为与 UDP 同号 `47720`，`PortWants` 已会一并申请。注意 ffmpeg 9 的 whip muxer 遇到 answer 里的 TCP 候选会失败，这是测试工具限制：验收配方改用 scratchpad 的 pion `whipverify`，或临时把它设回 `0`。

### 公开入口

- `GET /ca.crt`：无鉴权，明文与 TLS 都能取；`Content-Type: application/x-x509-ca-cert`，`Content-Disposition: attachment; filename="hearth-ca.crt"`。来源不是 `self` 时 404。
- `GET /ca`：无鉴权的安装说明页（服务端渲染的静态 HTML，走现有的 webui 或一个内嵌模板，不进 SPA 路由）。四段：macOS（双击 → 钥匙串 → 始终信任）、Windows（导入到「受信任的根证书颁发机构」）、Android（设置 → 安全 → 安装证书 → CA 证书）、iOS（Safari 下载 → 设置 → 已下载描述文件 → 安装 → 通用 → 关于本机 → 证书信任设置 → 打开完全信任）。页面写明装完后访问 `https://<本机地址>:8080`（**同一个端口，只是前面带 `https://`**，少打一个 `s` 就是不安全上下文、麦克风权限点不开），并列出当前 SAN 里的地址供复制。来源不是 `self` 时显示「当前使用外部证书，无需安装」。

## 前端

- `/api/site` 增加 `tls_source`。
- OBS 推流地址面板（`web/src/views/room/ingest-panel.tsx`）：`tls_source=self` 且页面是 https 时，地址把 `https://` 换成 `http://`（同主机同端口），面板加一行说明「自签证书 OBS 不认，推流地址用 http」；`file`/`upload` 或页面本来就是 http 时照旧用页面 origin。
- 管理后台（`web/src/views/admin.tsx`）「服务器」分区新增「TLS 与对外地址」卡片：来源选择（三选一）、证书摘要（主体、SAN、到期、指纹）、`self` 下的「下载根证书」与安装页链接、`file` 下的两个路径输入、`upload` 下的上传表单、对外地址与映射诊断回显。状态接口返回的监听地址只有一个。新组件放 `web/src/views/admin/tls-card.tsx`，`admin.tsx` 只加接线。
- 登录页 / 大厅在**明文且非 localhost** 访问时已有的「没有 https 浏览器不给权限」提示（若没有则加一条）链接到 `/ca` 页。

## 文档

- README / README.en：「三分钟跑起来」写明同一端口 `https://` 即可用、首次装根证书；「放在反代后面」注明反代用 `http://` 指向该端口；配置表加 `tls_cert_source`、`tls_cert_file`、`tls_key_file`、`tls_self_hosts`；常见问题加「朋友手机装根证书」与「地址要带 https」。
- `docs/selfhost-home.md`：三档的 HTTPS 段改写为「默认自签 + 装根证书；有域名用 `file`/`upload`」；文末路线图段删掉已实现的部分。
- `docs/roadmap.md` 节点 1 状态。

## 不改的

`admission.go`、内核选择器、`/providers/{alias}` 路径、`/healthz` 语义、rtc 包。

## 验收

1. 新数据目录启动：`curl -s http://127.0.0.1:8080/healthz` 与 `curl -sk https://127.0.0.1:8080/healthz` 都 200；`curl -sk https://127.0.0.1:8080/ca.crt | openssl x509 -noout -subject` 是根 CA；`openssl s_client -connect 127.0.0.1:8080 </dev/null | openssl x509 -noout -ext subjectAltName` 含本机 IP；`nc 127.0.0.1 8080` 连上不发字节，5 秒后被服务端断开（嗅探超时生效）。
2. 一台手机装根证书后访问 `https://<局域网 IP>:8080`：无警告，麦克风、投屏观看、PWA 安装、推送订阅可用。
3. SAN 变化：改 `lkembed_public_ip` 或 `tls_self_hosts` 后 60 秒内叶证书 SAN 更新，根指纹不变。
4. `file`：openssl 自造证书指路径 → 生效；覆盖文件后 60 秒内序列号变化；写入坏文件后旧证书仍在、日志有错。
5. `upload`：后台上传生效；证书与私钥不配对返回 400 且原状态不变。
6. 反代：Caddy/nginx 指向 `http://127.0.0.1:8080` 并透传 `X-Forwarded-Proto`，通行密钥注册与登录不受影响。
7. OBS 面板：自签 https 页面下给出的是 http 端口地址；用 ffmpeg 对该地址 WHIP 推流成功（`lkembed_tcp_port` 临时 0）。
8. 明文回归：`http://localhost:8080` 登录、进房、投屏与现状一致；WebSocket 信令在 `http://` 与 `https://` 下都能建连；`go test -race` 覆盖 cmux 接线的关闭路径（Shutdown 后 `m.Serve` 正常返回、无 goroutine 泄漏）。
9. `cd server && go build ./... && go vet ./... && go test ./...`；`cd web && npx tsc --noEmit && npm run build`。
