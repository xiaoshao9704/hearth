# 计划：自动端口映射（UPnP IGD + PCP / NAT-PMP）

状态：P1 已实现（2026-09-03）。取代 `docs/plan-bellows-upnp.md`（那份只覆盖远端 Bellows 一个进程，
且其中「改 SetNAT1To1IPs 语义」一条已由 `lite.Announcer` 的地址改写规则实现），该文档已删除。

## 目标

NAT 后的宽带线路上部署 hearth，进程启动时自己向上游网关申请端口映射并展示对外地址，
部署者不必进路由器后台做端口转发。**目标是把「能不能连上」这件事从人工配置变成自检 + 可见反馈**，
不是穿透一切网络。

非目标：CGNAT / 上游不可控线路的穿透（不做 TURN 中继、不做打洞代理）；
aio 内嵌 livekit/ingress 子进程的端口（理由见第三节）。

**IPv6 不是 v4 映射的退路**：对端有没有 v6 我们控制不了（移动网络、公司网、老旧 CPE 都可能只有 v4），
它只能是并列的额外候选。详见第十二节。

## 一、映射谁

| 端口 | 协议 | 来源 | 外部端口须与内部相同 |
| --- | --- | --- | --- |
| HTTP（Web/API/信令/WHIP 反代） | TCP | `ADDR`，默认 8080 | 否 |
| ember 媒体 | UDP | `ember_udp_port`，默认 47700 | 否（见下） |
| 进程内 bellows 媒体 | UDP | `bellows_udp_port`，默认 47710（仅当 ingest 槽位选中进程内 bellows） | 否（见下） |

### 媒体端口：宣告端口与监听端口解耦

ICE 宣告的地址端口本来就不必等于本地监听端口——srflx 候选天生如此：对端照候选往外部端口发连通性检查，
NAT 转回内部端口，ICE-Lite 被动回复，回包经 NAT 还原成外部地址与候选匹配，正常连通。
候选对 pion 只是一段发给对端的文本，它自己不拿来做判断。

约束只在实现：pion 的 `ICEAddressRewriteRule` 只改写候选的 **IP**
（`ice/external_ip_mapper.go` 的 `validateIPString` 只解析 IP，端口原样取自本地监听端口）。
今天全靠它做宣告，所以映射到不同外部端口就会宣告出错误的端口。

本方案自己补上这一步，P1 内完成：

- `lite.AppendMappedCandidate(sdp string, ext, local netip.AddrPort) string`：在每个 `m=` 段已有候选之后插一行
  `a=candidate:<foundation> 1 udp <priority> <ext.IP> <ext.Port> typ srflx raddr <local.IP> rport <local.Port>`。
  foundation 取不与已有候选冲突的值，priority 按 srflx 惯例低于 host。
- 调用点：ember 的 answer 与 offer 两处出口（`ember.go:439` / `ember.go:579`）、bellows 的对应出口。
  都是把 `pc.LocalDescription().SDP` 这个字符串直接发给对端，插一行纯文本，**不回灌给 pion**（它不需要知道）。
- 外部端口与内部相同时不追加，仍走 pion 的改写规则，既有路径零改动。
- 落地时要一并实测的细节：ICE-Lite 的候选是否在 `SetLocalDescription` 返回后就齐了
  （host 候选不经 gathering，预期是齐的）；BUNDLE 下插一段还是每段都插；浏览器对 `raddr/rport` 的宽容度。
- 长线仍可给 pion 提 PR 让 `External` 收 `ip:port`，届时删掉这段字符串处理。

TCP HTTP 口本就没有这个约束，只要把**实际对外地址**告诉用户（见「可见性」）。

## 二、协议与依赖

发现与申请按「快的先试」串行：

1. **PCP（RFC 6887，MAP opcode）** → 网关 UDP 5351，version=2。定长二进制报文（24 字节头 + 36 字节 MAP），
   自研约 150 行，无依赖。续租/删除必须复用同一 nonce；删除 = lifetime 0。
