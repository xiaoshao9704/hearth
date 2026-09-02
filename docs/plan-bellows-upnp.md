# 计划：远端 Bellows 的 UPnP 自动端口映射（外网推流时才需要）

状态：计划落盘，待执行。2026-09-02。

## 背景

远端 Bellows（`cmd/bellows`）已能跑在 LiveKit 同一局域网的机器上收 OBS 推流。推流者与 Bellows 同一局域网时
**不需要任何对外端口**：WHIP 信令经 hearth 同源反代到 Bellows（通行证模型下 OBS 只能走这条路），
`BELLOWS_PUBLIC_IP` 默认取本机出口网卡 IP，媒体在局域网内闭环。只有当有人从外网往该局域网推流时，
Bellows 所在机器才需要对外可达——本计划解决这一步的自动化。

## 目标

Bellows 启动时自动向上游路由器申请端口映射并拿到公网 IP，免手动改路由器；退出时释放。

## 前提与判定方式

1. 路由器开了 UPnP IGD（WANIPConnection:1/2 或 WANPPPConnection）。
2. **不要用 IP 段判断映射是否有效**。路由器 WAN 口是私网地址时映射未必无效：常见拓扑是上游设备（光猫/前置路由）
   拨号后把全部端口 DMZ/转发到本路由器，此时本路由器上的 UPnP 映射对外同样生效，但 `GetExternalIPAddress`
   返回的是上游分配的私网地址，真实公网 IP 只能靠外部探测（HTTP 回显服务）得到。真正无效的只有运营商 CGNAT
   且上游不可控的情况，替代路线是 IPv6（Bellows 监听 `[::]`，OBS 走 v6 直连，无需映射）。
   判定必须做**实际可达性测试**：hearth 所在的公网服务器是天然的外部探测点——管理后台一个「检测推流入口可达性」
   动作，hearth 对远端公网地址做 TCP 连接（WHIP 端口 `/healthz`）与一次 UDP 探测；UDP 侧最省事的信号是
   pion 的 ICE 结果本身（外网推流者建会话后 ICE 是否 connected），不另起探测协议。
3. **局域网推流者与 UPnP 无关**：同一内网即使没有映射也应能直连。为此媒体通告要同时包含 LAN 与公网候选：
   `SetNAT1To1IPs([]string{公网IP}, ICECandidateTypeSrflx)` 以 srflx 形式**追加**公网候选而保留本机 host（LAN）候选，
   而不是现在用 `ICECandidateTypeHost` 把 host 候选整体替换成单一 IP——替换语义下填公网 IP 会让 LAN 推流者
   绕经 NAT 回环，填 LAN IP 又让外网推流者不可达。这一条与 UPnP 无关，Bellows 现在就该改。

## 设计

- 依赖：`github.com/huin/goupnp`（`dcps/internetgateway2`，纯 Go）。
- 开关：`BELLOWS_UPNP=1`（默认关），只在 `cmd/bellows` 里做，`rtc/bellows` 包与 hearth 不感知。
- 启动流程：
  1. `internetgateway2.NewWANIPConnection2Clients()`（回退 v1 / PPP）发现 IGD，找不到 → 打日志继续以无映射方式启动。
  2. 公网 IP 仍以外部探测（`lite.ProbePublicIP`）为准；`GetExternalIPAddress` 只作为探测失败时的兜底，
     返回私网地址不视为错误（见「前提」第 2 条），只在日志里并列打印两者供排查。
  3. `AddPortMapping` 两条：UDP `BELLOWS_UDP_PORT`（媒体）、TCP `BELLOWS_ADDR` 端口（WHIP 信令）；
     内外端口相同，描述 `hearth-bellows`，租约 3600s。
  4. 每 30 分钟续租（重新 AddPortMapping 幂等）；SIGTERM 时 `DeletePortMapping` 两条。
- hearth 侧：`bellows_remote_url` 要填公网可达地址（域名 + DDNS，或直接公网 IP:端口）。公网 IP 会变的线路建议 DDNS；
  不做「Bellows 自动上报公网地址给 hearth」——多一条内部接口只为省一次配置，不值。

## 与 TLS 的关系

外网推流时 OBS bearer 模式的密钥走明文 HTTP 不合适。方案二选一：
- Bellows 所在机器上 Caddy 前置（自动证书，需 80/443 映射，UPnP 同样能申请）；
- OBS 填 hearth 的 https 地址：hearth 的 `/providers/{alias}/w`（bellows-remote 实例）会反代到远端 Bellows（`ProxyUpstream`），信令经 hearth TLS，媒体仍直达 Bellows。
  前提是 hearth 能访问到 Bellows 的 HTTP（也需要映射 TCP 端口），但 Bellows 自己不需要证书。**推荐后者**，零证书运维。

## 验收

- 开启 `BELLOWS_UPNP=1` 启动，日志打出外网 IP 与两条映射；路由器管理页可见。
- 外网机器 ffmpeg WHIP 推 h264+opus 到 `http://<公网IP>:8090/w/{key}` → 201，LiveKit 房间出现 `{user}-obs`。
- kill 进程后路由器映射消失。

## 工作量

约 80 行 Go + 一个依赖，半天。
