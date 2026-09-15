# Windows x64 桌面测试包

状态：Windows x64 测试包构建流程已配置；Windows 编译、安装启动、原生采集和 GPU 验收仍待执行。

## 包内内容与限制

- NSIS 当前用户安装包，复用 `web/dist`。GStreamer 固定 **1.28.7 / MSVC x86_64**，安装器 SHA-256 固定在 `setup-gst-windows.ps1`，不取 latest，不用 MinGW。
- GStreamer 运行时按实际用量裁剪（`bundle-gst-windows.ps1`，做法比照 macOS 侧的 `bundle-gst-macos.sh`）：官方完整 runtime 有 270 多个插件、150 多个 bin DLL，包内只留 **25 个插件**——WGC（`d3d11`）、WASAPI2、Media Foundation / NVENC / QSV / AMF 四类硬编、转换缩放、预览 JPEG、appsrc/appsink、Opus 与音频转换重采样、H.264/H.265 解析器、WHIP 一族（`rswebrtc`/`webrtc`/`rtp`/`rtpmanager`/`srtp`/`dtls`/`nice`/`debugutilsbad`）、`coreelements`/`typefindfunctions`/测试源。清单来源是各元素官方文档页的 Plugin 字段，脚本再用运行时自带的 `gst-inspect-1.0` 逐个元素复核「元素属于清单里的插件」，写错即构建失败。
- 插件之外的 DLL 由 `dumpbin /dependents`（Visual Studio 2022 工具链）从插件、`gst-plugin-scanner.exe`、命令行诊断工具和主程序链接的 GStreamer 库出发递归求闭包，只保留 `bin` 里真正被引用的；系统 DLL 不在运行时目录里，自然排除。`bin/*.exe` 只留 `gst-inspect-1.0`/`gst-launch-1.0`（安装后检查要用），`include`、`lib/*.lib`、`lib/pkgconfig`、`share/locale`、`etc` 等一律删除。
- `share` 只保留 `licenses`：Cerbero 将 `share/licenses` 归入 devel，脚本在开发安装完成后额外复制这个完整目录，携带各组件的 license/copyright 材料，**裁剪不得动它**。源码与版权信息以随包的官方材料为准。
- 硬件编码器是否注册取决于实际 GPU 和驱动：`nvcodec`/`qsv`/`amfcodec` 的 `plugin_init` 在没有对应硬件的机器上直接失败，插件本身都不会注册。CI 只保证这些插件 DLL 随包发出、并记录构建机上实际注册了哪些元素（诊断 artifact 的 `hardware-encoders.json`），真实硬编路径不能由 CI runner 代替验证。
- GStreamer 无需用户另装。MSVC CRT 从 Visual Studio 2022 官方 Redist 目录复制到包内；Windows 10/11 的系统组件和 GPU 驱动仍由系统提供。
- WebView2 使用 Tauri 的 `offlineInstaller` 模式；构建时下载并嵌入 Microsoft Evergreen 离线安装器，安装时缺少 WebView2 会自动安装。这部分由上游滚动更新，未宣称整个包逐字节可复现。
- 测试包未配置代码签名，Windows 可能提示未知发布者。未生成 release、tag 或自动更新元数据。
- Windows 原生采集与网页控制的支持范围以集成代码与实测结果为准。**Windows WebView2 的应用内私有 CA 信任尚未实现**；Rust 侧配对成功不能证明 WebView2 的 fetch/WSS 已信任。测试使用系统已信任且证书有效的 HTTPS。

## 构建和运行

GitHub Actions 首次试打：维护者审阅并集成桌面端与网页改动后，将提交推送到 `codex/windows-desktop`。工作流只接收 `codex/**` 下相关路径的 push，以及 `workflow_dispatch`；不监听 tag，不创建 release，不需要写权限或签名 secrets。

工作流进入默认分支后，可以手动选择集成分支运行：