2. **NAT-PMP（RFC 6886）** → 同一网关同一端口，version=0。PCP 请求被回 `UNSUPP_VERSION(1)` 时改发 v0 报文，
   共用同一 socket 与重试逻辑（约 50 行）。
3. **UPnP IGD** → SSDP M-SEARCH 发现 + 设备描述 XML + SOAP 调用，慢（2~3s）但消费级路由器覆盖面最广。
   **v1 与 v2 都要支持**：不少设备默认以 IGDv1 身份宣告（miniupnpd 的 `force_igd_desc_v1`，OpenWrt 默认开启），
   v1 是主路径而非回退路径。动作按版本择优：v2 优先 `AddAnyPortMapping`（端口被占时网关自行分配可用端口并返回，
   省去客户端重试），回退 v1 的 `AddPortMapping`；诊断用 v2 的 `GetListOfPortMappings`，v1 则按 index 遍历。

依赖取舍：

- PCP/NAT-PMP **自研**。报文定长、无厂商方言，第三方库（`jackpal/go-nat-pmp`）也不覆盖 PCP，自研反而更少代码。
- UPnP **用 `github.com/huin/goupnp`**（`dcps/internetgateway2`，纯 Go，自带 v2→v1→PPP 回退）。
  自研要踩的是设备描述 XML 的厂商方言（相对 URL、服务定位、WANIPConnection vs WANPPPConnection），
  这类兼容性坑没有自研价值。只引 `internetgateway2` 子包，传递依赖是已有的 `golang.org/x/net`、`x/sync`。
- **默认网关发现自研**（约 100 行，三个平台文件，各用系统原生途径）：
  linux 读 `/proc/net/route`（Destination 全零那行的 Gateway，小端十六进制）；
  darwin 用 `golang.org/x/net/route` 直接读路由表；windows 调 `windows.GetAdaptersAddresses` 取 `FirstGatewayAddress`。
  `x/net`/`x/sys` 已在依赖树里，只是提升为直接依赖，不新增模块。
  不引 `jackpal/gateway`：它唯一多给的是 Windows，而其 Windows 实现是 shell out 跑 `route print` 解析文本——
  本地化输出有解析风险，服务模式下还会闪控制台窗口，比自研调 API 更差。

**自研与否的判据是「我们要用多深」**：PCP 要用到 nonce 认领、epoch 检测、v6 pinhole、按错误码分类处置——
这些都在自研代码里自然延伸，而现成库（`jackpal/go-nat-pmp`）压根不覆盖 PCP，用它反而堵死扩展。
UPnP 我们只用五个动作、永远不会更深，需要的全是别人趟过的厂商方言，所以收库。

### 实现要点（协议细节只记决策与坑）

**PCP（首选）**：24 字节公共头 + 36 字节 MAP 数据。`nonce` 随机生成后**必须保存**——续租与删除靠它认领同一条映射；
删除 = lifetime 设 0。响应里的 `epoch time` 是网关的运行时长，**回退即表示网关重启、映射已丢**，
不必等续租周期，立刻重建。IPv6 下同一个 MAP 就是防火墙 pinhole 语义。

**NAT-PMP（回落）**：PCP 请求被回 `UNSUPP_VERSION(1)` 时改发 12 字节 v0 报文，同一网关同一端口（5351）。
无 nonce、无 pinhole，其余语义一致。重试按 RFC 6886 从 250ms 指数退避，2~3 次即可（还有 UPnP 兜底，不必退到 9 次）。

**UPnP**：SSDP 发现后按版本择优调用（见上）。租约一律申请有限时长，不用永久映射。

**错误码决定行为**，不能一律当作「失败」：

