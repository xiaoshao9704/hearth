# 路线图（2026-09-09 定稿）

状态：**已确认（2026-09-09，用户拍板）**。节点按依赖顺序排，版本号是建议。每个节点开工前在本文更新状态行；节点内的细节设计另开 `docs/plan-<name>.md`。

## 定位

Hearth 是几个朋友的私人客厅：一个文件跑起来的语音、高清投屏、OBS 直推与聊天。它不打算替代 Discord，只做 Discord 做不好或用不了的那一块：画质、延迟、在自己家里跑、数据在自己手里。目标规模是 5 到 20 人的熟人圈，不是社区平台。

主打场景排序：一起看画面（游戏画面、看片、看代码）第一，语音开黑第二，OBS 私人直播第三。投屏是差异化的锚，语音是基础能力而非卖点。

## 节点 0：文档收尾（v0.9.21，纯文档）

状态：已发布 v0.10.0（2026-09-09）。

- README / README.en / 宣传页首屏加命名段落与定位语。
- 状态过期的 plan 文档（passkey、notify、latency、entrypoints、ingest-entry、chat-files、site-readme）改成「已实施」。
- README 首屏加 30 秒 GIF：起服务、开频道、投屏、OBS 推流。
- 新增「家里跑」三档路线文档：有公网 IPv4 / 只有 IPv6 / CGNAT 走另一台公网机器跑 `stage` + 隧道。

完成标准：三份文档定位表述一致；GIF 只从本地干净实例录制（示例用户 `alice`/`bob`、示例域名 `hearth.example.com`，地址栏与设备标签不入图）。

## 节点 1：家庭自托管 TLS（v0.10.0）

状态：已发布 v0.10.0（2026-09-09）。设计见 `docs/plan-tls.md`。

- `tls_cert_source` 四档 `off / self / file / upload`，默认 `self`；`HTTPS_ADDR` 决定合并模式（同端口双协议，默认）还是分开模式（明文与 TLS 各一个端口）。
- `self`：本地根 CA 落 `<data>/tls/`，带名称约束防滥用；叶证书 SAN 随本机与公网 IP 变化重签，根 CA 不动；无鉴权的 `/ca.crt` 与分系统安装说明页 `/ca`（iOS 需在「证书信任设置」里手动开完全信任）。
- `file` / `upload`：外部工具（acme.sh / lego / certbot / `tailscale cert` 等）签出的证书指路径，续期后自动热换；或后台直接上传证书与私钥。
- 顺带：ICE-TCP（`lkembed_tcp_port`）默认开并纳入端口映射；对外地址与映射诊断在管理后台 `GET /api/admin/tls` 可见。
- 没域名时通行密钥不可用（WebAuthn 要求 RP ID 是域名），密码登录兜底，文档写明。

完成标准：无域名的新机器上，手机装根 CA 后语音、投屏、推送、PWA 全通；有域名的机器 `file`/`upload` 档证书生效并跟随文件变化热换；`off` 档行为与现状一致。

## 节点 2：国内发布轮（不改代码，与节点 1 交错）

状态：待开始。

- V2EX、Linux.do、B 站演示视频；切入点「Discord 用不了，一个文件跑私人语音投屏间」。
- 应用商店模板：CasaOS、1Panel、飞牛、Unraid Community Apps。
- awesome-selfhosted 提 PR。
- 观察 2 到 4 周：star、镜像拉取、陌生人 issue。这三个数决定后续节点的投入力度。

## 节点 3：i18n（v0.11.0，独占版本）

状态：待开始。前置：所有并行功能分支合完，期间不开新功能分支。

- 前端 key 字典 + `t()` + 外观设置里的语言切换，不引第三方库。
- 服务端错误改返回稳定 `code`，前端按 code 映射文案；中文 message 保留兜底。
- dyncfg 的 Label/Hint 挪到前端字典，服务端只留 key。
- CLI 输出与日志不翻。
- CLAUDE.md 加「新文案必须双语」铁律；加扫残留中文字面量的门禁脚本。

完成标准：`tsc` 通过，门禁零残留，英文界面走完注册、进房、投屏、OBS、管理后台全流程。

## 节点 4：英文发布轮

状态：待开始。前置：节点 3。

- Show HN + 技术文章（进程内嵌 LiveKit、HEVC 直通反代）。
- r/selfhosted；OBS 论坛与 r/obs（自托管 WHIP 目标的空位）。

## 节点 5：按反馈排序的储备项

- TURN over TLS 443（`docs/plan-client-ice.md` 第二阶段），需要公网机器，家庭场景配合另一台机器上的 `stage` 一起讲。
- 全局热键小托盘程序，解决游戏全屏时按键说话失效。
- 浏览器投屏 HEVC（`docs/plan-hevc-clarity.md` 评审）。
- 技术债：`is_admin` 列、`warnLegacyConfig`、server-sdk-go 回正式版、Windows 与 OBS HEVC 真机验收。

## 贯穿始终

频道寻址逢改必靠 id，不单独立项（见 `docs/plan-entrypoints.md` 与 CLAUDE.md）。
