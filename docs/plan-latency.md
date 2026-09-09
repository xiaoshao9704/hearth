# 计划：延迟与连接质量（读数面板、参与者连接质量、端到端延迟标尺）

状态：**已实施（2026-09-08；2026-09-09 复核）。** 证据：`web/src/latency/measure.ts`、`web/src/main.ts`（`#/latency` 路由）。本文自包含；与代码不一致处见文末「实施记录」。

## 背景

现在房间里能看到的只有视频卡片上的码率 / 帧率 / 丢包，诊断上报只在建连结果时发一次。"晚上卡"到底是谁的网、卡在哪一段，没有数据；OBS 推流端在浏览器之外，更看不见。目标是三档能力，全部在浏览器内完成：

1. **连接质量读数**：每条线（语音 / 舞台）到服务器的往返、抖动、丢包、走 UDP 还是 TCP 兜底；每路视频轨的抖动缓冲与解码耗时（收）、编码耗时与质量受限原因（发）。房间里可看，并定期进诊断上报。
2. **参与者连接质量**：LiveKit 按每个参与者的 RTCP 回报算了连接质量（excellent / good / poor / lost），客户端能收到全员的更新。名册与推流角标显示它——这是"OBS 那头上行差"唯一便宜的来源。
3. **端到端延迟标尺**：一个全屏页把服务器同步过的时间画成色码；推流方用 OBS 抓它或浏览器共享该标签页；任何观众在那路视频上点"测延迟"，页面从视频帧读像素解出时间、与自己校准过的时钟相减，得到采集 → 编码 → 服务器 → 解码 → 渲染的全程延迟。时钟都对齐到服务器，两端不必同机。

## 已核实的事实

- `web/src/engine/types.ts` `VideoStats {width,height,fps,kbps,loss?}`；`engine/livekit.ts` 已有每个 transport（pub/sub）的 `getStats` 快照（约 37、269 行 `captureTransportStats`，目前只服务诊断）与 sender stats（约 349 行）；卡片统计由房间页 2 秒轮询（`room.tsx` 约 506 行）。
- `EPart` 在 `engine/livekit.ts` 约 84 行组装；livekit-client 有 `RoomEvent.ConnectionQualityChanged(quality, participant)`，客户端会收到所有参与者的更新，**不需要服务端改动**。
- 诊断上报 `room.tsx` 约 146 行 `diag(level, event, line, fields)` → `api.ts` `/api/client-log`（需登录；服务端 `clientlog.go` 有按用户限频与脱敏；`detail` 字段上限 2000 字符）。
- 路由在 `web/src/main.ts`（hash：`#/login`、`#/join/`、`#/lobby`、`#/room/`、`#/manage/`、`#/admin`），轻页面用 vanilla TS。服务端没有时间接口。
- 推流角标 `room/ingest-badge.tsx` 只显示实测数据；顶栏有 `conn-chip`（已连接 / 重连中）。

## 设计

### 一、连接质量读数（前端为主）
- `engine/types.ts` 加：
  ```ts
  export interface LineStats { rtt_ms?: number; jitter_ms?: number; loss_pct?: number; transport?: 'udp'|'tcp'|'relay'|'unknown'; local?: string; remote?: string; at: number }
  export interface VideoStats { …既有…; jitter_buffer_ms?: number; decode_ms?: number; frames_dropped?: number; encode_ms?: number; limitation?: 'none'|'cpu'|'bandwidth'|'other' }
  ```
  `AVEngine` 加 `lineStats(): LineStats | null`（从已有的 transport 快照派生：`candidate-pair` 里 `nominated` 的 `currentRoundTripTime`、`remote-inbound-rtp` 的 `jitter`/`fractionLost`、候选对的 `protocol`/`candidateType`）。每条线各一个引擎实例，房间页按线取。
- 视频轨统计扩展：收侧 `inbound-rtp` 的 `jitterBufferDelay/jitterBufferEmittedCount`、`totalDecodeTime/framesDecoded`、`framesDropped`；发侧 `outbound-rtp` 的 `totalEncodeTime/framesEncoded`、`qualityLimitationReason`。
- 房间 UI：顶栏 `conn-chip` 点开一个小面板（新文件 `room/conn-panel.tsx`），两行（语音线 / 舞台线）各显示 RTT · 抖动 · 丢包 · 传输方式，5 秒刷新；卡片统计行追加「缓冲 xx ms · 解码 xx ms」（发侧「编码 xx ms · 受限：带宽」）。
- 诊断：进房后每 60 秒一次 `diag('info', 'line_stats', line, { detail: JSON 压缩串 })`，含两条线的 `LineStats` 与本地发布轨的 `VideoStats` 摘要；页面隐藏时不发；断线期间不发。`detail` ≤ 2000 字符。

### 二、参与者连接质量（前端）
- `EPart` 加 `quality?: 'excellent'|'good'|'poor'|'lost'`；引擎监听 `ConnectionQualityChanged` 更新快照并触发参与者变更回调（沿用现有的参与者更新通路，不加第二条）。
- 名册行：`poor`/`lost` 时名字旁一个小图标（`signalLow`，`ui.ts` 加 icon）带 title「连接质量差」；推流角标 `ingest-badge.tsx` 的弹出行追加「上行：差」。

### 三、端到端延迟标尺
**服务端**：`GET /api/time` → `{now_ms}`，无需登录，无副作用（放在 `/healthz` 旁）。

**时钟校准** `web/src/latency/clock.ts`：连发 5 次 `GET /api/time`，取往返最小的一次，`offset = now_ms + rtt/2 - Date.now()`；导出 `syncedNow()` 与 `syncQuality()`（用的那次 rtt）。

