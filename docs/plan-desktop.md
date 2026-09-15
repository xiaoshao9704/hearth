# 方案：Windows / macOS 桌面端

状态：**方向已定、关键假设已验证（2026-09-15），里程碑 1（macOS 纵切）代码已落地，待真机人工验收。**
对应 [roadmap](roadmap.md) 的提前技术验证节点，不指定发布版本号。
下文「已验证」指本仓库 2026-09-15 的实测或官方文档核实，「未验证」项不得当作承诺。

## 定案

| 决定 | 内容 |
| --- | --- |
| 壳 | Rust + Tauri v2。`desktop/` 目录进主仓库，`frontendDist` 指向 `../web/dist`，不复制一份前端。 |
| 原生投屏发布 | 走**现有 WHIP 入口** `/providers/{alias}/w/{channel}`：设备票（`POST /api/channels/{channel}/cast-ticket`，10 分钟、绑频道与设备，标签 `cast-{device_id}`）+ `admitIngest`，`kind=ingest`。与 OBS 的账号级推流令牌互不干扰。不用 LiveKit Rust SDK。 |
| 媒体管线 | GStreamer（gstreamer-rs）。编码与 WHIP 全用现成元素：`vtenc_*` / `mfh26*enc` / `nvd3d11h26*enc` / `qsvh26*enc` / `amfh26*enc` + `whipclientsink`，`congestion-control=disabled` 固定码率，比照 OBS。 |
| 采集 | Windows：`d3d11screencapturesrc capture-api=wgc`（整屏或 `window-handle`）+ `wasapi2src loopback-mode=include-process-tree loopback-target-pid=…`。macOS：**自写 ScreenCaptureKit 采集**（画面 + 所属应用音频）喂 `appsrc`；GStreamer 在 macOS 没有可用的屏幕与系统声采集。 |
| 证书信任 | 应用内信任，不装系统 CA。WebView 与 Rust 各自的信任回调由桌面壳自己挂（wry 不暴露）。 |
| 观看 / 语音 / 聊天 | 复用 WebView 里的现有网页与 LiveKit JS 引擎；原生侧只做投屏发布。 |
| 本机服务 | 首版不打包 hearth。「在本机运行服务器」只调用已安装 hearth 的 `service` CLI，找不到就引导下载。 |
| 首版目标 | 指定程序音频、外网稳定投屏、免系统 CA 的自签连接、全局热键按键说话（并入 roadmap 节点 5 的储备项）。硬编 CPU 降幅不作准入线。 |
| 仓库与分支 | PoC 实验代码不进仓库。桌面端目录与网页桥能力锁步提交。 |

为什么不走 LiveKit Rust SDK：它与浏览器共用同一份 libwebrtc 的带宽估计与降级策略，用户实测「浏览器投屏外网卡、OBS 不卡」的差异正来自发布端行为；WHIP + 固定码率硬编是 OBS 的路径，且身份、展示、管制在推流入口上已全部现成。

## PoC 证据（2026-09-15）