| 返回 | 含义 | 处理 |
| --- | --- | --- |
| PCP `NOT_AUTHORIZED(2)` / UPnP `606` | 网关**主动禁用**了端口转发（多见于行为探测误判，见第六节） | 停止重试，给出指向路由器选项的诊断提示 |
| UPnP `718` ConflictInMappingEntry | 外部端口被占 | v2 改用 `AddAnyPortMapping`；v1 提示换端口 |
| UPnP `725` OnlyPermanentLeasesSupported | 老设备只支持永久映射 | 用 lease=0 重发，续租退化为定期重建 |
| PCP `NO_RESOURCES(8)` / UPnP `501` | 网关资源不足或后端故障 | 退避重试 |

## 三、包与接线

新包 `server/internal/portmap`，中性命名，不进 `rtc/`（HTTP 端口与内核无关）：

```go
type Want struct { Proto string; Port int; Desc string; StrictPort bool } // StrictPort: 外部端口必须相同
type Mapping struct { Proto string; Internal, External int; ExternalIP, Method string; ExpiresAt time.Time }

func New(wants func(context.Context) []Want) *Mapper
func (m *Mapper) Run(ctx context.Context)        // 后台：申请 → 续租 → 变化打日志
func (m *Mapper) Snapshot() ([]Mapping, Status)  // 给 healthz / 管理后台回显
func (m *Mapper) Close(ctx context.Context)      // 撤销全部映射
```

- 租约 3600s，半程续租；每轮**幂等重发**而不是查询后决定 —— 路由器重启丢映射时自然自愈。
- `wants` 是 getter 每轮读：`ember_udp_port` 等是动态配置，后台改了端口下一轮撤旧加新。
- `Run` 完全异步，任何失败都不影响启动；发现不到网关就停在「无网关」状态，不重试轰炸（退避到 10 分钟一次）。

接线点：

- `cmd/server`：把 `cfg.Addr` 的端口 + 当前选中内核的媒体端口交给 Mapper。
  **附带必要改动**：`main` 现在是 `log.Fatal(http.ListenAndServe(...))`，没有优雅退出，映射撤销无处挂。
  改成 `signal.NotifyContext` + `http.Server.Shutdown` + `Mapper.Close`。这是本方案唯一的既有代码改动。
- `cmd/bellows`（P1 一并做，取代 `plan-bellows-upnp.md`）：同一个包，映射 `BELLOWS_UDP_PORT`
  与 HTTP 端口，宣告同样走上面的候选追加。远端形态跑在别人的局域网里，**是最需要自动映射的场景**。
- **远端 Bellows 状态接口**（P2）：远端进程没有数据库也不回调 hearth，它的映射成败 hearth 无从得知，
  而管理后台恰恰要展示。三件事分开，不复用同一个端点：**探活**（`/healthz`，匿名、恒 200、无副作用）、
  **周期刷新**（进程内 ticker，不靠外部请求触发）、**诊断回显**（方法、每条映射、诊断分类、宣告地址）。
  诊断走独立的状态端点并**用共享密钥签名**才返回——它暴露的是对方的内网拓扑，不能裸奔。
  hearth 在管理后台渲染 bellows-remote 实例时拉一次，连不上本身也是有用的信息
  （「hearth 访问不到远端的 HTTP 端口」）。
- aio 内嵌 livekit/ingress 端口：本期不做。默认档 `stage_provider=none` 时它们根本不起；
  真要做也该由 aioinit 自己声明 wants，等有人用再说。

## 四、宣告：统一由 lite 自己产出候选

pion 的地址改写规则只能改 IP、不能改端口（第一节），而映射给出的外部端口未必等于内部端口。
既然 SDP 出口已经要自己追加候选行，就把宣告收归一处，不再分散在「pion 改写规则」和「自己追加」两处：

- **pion 只宣告本机真实地址**（host 候选，内网对端靠它直连），不再向它传 STUN 探测结果。
- **所有外部地址由 lite 在 SDP 出口追加为 srflx 候选**，按优先级取：
  1. **端口映射结果** `外部IP:外部端口` —— 两者都准确，这是唯一可信的来源；
  2. **STUN 探测结果** `公网IP:本地端口` —— 端口是「假设 NAT 端口保持」，只在 1:1 NAT（云服务器绑公网 IP 的形态）成立；
  3. 都没有则不追加，只留 host 候选。
