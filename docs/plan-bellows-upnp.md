# 计划：远端 Bellows 的 UPnP 自动端口映射（外网推流时才需要）

状态：计划落盘，待执行。2026-09-02。

## 背景

远端 Bellows（`cmd/bellows`）已能跑在 LiveKit 同一局域网的机器上收 OBS 推流。推流者与 Bellows 同一局域网时
**不需要任何对外端口**：WHIP 信令默认经 hearth 同源反代到 Bellows（`bellows_public_url` 也可直指其内网地址），
`BELLOWS_PUBLIC_IP` 默认取本机出口网卡 IP，媒体在局域网内闭环。只有当有人从外网往该局域网推流时，
Bellows 所在机器才需要对外可达——本计划解决这一步的自动化。

## 目标

Bellows 启动时自动向上游路由器申请端口映射并拿到公网 IP，免手动改路由器；退出时释放。

## 前提（做之前先确认，不满足则整条路线无意义）

1. 路由器开了 UPnP IGD（WANIPConnection:1/2 或 WANPPPConnection）。
2. 路由器 WAN 口拿到的是**真公网 IPv4**。国内宽带常见运营商大内网（CGNAT），此时 UPnP 只能映射到运营商内网地址，
   等于白做；替代路线是 IPv6（Bellows 监听 `[::]`，OBS 走 v6 直连，无需映射）或经 hearth 所在服务器中继（占其上行带宽，一般不可取）。
   判断方法：UPnP `GetExternalIPAddress` 返回值与 `https://api.ipify.org` 看到的一致才是公网。

## 设计

- 依赖：`github.com/huin/goupnp`（`dcps/internetgateway2`，纯 Go）。
- 开关：`BELLOWS_UPNP=1`（默认关），只在 `cmd/bellows` 里做，`rtc/bellows` 包与 hearth 不感知。
- 启动流程：
  1. `internetgateway2.NewWANIPConnection2Clients()`（回退 v1 / PPP）发现 IGD，找不到 → 打日志继续以无映射方式启动。
  2. `GetExternalIPAddress` → 作为 `bellows_public_ip`（优先级低于显式 `BELLOWS_PUBLIC_IP`）。若返回私网地址（10/172.16/192.168/100.64），
     打警告：路由器上游不是公网，映射无效。
  3. `AddPortMapping` 两条：UDP `BELLOWS_UDP_PORT`（媒体）、TCP `BELLOWS_ADDR` 端口（WHIP 信令）；
     内外端口相同，描述 `hearth-bellows`，租约 3600s。
  4. 每 30 分钟续租（重新 AddPortMapping 幂等）；SIGTERM 时 `DeletePortMapping` 两条。
- hearth 侧：`bellows_remote_url` 要填公网可达地址（域名 + DDNS，或直接公网 IP:端口）。公网 IP 会变的线路建议 DDNS；
  不做「Bellows 自动上报公网地址给 hearth」——多一条内部接口只为省一次配置，不值。

## 与 TLS 的关系

外网推流时 OBS bearer 模式的密钥走明文 HTTP 不合适。方案二选一：
- Bellows 所在机器上 Caddy 前置（自动证书，需 80/443 映射，UPnP 同样能申请）；
- OBS 填 hearth 的 https 地址：hearth 的 `/w` 会反代到远端 Bellows（`ProxyUpstream`），信令经 hearth TLS，媒体仍直达 Bellows。
  前提是 hearth 能访问到 Bellows 的 HTTP（也需要映射 TCP 端口），但 Bellows 自己不需要证书。**推荐后者**，零证书运维。

## 验收

- 开启 `BELLOWS_UPNP=1` 启动，日志打出外网 IP 与两条映射；路由器管理页可见。
- 外网机器 ffmpeg WHIP 推 h264+opus 到 `http://<公网IP>:8090/w/{key}` → 201，LiveKit 房间出现 `{user}-obs`。
- kill 进程后路由器映射消失。

## 工作量

约 80 行 Go + 一个依赖，半天。