| 项 | 结果 | 说明 |
| --- | --- | --- |
| `whipclientsink` → hearth lkembed WHIP | **通过** | H.264 与 H.265 均建流，LiveKit 侧确认 `video/H265`；`lkembed_tcp_port` 开着（answer 含 TCP 候选）正常。 |
| 编码器约束 | 已验证 | `vtenc_*` 必须显式 `max-keyframe-interval`，否则会话建立后永远等关键帧；webrtcsink 拥塞控制不能驱动 `vtenc_*` 码率，固定码率正合需要，自适应要自写胶水。观众中途加入的按需 IDR（PLI 联动）未验证。 |
| WHIP 轨道属性 | 已验证 | 进房轨道 `source` 固定为 CAMERA，前端按 `kind=ingest` 识别推流设备，不受影响。 |
| macOS GStreamer 采集 | **不可用** | 上游无 ScreenCaptureKit 元素；`avfvideosrc capture-screen` 走已弃用的整屏 API，本机零帧（权限与 API 弃用两个因素未分离）；`osxaudiosrc` 无系统声回环。 |
| GStreamer 分发 | 已验证 | Homebrew `gstreamer` 的 nice 插件是悬空链接，需 `libnice-gstreamer`；分发包必须自带。1.24 起官方 Windows/macOS 二进制含 gst-plugins-rs，Windows ARM64 除外。无官方最小裁剪配方，完整包约几十 MB。 |
| WKWebView 证书回调 | **通过** | 页面 JS 的 `fetch` 与 `wss` 都走 `WKNavigationDelegate` 的 `didReceive challenge`；每条新 TLS 连接问一次，放行不缓存；`protectionSpace` 的 host/port 可靠，可精确限定目标。自签叶证书需 `CA:FALSE` + `serverAuth` EKU，hearth `selfca` 已满足。 |
| Tauri 接线 | **通过** | `with_webview` 取到 WKWebView，读出原 delegate，挂一个只实现挑战方法、`respondsToSelector:` 与 `forwardingTargetForSelector:` 全部转发的代理对象。实测：wry 的 `decidePolicy`/`didFinish`/`didCommit`/`didBecomeDownload` 经代理仍答 YES，挑战方法只有代理答 YES，未知选择器答 NO；换上代理后 IPC 与页面加载照常。已配对的 host:port 走锚定评估放行，未配对（同一张证书换个 host 访问）走 performDefaultHandling，页面加载失败。 |
| `whipclientsink` 的 CA 入口 | **没有** | signaller 只有 `whip-endpoint` / `auth-token` / `timeout` / `use-link-headers` / `manual-sdp-munging`，自签服务器的锚交不进去。因此 Rust 侧 https 推流走「只绑回环、随机端口、随机 secret 路径、只转发到当前服务器」的反代，由我们这一跳带锚出网（`desktop/src-tauri/src/whipproxy.rs`）。 |
| 原生发布失败回报 | **通过** | ICE 失败/断开时 whipclientsink 不往 bus 上 post error。壳侧看门狗盯 webrtcbin 的 `ice-connection-state`/`connection-state`，连续坏满 5 秒即记错、`emit("publish-state")` 推给网页并收管线；另有「建流 15 秒还一帧没编出」的兜底。实测掐掉服务器后 webrtcbin 自己约 16 秒才转 disconnected，再 5 秒判定，端到端约 21 秒。事件订阅需要 `core:event:allow-listen` 能力（`capabilities/default.json`，只给本地 main 窗口），缺它 `plugin:event|listen` 会被 ACL 拒；事件已实测送达网页的监听回调，房间页据此复位按钮的那一步未在真实房间里点过。 |
| WebView2 证书回调 | 文档核实 | `ServerCertificateErrorDetected` 覆盖全部 web resource，非导航请求走 DEFAULT 即拒绝；`ALWAYS_ALLOW` 按 host+证书在同 session 缓存，需 `ClearServerCertificateErrorActions` 撤销。wry 不暴露，需 webview2-com 直接挂。WebSocket 是否触发未实测。 |
| Windows GStreamer 采集与硬编 | 文档核实 | `d3d11screencapturesrc` 1.22 起支持 WGC 与 `window-handle`；`wasapi2src` 1.22 起支持进程树回环，要求 Windows 10 build 20348+；`mfh26*enc` / `nvd3d11h26*enc` / `qsvh26*enc` / `amfh26*enc` 直接吃 `D3D11Memory`。本机无 Windows，未实测。 |
| ffmpeg whip muxer | 不采用 | answer 超过 8192 字节即失败，多地址机器必现；只收 H.264。 |

## 架构约束

- **同一网页渐进增强**。WebView 加载本地打包的 `web` 产物，浏览器用同一份源码；不另写桌面路由、频道页或设置系统。网页通过桥能力检测得知原生能力，无桥时行为不变，可替代则降级，无替代则隐藏并说明。
- **能力不等于权限**。显隐继续按服务端返回的 `role`/`my_role`，推流权由 `admitIngest` 最终判定。
- **身份与展示**。原生发布的 identity 由服务端按设备票组装（`u{uid}-cast-{device_id}`），展示按 `Meta.uid` 聚合，管制走 `MatchesUser`。客户端不签 JWT、不持有 LiveKit 密钥。断线重推回到 WHIP 入口重新判定。
- **单一职责的媒体组件**。麦克风与远端播放只由 WebView 负责；原生侧只发布投屏画面与所属应用音频，不订阅、不播放，也不把远端声音再发布。原生采集中断先停止并提示，不静默切到浏览器采集或扩大音频范围。
- **音频范围如实表达**。Windows 说明覆盖进程树；macOS 说明「共享此窗口画面与所属应用声音」，单窗口独立声音不承诺。拒绝指定音频时不得改成全系统声音。
- **采集在登录用户的客户端进程内**。Windows SCM 服务与 macOS LaunchAgent 只运行 hearth 服务器；客户端退出、注销、锁屏时采集停止。
- **网页对远端服务器的地址不再同源**。`SERVER_URL` 已可配置；桥要把用户选定的服务器地址交给网页，CSP、登录 token 存储、下载路径按本地打包形态适配。客户端与远端服务器会出现版本错位，API 兼容从此是约束。
- **房间状态仍是 Solid 信号**。原生发布状态经桥接入同一房间状态，不建定时同步的布尔副本。控制命令与低频统计走 IPC；帧与音频不经 JS。
- **IPC 最小权限**。只开放列采集源、开始/停止发布、连接指定服务器、固定的服务操作与统计；参数严格校验；只有本地打包的窗口可调用，远程页面、iframe、外链不可。