- 显式配置的 `*_public_ip` 保持覆盖语义（只通告该地址），继续走 pion 的改写规则实现：
  它的端口天然就是本地监听端口，语义正确，而「删掉 host 候选」交给 pion 比自己删 SDP 行更稳。

顺带把候选类型说诚实了：外部地址本就是 srflx（服务器反射地址），此前借 `AsCandidateType: Host` + append
把它伪装成 host 候选，是受改写规则 API 所限。

### STUN 只能给出 IP，给不出可用的端口

这是把 STUN 结果排在映射之后的原因，也是不再把它喂给 pion 的原因：

- 探测走的是临时端口的 socket，探到的映射属于**那个临时端口**，与媒体端口无关；
- 改从媒体端口的 socket 探测也不成立：mux 一旦接管该 socket，STUN 响应（无 USERNAME 属性、无法按 ufrag 分发）
  会被当作无法识别的包丢弃，只能在 mux 接管前探一次；
- 而那一次探到的映射是出站流量的副产品，几十秒无流量即过期，等对端来连时早已失效。

**准确的外部端口只有显式映射能给**。STUN 的价值收敛为两条：给出公网 IP（用于 1:1 NAT 场景的宣告，以及展示），
和作为映射结果的独立佐证（第六节：两路信号并列展示、互不否决）。

### 判定映射是否有效，不看 IP 段

上游做了 DMZ/端口转发时，网关返回私网外部地址而映射对外有效；反之 CGNAT 下网关报成功却不可达。
（更麻烦的是网关返回的地址未必是它 WAN 口的地址——开了行为探测的实现会返回自己 STUN 探到的公网地址，
两种语义混在同一个字段里。）所以映射结果与 STUN 结果并列展示、互不否决；映射建立成功后主动触发一次探测刷新。

## 五、宿主这一层：容器网络与本机防火墙

- **bridge 网络下全部失效**：SSDP 多播出不了网桥、默认网关是 docker0、PCP 请求的源地址是容器地址。
  自动映射只在 `network_mode: host`（或裸机）下可用，README 与 aio 段落必须写明。
- 失败时**不静默**：日志一句「未发现支持 UPnP/PCP 的网关（容器 bridge 网络下无法发现，需 network_mode: host）」，
  管理后台同文案。
- **不做 `PORTMAP_CLIENT` 之类的逃生阀**：曾设想用 UPnP 的 `NewInternalClient` 指向宿主地址，让 bridge 容器也能映射。
  但主流实现（miniupnpd）默认开 `secure_mode`，该模式只允许客户端为**自己的源地址**建映射，指定别的内部客户端会被拒；
  IGDv2 更是把这条列为强制要求。PCP/NAT-PMP 同样只能给报文源地址建映射（THIRD_PARTY option 少有设备支持）。
  bridge 下没有可靠出路，文档直说要 host 网络。

### 本机防火墙是第二道门（Windows 尤甚）

三层角色里「宿主系统」这一层在 Linux 上通常是空的（云主机的安全组在更外面，桌面发行版一般不开 firewalld），
但 **Windows 默认阻止一切入站**：映射成功 + 本机防火墙拦截 = 完全不通，而症状与「映射失败」一模一样。
它的行为还不一致——交互式桌面首次监听时弹窗询问，**作为服务运行时不弹、直接静默拒绝**。

规划中的 Windows 单文件部署据此要求：

- 诊断项预留 `host_firewall`；
- 提供 `firewall-allow` 子命令（沿用既有 `healthcheck` / `adduser` 子命令风格），管理员执行一次添加放行规则，
  进程本身不常驻管理员权限；实现放到 Windows 支持那一期（当下无环境可验证，盲写无益）。

本期就要付的跨平台成本只有两处：网关发现的三个平台文件，以及优雅退出要写成跨平台
（Windows 无 SIGTERM，用 `os.Interrupt`；将来作为服务运行时还要响应 SCM 的停止事件）。