**编码** `web/src/latency/code.ts`（纯函数，可单测）：把 32 位毫秒时间戳（`syncedNow() mod 2^32`）编成一行 **40 块**：前 4 块同步标记（黑白黑白）、32 块数据（黑 = 0，白 = 1，高位在前）、4 块校验（数据按 8 位异或）。提供 `encode(ts): boolean[]` 与 `decode(samples: number[]): number | null`（`samples` 是 40 个亮度值 0–255，先按同步块自适应阈值，再校验，失败返回 null）。

**标尺页** `#/latency`（`web/src/views/latency.ts`，vanilla，不需登录）：全屏，页面顶部 1/3 高度画色码一行（canvas，块宽 = 宽度 / 40），下方大字显示可读的同步时间与「时钟校准 ±xx ms」；`requestAnimationFrame` 每帧更新。页面说明：「用 OBS 抓这个窗口，或在浏览器投屏里共享这个标签页；观众在那路画面上点『测延迟』」。

**解码** `web/src/latency/measure.ts`：给定 `<video>`，每 150 ms `drawImage` 到 40×3 的离屏 canvas（把整帧压成一行 40 块：按块中心区域取平均亮度，行取画面高度 1/6 处），`decode` 得到时间戳 → `delay = syncedNow() - ts`（处理 32 位回绕）。跑 10 秒，去掉 null，输出中位数、p95、样本数。

**入口**：视频卡片（浏览器投屏与 OBS 推流都适用）的「更多操作」菜单加「测延迟」；结果在卡片上方一个小面板显示「端到端 480 ms（p95 610，31 样本，时钟 ±12 ms）」，解不出色码时提示「画面里没有标尺：让推流方打开 #/latency」。

## 改动清单
| 位置 | 改动 |
|---|---|
| `server/internal/api/api.go` | `GET /api/time` |
| `web/src/engine/types.ts`、`engine/livekit.ts` | `LineStats`、`VideoStats` 扩展、`lineStats()`、连接质量事件 |
| `web/src/views/room/conn-panel.tsx`（新）、`room/ingest-badge.tsx`、`room.tsx`（接线：面板开关信号、卡片统计行、名册图标、菜单项、60 秒 diag） | 一、二档 UI |
| `web/src/latency/{clock,code,measure}.ts`（新）、`web/src/views/latency.ts`（新）、`main.ts` 路由 | 三档 |
| `web/src/ui.ts`、`style.css` 末尾 | icon、样式 |
| `README.md` | 「延迟怎么测」一小节 |

## 验收
1. Go 全套 + tsc/build；`code.ts` 的编码/解码单测（往返、单块噪声下校验失败返回 null、亮度阈值自适应）——web 没有测试框架就用 `node --test` 跑一个 `.mjs`，或在 `package.json` 加 vitest，二选一报告写明。
2. 本地烟测：`GET /api/time` 无登录 200；`#/latency` 页渲染；**同页自测**：在标尺页里把自己的 canvas 当视频源喂给 `measure`，延迟应接近 0（±一帧）；两个用户进房，一个用浏览器投屏共享标尺标签页，另一个点「测延迟」得到数百毫秒量级的结果并贴图；连接面板两条线有 RTT/传输方式；名册在正常连接下无图标。
3. 诊断：服务端日志出现 `event="line_stats"` 且 `detail` 为压缩 JSON，60 秒一条，页面隐藏时停。

## 不做
- 不做语音的端到端延迟（没有外部回路）；不做服务端侧的推流 RTT 采集；不做历史曲线与持久化。

## 实施记录（与本文设计的差异）

- `lineStats()` 是**同步读快照**：引擎连上后把原有的 ICE 探测定时器转成 5 秒慢速刷新（同一个定时器、同一条 `captureTransportStats` 采集路径），没有第二路 `getStats`。断开即停。
- 线路读数的抖动/丢包：收侧优先取 `inbound-rtp`（自己实际收到的，丢包按区间差分），没有收侧数据才退回服务端回报的 `remote-inbound-rtp` 的 `jitter`/`fractionLost`；RTT 取选中候选对的 `currentRoundTripTime`，缺失才退回 `remote-inbound-rtp.roundTripTime`。
- 校验 4 块的算法：32 位数据按 8 位分 4 组全部异或得 1 字节，再把高低两个半字节异或折成 4 位（本文写的「按 8 位异或」得到的是 8 位，折半是等价的 4 位方案）。任一块翻转必然改变其中一位，单块噪声一定被抓到（有单测）。
- 解码取样用 `200×3` 的离屏 canvas（每块横向 5 px、只用中间 3 px = 中心 60%），不是 `40×3`——`40×3` 没有「块内中心」可取，块边界的缩放混色会污染判读。
- 标尺页**不看 `document.hidden`**：`requestAnimationFrame` 本身在页面不渲染时就不回调（这就是「隐藏时停」）；而标签页被采集时 `hidden` 也是 `true`，此时主动停画会让观众读到一个冻住的时间戳，比不停更糟。
- 视频卡片原先没有「更多操作」菜单，本次新加（只对远端画面出现——本机预览没经过服务器，测不出全程）；菜单里目前只有「测延迟」一项。结果面板贴在卡片顶部居中。
- 60 秒诊断按线各发一条（`role=voice`/`stage`，合并形态只发一条 `voice`），`detail` 是 `{line, video}` 的压缩 JSON；`video` 只在承担舞台角色的那条线上有值。
- 名册的连接质量图标挂在**设备粒度**的行（单设备用户行与多设备用户的设备子行），聚合的用户行不挂——质量是每台设备各自的。