## 应用内信任

- 系统已信任的有效 HTTPS 直接连接，不额外配对。
- 系统未信任的自签/私有 CA：根指纹从邀请或管理员另一渠道取得，与 `/ca.crt` 下载的根比对一致后，作为**仅限该服务器配置**的信任锚；随后正常校验链、SAN、有效期。不全局放行，不豁免过期、主机不匹配、链不合法。
- 回调常驻：WKWebView 每条新 TLS 连接都会询问，LiveKit wss 重连与每次 WHIP 请求都要重新答；WebView2 的放行缓存在删除信任时必须清除。
- 跨网络仅 HTTP 时不传会话、邀请码或媒体票，提示配置 TLS。
- 叶证书续期不需重新配对；根更换、名称约束变化才重新配对。
- 首版邀请只接受一个服务器地址加可选根指纹；多候选地址探测不做。

## 里程碑

### M1：macOS 纵切（代码已落地，待人工验收）

已实现（2026-09-15，`desktop/` 与 `web/src/bridge/`）：脚手架、运行时服务器地址与配对页、桥能力检测、房间页原生投屏入口、SCK 采集 → `appsrc` → `vtenc_h26x` → `whipclientsink`、应用内信任（WKWebView 转发式 delegate + Rust 侧回环反代带锚）、发布失败回报。测试源模式下 WHIP 全链路（含 https 端点）实测建流。

待人工验收（无人值守环境拿不到 TCC 与本地网络授权，代码只经编译与走查）：

- SCK 真实出帧与所属应用音频：GUI 下授予屏幕录制权限后，浏览器观众看到画面并听到应用声音。
- 同机/同网 ICE：未签名 app 发往局域网地址的 UDP 被 macOS 本地网络隐私门丢弃（信令成功、ICE 失败）。需签名后由用户在弹窗授权；连远端公网服务器不受影响。
- 禁言/踢出切断原生发布、停止共享与退出回收轨道在真实房间里的走查。
- 已知限制：桌面 origin 是 `tauri://localhost`，部署侧把 `CORS_ORIGIN` 收紧会打死桌面端；失败判定端到端约 21 秒，瓶颈在 webrtcbin 的断连检测。

原定内容：

- `desktop/` Tauri 脚手架，加载 `../web/dist`，可指定服务器地址登录进房。
- `web/src/bridge/`：桥能力检测；房间页在有原生投屏能力时把「投屏」入口切到原生流程，无桥时原样。
- Rust：`list_sources`（SCK 的显示器与窗口）、`start_publish`、`stop_publish`、`publish_stats`；管线 SCK → `appsrc` → `vtenc_h265`（固定码率、显式关键帧间隔）→ `whipclientsink`；应用音频 → `appsrc` → `opusenc`。
- 应用内信任：`with_webview` 挂转发式 navigation delegate；Rust 侧 WHIP 请求加同一份信任锚。
- 完成标准：浏览器观众看到桌面端投屏与所属应用声音；在只有自签证书的 hearth 上登录、进房、语音、投屏全程不装系统 CA；禁言与踢出能切断原生发布；停止共享与退出应用回收轨道。

### M2：Windows

- 需要 Windows 机器实测。采集与回环改用 GStreamer 现成元素，编码器按 GPU 择一；WebView2 证书事件用 webview2-com 挂；确认 WebSocket 是否触发该事件。
- 完成标准与 M1 相同，另加：进程树音频范围在多进程应用上正确。

### M3：热键、本机服务、发行

- 全局热键按键说话（含游戏全屏）；「在本机运行服务器」调用已安装 hearth 的 `service` CLI 并显示状态。
- Windows 签名、WebView2 运行时、GStreamer 运行时打包；macOS 签名、公证、屏幕录制与麦克风权限归属用真实签名的包验证。
- 两端从干净系统完成安装、自签直连、邀请进房、原生投屏、升级与卸载后再定版本。

## 验收总则

验收独立复核实现 diff、原始测量与负向测试记录，不以完成声明代替证据。阶段状态只在文档、代码与实测一致时更新；未通过项写明限制，不标为已实施。