## 六、多层 NAT：上游一层与路由器的自我判定

「上游设备持公网地址（PPPoE 拨号）+ 二级路由做家庭网络」的拓扑在国内很常见，本方案必须正面处理。

### 单层申请 + 上游一次静态配置

hearth 只向自己的默认网关（二级路由）申请，上游那层由用户一次性配置：
指定端口转发（把媒体端口转给二级路由的 WAN 地址，更安全）或 DMZ（未匹配的入站全兜给它）。
**DMZ 是端口不变透传**，所以此时外部端口必须等于内部端口——这是「优先请求同端口」成为硬策略的原因之一。

### 级联留到 P1 之后实现

P1 只向默认网关申请一层，上游那层由用户按诊断提示一次性配置（上面这段）。级联要做，已定的结论：

- **上游看到的请求者就是内层路由**：PCP 的 client IP 字段、UPnP 的 `NewInternalClient` 都填内层路由的
  WAN 地址即可——那本来就是报文到达上游时的源地址，既过得了 PCP 的 `ADDRESS_MISMATCH` 校验，
  也过得了 miniupnpd `secure_mode`「只能给自己开洞」这条（第五节）。不需要 THIRD_PARTY option。
- **发现上游**：先按内层 WAN 网段的 `.1` 启发式猜一跳，再对该地址做**单播 SSDP**（M-SEARCH 直接发给它，
  绕开多播出不了本级路由的问题）与 PCP 单播。无特权 traceroute 兜底可以后置，不是先决条件。
- **深度上限 3**，任一跳拿到的外部地址已是公网即停；某一跳失败就退回单跳诊断（即今天的行为），纯增量。
- **实现形态是链式 client**：每跳一个 client，逐跳把「上一跳给出的外部端口」作为下一跳的内部端口申请，
  `Mapper` 只看最终的外部地址与端口，不感知跳数。

### 曾经的顾虑与解法

- **发现不了上游网关**：SSDP 多播出不了本级路由、PCP 只发默认网关——解法是上面的单播 SSDP/PCP + 网段启发，
  TTL 探测（收 ICMP 要额外权限，容器内不保证可用）因此只是兜底，不是必需。
- **失败点加倍**：两层各自续租，任一层失效整条链即断。解法是失败即退回单跳诊断，
  级联只是「成了更好」的增量，不改变 P1 的可用性下限。
- **上游设备的实现质量参差**，且常带远程管理、配置可能被上游重置——所以级联不能替代诊断指引：
  识别出上游还有一层时，仍然明确告诉用户「请在上游设备把 <端口> 转发到 <本机网关的 WAN 地址>，或开启 DMZ」，
  一次配好的静态转发比一条可能在任意一层悄悄失效的自动链条更可靠。
- 端口链对齐从来不是障碍（先申请内层拿到中间端口，再用它去申请外层）。

### 路由器可能自己否决自己（假阴性）

主流实现（miniupnpd 的 `ext_perform_stun`）会做一次 RFC 5780 的**过滤行为**探测，
若探测不到「陌生源可入」就判定上游不支持端口转发，并**主动禁用整个端口转发功能**，
此后所有 UPnP/NAT-PMP 请求一律失败。

这个判定在上游已做 DMZ/端口转发时是**误判**：探测测的是「已有出站会话的端口能否接收陌生源」，
而 DMZ 处理的是「没有映射表匹配的入站流量往哪转」，两者互不相干——真实的媒体端口在上游没有出站会话，
入站包走的正是后一条路径。（该实现自己提供了 `ext_perform_stun=allow-filtered` 应对，
但上层配置界面未必透传这个值；关掉探测并允许私网 WAN 地址是更容易的出路。）

对本方案的要求：

- **失败不能只报「映射失败」**，要分类到可操作的提示（`Status.Diagnosis`，日志与管理后台同文案）：

