# 计划：宣传页 + README 重写（中英双语结构）

状态：**已实施（原定稿 2026-09-08，2026-09-09 核对）。** 证据：`site/index.html`、`README.en.md`、`.github/workflows/pages.yml`。本文自包含。纯文档与静态页，不改产品代码。

## 背景

`README.md` 是随功能一路追加出来的（282 行、13 个小节，架构细节与部署步骤混排），首次访问的人看不到"这是什么、给谁用、三步跑起来"。仓库要面向仓库外的人展示，需要一个静态宣传页与一份结构化的 README，并留好多语言位（先英语）。**界面本身目前只有中文**，英文文档必须明说这一点。

## 隐私铁律（本任务尤其要守）

截图、示例、文案里**不得出现**维护者的真实域名、IP、用户名、设备型号、家宽/机房等信息；示例域名一律 `hearth.example.com`，示例用户名用 `alice`/`bob`/`carol`。截图只能来自本地干净实例（见验收）。

## 交付物

### 1. `README.md`（中文，主入口）
顶部第一行：`[English](README.en.md)`。结构固定为：

1. 一句话定位 + 三句话说清"自建的语音 / 投屏 / OBS 推流 / 聊天房间，单二进制，零外部依赖"。
2. 截图一张（大厅或房间）。
3. **功能一览**（分组小标题，每条一行）：语音（SFU、降噪、回声消除、按键说话、AFK）；投屏与推流（浏览器投屏含系统声音、OBS WHIP 直推、HEVC/AV1 直通、剧场/全屏/画中画）；聊天（实时、@、回复、表情、图片与文件直传不落盘、静音）；通知（后台通知、离线推送 @ 与回复、角标）；账号（通行密钥、会话管理、访客邀请与可选转正、角色阶梯）；部署（单二进制三系统、docker、自动端口映射、舞台线可拆到另一台机器）；诊断（连接质量、端到端延迟标尺）。
4. **三分钟跑起来**：单二进制（下载 → 运行 → 打开）、docker compose（贴一段可直接用的 compose，端口 8080 与 47700 tcp/udp）、反代要点（`Host` 与 `X-Forwarded-Proto`、WebSocket）。
5. **部署形态**：一台机器（默认）；舞台线拆到上行更好的机器（`stage` 容器）；官方 LiveKit 兼容。各一段，不超过 10 行。
6. **配置**：管理后台可改的键表（从 `server/internal/api/dyncfg.go` 逐条核对，列 12 个键：键名、默认、一句话）；环境变量表（从 `cmd/server` 与 `api` 里实际读取的 env 核对）。
7. **常见问题**：连不上语音（STUN/ICE-TCP/安全组 47700）、国内 STUN、iPhone 通知要装成 PWA、通行密钥换域名失效、OBS 填法。
8. 架构一段（双线内核、`lkembed`、admission 单点、id 寻址）链接到 `CLAUDE.md` 与 `docs/`。
9. 开发（构建、测试、发布 tag 触发 CI）、里程碑（保留现有）、License。

现有 README 里的技术细节（端口映射、远端舞台、通行密钥、通知、延迟）**压缩进上面的结构**，不丢事实但去掉过程叙述；核对每条命令与键名与当前代码一致（`git grep` 验证，不凭印象）。

### 2. `README.en.md`
与中文同结构、同截图，地道英文（不是逐句直译）。功能一览后加一行 note："The UI is currently Chinese-only; an English UI is planned."

### 3. `site/index.html`（宣传页）
- 纯静态单文件 + `site/assets/`（截图 PNG，每张 ≤ 400 KB，用 `sips`/`pngquant` 压）。**零外部依赖**（不引任何 CDN 字体/脚本，国内可访问性优先），内联 CSS/JS。
- 双语：`<html lang>` 随 `?lang=en`/`zh` 或 `localStorage.hearth_site_lang`，页面所有文案用 `data-i18n` 键从内联字典取，右上角切换按钮；默认按 `navigator.language` 判 zh/en。
- 主题跟随系统明暗（沿用产品的 ember 配色变量，`prefers-color-scheme`）。
- 内容顺序：hero（名字、一句话、两个按钮：GitHub / 三分钟部署锚点）→ 三张截图横排（大厅 / 房间投屏 / 手机聊天）→ "为什么自建"三点（数据在自己机器、零依赖单二进制、上行有限也能投屏）→ 功能网格（六到八格）→ 三步部署（docker compose 片段可复制）→ 页脚（License、仓库链接）。
- 响应式：375px 单列，无横向滚动。
- 加 `.github/workflows/pages.yml`：push 到 main 且 `site/**` 变化时发布 `site/` 到 GitHub Pages（`actions/upload-pages-artifact` + `deploy-pages`）。仓库是否开启 Pages 由用户在设置里做，README 里写一句。

### 4. 截图
从本地干净实例截（`hearth-smoke`、`reg_policy=open`、两个用户 `alice`/`bob`、频道 `lounge`）：
- 大厅（桌面，1280×800）；
- 房间（桌面，有一路投屏画面——用 canvas 造一路画面顶掉 `getDisplayMedia`，见此前 agent 的做法，聊天面板开着有三四条消息、一个 @）；
- 房间（375×812 手机视口，聊天优先布局）。
截图里不能出现真实域名（地址栏不入图）。放 `site/assets/`，README 引用同一份（相对路径 `site/assets/…`）。

## 改动清单
| 位置 | 改动 |
|---|---|
| `README.md`、`README.en.md` | 重写 / 新增 |
| `site/index.html`、`site/assets/*.png` | 新增 |
| `.github/workflows/pages.yml` | 新增 |
| `CLAUDE.md` | 「隐私铁律」段补一句：截图与宣传页同受约束 |

## 验收
1. `README.md` 与 `README.en.md` 小节一一对应；文中每个命令、端口、键名、路径经 `git grep` 核对（报告列出核对表）。
2. `site/index.html` 用浏览器打开：中英切换、明暗跟随、375px 无横向滚动；所有资源为相对路径、无外链请求（DevTools 网络面板零外部域名）。
3. 三张截图无任何真实标识；文件大小达标。
4. `pages.yml` 语法有效（`actionlint` 有则跑，无则人工核对 uses 版本）。
5. 不改产品代码；`cd server && go build ./...`、`cd web && npx tsc --noEmit` 仍通过（防止误碰）。

## 不做
- 不做界面 i18n；不做 docs/ 计划文档翻译；不做 logo 重设计（沿用 `web/public/icon.svg`）。