```sh
gh workflow run desktop-windows.yml --ref codex/windows-desktop
gh run list --workflow desktop-windows.yml --branch codex/windows-desktop
gh run watch <run-id> --exit-status
gh run download <run-id> --pattern 'hearth-windows-x64-*'
```

在 Actions 页面下载 `hearth-windows-x64-<commit>` artifact，解压并运行其中的 `*-setup.exe`。诊断 artifact 单独保存安装日志、插件检查、文件清单与 GUI 日志，保留 14 天。若 GUI 检查失败，job 失败且只上传诊断，不把该包当作已通过的测试包。

本地构建要求 Windows x64、PowerShell 7、Node.js 22.14.0、Rust 1.95.0 MSVC、Visual Studio 2022 的 C++ 桌面工具链（含 Windows SDK）。从仓库根目录，在同一个 PowerShell 进程执行：

```powershell
# 必须在当前进程调用，保证 PKG_CONFIG / PATH 传给后续 cargo。
& ./desktop/scripts/setup-gst-windows.ps1
npm --prefix web ci
if ($LASTEXITCODE -ne 0) { throw 'web npm ci 失败' }
npm --prefix web run build
if ($LASTEXITCODE -ne 0) { throw 'web build 失败' }
npm --prefix desktop ci
if ($LASTEXITCODE -ne 0) { throw 'desktop npm ci 失败' }
npm --prefix desktop run build:windows
if ($LASTEXITCODE -ne 0) { throw 'desktop build 失败' }
```

`npm run setup:windows` 会在子进程中安装并创建快照，环境变量不能传回调用它的 shell；本地连续构建请用上面的直接 PowerShell 调用。Actions 通过 `GITHUB_ENV` / `GITHUB_PATH` 传到后续步骤，应用安装不会修改用户或系统的全局 PATH。

输出：`desktop/src-tauri/target/x86_64-pc-windows-msvc/release/bundle/nsis/*-setup.exe`。`tauri.windows.conf.json` 在 Windows 目标下由 Tauri 自动合并；macOS 不加载此配置。

安装检查脚本会静默安装到临时目录并注册 Hearth 的卸载/协议入口，因此只在专用测试账户或 CI runner 执行，不能用于已有 Hearth 安装的日常账户：

```powershell
npm --prefix desktop run test:windows
```

## 运行时布局与 Rust 接口约定

```text
安装目录/
  hearth-desktop.exe
  *.dll                         # runtime/bin 中所有 DLL（含 app-local CRT）
  gstreamer/
    bin/                        # 依赖闭包内的 DLL + gst-inspect/gst-launch
    lib/gstreamer-1.0/          # 白名单插件
    libexec/gstreamer-1.0/gst-plugin-scanner.exe
    share/licenses/             # 官方许可证/版权文件
```

资源 map 将 `target/windows-gstreamer/` 递归映射为 `gstreamer/`；脚本将其中 `bin/*.dll` 物理复制到独立的 `target/windows-root-dlls/`，再映射到 exe 目录，避免 NSIS 按相同源路径去重。后者保证 Windows 在进入 Rust `main` 之前就能解析静态 DLL 依赖，不能仅靠 `gst::init` 前修改 PATH 代替。

Rust 实现负责在第一次 `gst::init()` 前用 `current_exe()` 定位：

- `GST_PLUGIN_SYSTEM_PATH_1_0` → `exe_dir/gstreamer/lib/gstreamer-1.0`。
- `GST_PLUGIN_SCANNER_1_0` → `exe_dir/gstreamer/libexec/gstreamer-1.0/gst-plugin-scanner.exe`。
- 当前进程 PATH 前置 `exe_dir/gstreamer/bin`，使插件扫描器及插件依赖从包内加载；不改用户或系统环境。
- 注册表缓存写用户可写缓存目录；开发态不存在包内 runtime 时允许使用 SDK 环境。

## 本轮验收标准

CI 检查：