| 诊断 | 触发 | 提示 |
| --- | --- | --- |
| `no_gateway` | 发现不到任何网关 | 容器 bridge 网络无法发现，需 host 网络；或网关未开 UPnP/PCP |
| `disabled_by_gateway` | PCP `NOT_AUTHORIZED` / UPnP `606` | 网关因 NAT 行为探测禁用了端口转发；上游若已做 DMZ/转发，关掉探测或改用允许被过滤的模式 |
| `upstream_nat` | 映射成功但外部地址是私网 | 上游还有一层，请在上游设备把该端口转发到本机网关的 WAN 地址，或开 DMZ |
| `port_conflict` | UPnP `718` | 外部端口被占，换一个媒体端口 |
| `host_firewall` | 映射成立但本机防火墙可能拦截（Windows 预留） | 以管理员身份执行 `hearth firewall-allow` |
| `ok` | 映射成立 | 展示外部地址与剩余租约 |
- **不相信任何「声称支持」的信号**：设备描述里宣告的能力、管理界面上勾选的开关，都可能与实际生效的配置脱节
  （实测遇到过：管理界面提供了「强制转发」开关，而生成配置的脚本根本没有这个字段，勾选完全空转）。
  只有两样可信：实际请求的返回结果，以及真正的可达性。
- **路由器返回私网外部地址不算失败**（第四节已述）：关掉行为探测后，这恰恰是最常见的正常状态——
  映射真实有效，只是网关不知道自己的公网地址。公网地址以我们自己的 STUN 探测为准。

## 七、配置

只加一个全局键（进程内网络基建，与 `bellows_udp_port` 同类，不进实例 params）：

- `portmap_mode` / `PORTMAP_MODE`：`auto` | `off`。
- 租约时长、重试退避、发现超时写死常量，不开配置项。

**默认 `auto`**（已定）。部署 hearth 的目的本就是让人从外面连进来；只映射服务自己监听的端口，不是任意端口；
且管理后台明示已开的洞并可一键关闭。

## 八、可见性（小白真正需要的那一半）

映射本身只解决一半问题，另一半是「我现在到底能不能被连上、地址是什么」：

- 管理后台加「网络」区：发现结果（方法 UPnP / PCP / NAT-PMP / 无）、每条映射（协议、内外端口、外部地址、剩余租约）、
  STUN 探到的公网地址、以及**「分享给朋友的地址」建议值**（外部地址 + HTTP 外部端口）。
- 诊断回显走管理接口 `/api/admin/network`，**不进 `/healthz`**：探活、周期刷新、诊断回显是三件事，
  `/healthz` 只保留探活（匿名、恒 200、无副作用），映射失败不得让它变成非 200（autoheal 会误杀），
  内网拓扑也不该从匿名端点漏出去。
- 顺手解决邀请链接：`PUBLIC_URL` 为空时按请求推导，局域网里打开后台生成的邀请链接是内网地址、发出去没用。
  管理后台给一个「用检测到的公网地址填入」按钮（写 `cfg_public_url`）。属 P2。
- **「一键检测外部可达性」待定**：本机自测受 NAT 回环影响不可信，真正的外部探测点得靠第三方。
  但既然映射结果两个方向都可能骗人（第六节的假阴性，以及 CGNAT 下的假阳性），「到底通没通」就只剩实测能回答。
  hearth 部署在公网服务器上时它自己就是探测点（远端 bellows 的场景）；纯家庭部署没有天然探测点，方案待定。
  在此之前如实并列展示映射与 STUN 两路信号，不替用户下结论。

## 九、验收

- host 网络 + 支持 UPnP 或 PCP 的路由器：启动日志打出方法与三条映射，路由器管理页可见；
  外网浏览器可开 `http://<外部地址>:<外部端口>`；外网机器入 ember 语音 ICE connected。
- 路由器重启后一轮续租内映射自愈。
- SIGTERM 后路由器上映射消失。
- 公网直连服务器（无网关）：一句「未发现网关」，其余行为与今天完全一致，**启动无额外延迟**。
- bridge 容器：给出指向 `network_mode: host` 的明确日志，不报错刷屏。
- `cd server && go build ./... && go vet ./...` 通过；PCP/NAT-PMP 报文编解码有离线单测（构造响应字节喂进去，
  与 `lite` 的探测注入同一测法）。