1. Windows MSVC x64 的 `cargo check --locked --all-targets` 和 NSIS 构建成功，前端类型检查与构建通过。
2. 固定下载哈希匹配；裁剪后 WGC/WASAPI2/各硬编插件、转换缩放与预览依赖、官方许可证目录齐全，元素与插件的对应经 `gst-inspect` 复核。
3. 安装后的完整运行时逐文件 SHA-256 与打包清单一致，主 exe 旁 DLL 与原版一致。
4. 清掉构建机 GStreamer 环境与插件缓存后，使用包内 `gst-inspect` 逐个检查所需元素（含 WHIP 内部一族），跑转换/缩放与预览 JPEG 两条管线；硬编元素在构建机上注册时再跑一条 Media Foundation 编码管线。
5. GUI 从纯系统 PATH、无 GST 环境启动，保持至少 20 秒、有主窗口、实际加载的 GStreamer DLL 位于安装目录。秒退、无窗口、初始化错误均失败并保留日志。runner 自带系统组件，故这仍不能代替干净 Windows 验收。

真实 Windows 设备验收，另记系统/GPU/驱动、操作与结果：

1. **未预装 GStreamer** 的系统完成安装、启动和卸载；从开始菜单启动不依赖终端或全局 PATH。
2. WGC **窗口与显示器**均可枚举、选择并显示对应预览；切换来源后预览正确，关闭源能明确反馈。
3. 按来源支持的 **audio scope** 开关采集，关闭后不发送该音频；窗口的进程树与显示器的声音范围按 UI 说明逐项验证，不扩大范围。
4. 投屏 **分辨率、fps、bitrate** 设置实际生效，结合采集/编码统计与观众侧画面验证，不能只看表单值。
5. 停止共享、切换源、关闭窗口、退出应用后，采集/预览/音频/编码管线和发布轨道清理，无残留会话。
6. 在实际 GPU 上验证可用硬编路径、观看与声音、连续运行及故障反馈。**CI 编译与包体检查不等于 GPU 真实采集验收。**

## 官方依据

- [GStreamer 下载与 Inno Setup 参数](https://gstreamer.freedesktop.org/download/)：`/TYPE=runtime`、`/TYPE=devel`、`/DIR`、`/CURRENTUSER`、`/VERYSILENT`、`/NORESTART`。
- [固定版本安装器实现](https://github.com/GStreamer/cerbero/blob/1.28.7/cerbero/packages/windows/inno_setup.py)、[portable 模式实现](https://github.com/GStreamer/cerbero/blob/1.28.7/data/inno/base.iss)、[Inno Setup `/TASKS` 参数](https://jrsoftware.org/ishelp/topic_setupcmdline.htm)：`/portable=1` 与显式清空可选任务，SDK 安装也不修改全局环境或注册系统卸载器。
- [官方许可证文件分类](https://github.com/GStreamer/cerbero/blob/1.28.7/cerbero/build/filesprovider.py)：`add_license_files` 把许可证加入 devel，必须在快照后补齐。
- [固定版本官方 SHA-256](https://gstreamer.freedesktop.org/data/pkg/windows/1.28.7/msvc/gstreamer-1.0-msvc-x86_64-1.28.7.exe.sha256sum)。
- [GStreamer Windows 部署](https://gstreamer.freedesktop.org/documentation/deploying/windows.html)。其中 MSI 示例属于旧系列，本脚本不混用。
- [Tauri 资源 map 与目录保留规则](https://v2.tauri.app/develop/resources/)、[Windows NSIS 与 WebView2](https://v2.tauri.app/distribute/windows-installer/)。
- [Microsoft DLL 搜索顺序](https://learn.microsoft.com/en-us/windows/win32/dlls/dynamic-link-library-search-order)、[VC++ 可再分发文件与 app-local 部署](https://learn.microsoft.com/en-us/cpp/windows/redistributing-visual-cpp-files)。