## 十、工作量

PCP + NAT-PMP 自研约 200 行，UPnP 接线约 120 行，续租循环与快照约 150 行，SDP 候选追加约 60 行，
`cmd/server` 优雅退出约 30 行，healthz 与管理后台回显约 150 行（含前端），文档若干。
合计约 600 行 + 2 个小依赖。

## 十一、分期

- **P1**（核心价值）：`portmap` 包 + PCP/NAT-PMP + UPnP + 网关发现 + 宣告收归 lite（`AppendMappedCandidate`）+
  `cmd/server` 与 **`cmd/bellows` 两处接线** + 诊断分类与日志 + README/部署文档的 host 网络说明；
  删除 `plan-bellows-upnp.md`。远端 Bellows 跑在别人的局域网里、最需要打洞，是本方案的核心场景之一，不后置。
- **P2**：远端 Bellows 状态接口（见第三节）+ healthz 与管理后台「网络」区；PCP 的 v6 pinhole；`PUBLIC_URL` 一键填入。

## 十二、IPv6：并列候选，不是替代

- **定位**：ICE 天然多候选，v6 与映射后的 v4 并列宣告，对端各取可达者——只有双方都有 v6 时 v6 才会被选中。
  所以 v6 不降低 v4 映射的优先级，CGNAT 线路上它也只救得回「双方都有 v6」的那部分人，
  其余只能走公网服务器上的 hearth 转发，本方案不假装能解决。
  （旧的 `plan-bellows-upnp.md` 把 IPv6 写成 CGNAT 的替代路线，这个假设不成立，P3 删除那份文档时一并作废。）
- **现状（已核实）**：`lite.Transport` 监听 `:port`，socket 的 LocalAddr 是 `[::]`，
  pion 的 `UDPMuxDefault.GetListenAddresses` 据此按 UDP4+UDP6 枚举全部接口地址——网卡有 v6 GUA 时
  **今天就已经在宣告 v6 host candidate**。但消费级路由器默认拒绝入站 v6，这条候选常常是「宣告了却不通」，
  有 v6 的对端要先试它、失败、再回落 v4，白付一轮连通性检查。
- **`Announcer` 对 v6 实质无操作（已核实）**：pion 的改写规则按地址族分开（catch-all 的族由 external 地址决定），
  填 `ember_public_ip=<v4 地址>` 不会波及 v6 候选；STUN 侧从 v6 地址探测要么族不匹配直接失败、
  要么探到的就是自己（v6 无 NAT，`ext == local` 被 `announceRules` 跳过）。两条路径都不产生规则。
  所以 v6 候选是原样宣告的，问题**只**在路由器防火墙不放行入站。
- **v6 的安全模型与 v4 不同**：主流实现的端口映射 ACL（限定哪些内网地址、哪些端口可被映射）**只作用于 v4**，
  v6 一律放行——把关的只剩「只能给自己开洞」这一条。文档里提醒开启 v6 pinhole 前先了解这个差异，
  不要假定 v4 侧收紧的 ACL 同样护着 v6。
- **要做的**：PCP 的 MAP 对 v6 就是防火墙放行（pinhole）语义，同一个 `Mapper`、同一份 `Want` 即可覆盖，
  作用是让**已经在宣告的**那条 v6 候选真的可用。归入 P2。
- UPnP 侧的对等能力在 IGDv2 的 `WANIPv6FirewallControl:1`（`AddPinhole`/`DeletePinhole`）。
  主流实现编译进了它，但常被两道开关关着：设备以 IGDv1 身份宣告（兼容老客户端的默认设定）、以及 v6 功能被单独禁用。
  所以 v6 pinhole 两条路都试，PCP 优先（不依赖设备宣告哪个版本），发现不到就算了。

