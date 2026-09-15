#Requires -Version 7.0
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# 探测硬编元素时 gst-inspect 本来就会以非零退出；退出码由脚本自己判，不转成终止错误。
$PSNativeCommandUseErrorActionPreference = $false
if (-not $IsWindows) { throw '此脚本仅用于 Windows 打包。' }
$target = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../src-tauri/target'))
$runtime = Join-Path $target 'windows-gstreamer'
$logs = Join-Path $target 'windows-logs'
if (-not (Test-Path (Join-Path $runtime 'hearth-gstreamer-source.json'))) {
    throw '请先运行 setup-gst-windows.ps1，生成固定版本的完整运行时快照。'
}
New-Item -ItemType Directory -Force $logs | Out-Null

# 官方完整 runtime 有 270 多个插件、150 多个 bin DLL，整包搬运没有意义：
# 只留 Rust 侧真正建管线用到的插件，再从这些插件反推 DLL 依赖，其余删掉。
# 做法比照 macOS 侧（bundle-gst-macos.sh）：显式清单 + 依赖闭包 + 自检。
#
# 元素 → 插件的对应取自各元素官方文档页的 Plugin 字段
# （https://gstreamer.freedesktop.org/documentation/，如 d3d11screencapturesrc 属 d3d11 插件），
# 文件名是官方 Windows runtime 的 lib/gstreamer-1.0/gst<插件>.dll；
# 下面的 $expected/$optional 会用运行时自带的 gst-inspect 逐个复核，写错即失败。
$plugins = @(
    'coreelements'      # capsfilter/queue/identity/fakesink：管线里到处是
    'app'               # appsrc/appsink：采集喂管线、预览取帧
    'typefindfunctions' # caps 探测兜底，不在任何 launch 串里但缺它边角情况会炸
    'videotestsrc'      # 测试源模式（HEARTH_DESKTOP_TESTSRC=1）与编码器探测管线
    'audiotestsrc'      # 测试源模式的声音
    'videoconvertscale' # videoconvert/videoscale
    'jpeg'              # jpegenc：源预览那条单帧管线（preview.rs）
    'videoparsersbad'   # h264parse/h265parse
    'audioconvert'      # 采集音频 F32LE → opusenc 要的格式
    'audioresample'
    'opus'              # opusenc
    'd3d11'             # d3d11screencapturesrc：WGC 屏幕/窗口采集
    'wasapi2'           # wasapi2src：系统与进程树 loopback 音频
    'mediafoundation'   # mfh264enc/mfh265enc
    'nvcodec'           # nvh264enc/nvh265enc
    'qsv'               # qsvh264enc/qsvh265enc
    'amfcodec'          # amfh264enc/amfh265enc
    'rswebrtc'          # whipclientsink（gst-plugins-rs）
    'webrtc'            # webrtcbin
    'rtp'               # rtph264pay/rtph265pay/rtpopuspay
    'rtpmanager'        # rtpbin/rtpsession/rtpfunnel
    'srtp'              # 媒体加密
    'dtls'              # DTLS 握手
    'nice'              # ICE
    'debugutilsbad'     # webrtcbin 内部会拉起这里的元素，macOS 侧实测被加载
)
# 没带：videorate（采集侧用 caps 定帧率，没有任何管线插它）、rsrtp（congestion-control=disabled
# 时 webrtcsink 不拉 rtpgccbwe）、sctp（不开数据通道）——与 macOS 侧的取舍一致。

$pluginDir = Join-Path $runtime 'lib/gstreamer-1.0'
$pluginFiles = @()
foreach ($name in $plugins) {
    $file = Join-Path $pluginDir "gst$name.dll"
    if (-not (Test-Path $file)) { throw "完整运行时里没有插件 gst$name.dll，清单与官方包不一致。" }
    $pluginFiles += $file
}

# --- 元素 → 插件复核（在裁剪前，用完整运行时跑） -------------------------------
# 清单写错的代价是运行时缺元素，必须在打包这一步就暴露，而不是等安装后的元素检查。
$env:GST_PLUGIN_SYSTEM_PATH_1_0 = $pluginDir
$env:GST_PLUGIN_PATH_1_0 = ''
$env:GST_PLUGIN_SCANNER_1_0 = Join-Path $runtime 'libexec/gstreamer-1.0/gst-plugin-scanner.exe'
$env:GST_REGISTRY_1_0 = Join-Path $logs 'gst-registry-bundle.bin'
$env:PATH = "$(Join-Path $runtime 'bin');$env:PATH"
$inspect = Join-Path $runtime 'bin/gst-inspect-1.0.exe'

function Get-ElementPluginFile {
    param([string]$Element)
    $output = (& $inspect $Element 2>$null) -join "`n"
    if ($LASTEXITCODE -ne 0) { return $null }
    $found = [regex]::Match($output, '(?m)^\s*Filename\s+(.+?)\s*$')
    if (-not $found.Success) { return $null }
    return [IO.Path]::GetFileName($found.Groups[1].Value)
}

# 必须注册的元素：Rust 侧 launch 串里直接出现的，加上 whipclientsink 内部会拉起的一族。
$expected = [ordered]@{
    'capsfilter'             = 'coreelements'
    'queue'                  = 'coreelements'
    'identity'               = 'coreelements'
    'fakesink'               = 'coreelements'
    'appsrc'                 = 'app'
    'appsink'                = 'app'
    'videotestsrc'           = 'videotestsrc'
    'audiotestsrc'           = 'audiotestsrc'
    'videoconvert'           = 'videoconvertscale'
    'videoscale'             = 'videoconvertscale'
    'jpegenc'                = 'jpeg'
    'h264parse'              = 'videoparsersbad'
    'h265parse'              = 'videoparsersbad'
    'audioconvert'           = 'audioconvert'
    'audioresample'          = 'audioresample'
    'opusenc'                = 'opus'
    'd3d11screencapturesrc'  = 'd3d11'
    'wasapi2src'             = 'wasapi2'
    'whipclientsink'         = 'rswebrtc'
    'webrtcbin'              = 'webrtc'
    'rtph264pay'             = 'rtp'
    'rtph265pay'             = 'rtp'
    'rtpopuspay'             = 'rtp'
    'rtpbin'                 = 'rtpmanager'
    'srtpenc'                = 'srtp'
    'dtlssrtpenc'            = 'dtls'
    'nicesink'               = 'nice'
}
# 硬编候选（encoder.rs）：按 GPU/驱动动态注册，构建机没有对应硬件时不注册。
# 这里只要求「注册了就必须落在白名单插件里」，不要求一定注册。
$optional = [ordered]@{
    'mfh264enc'   = 'mediafoundation'
    'mfh265enc'   = 'mediafoundation'
    'nvh264enc'   = 'nvcodec'
    'nvh265enc'   = 'nvcodec'
    'qsvh264enc'  = 'qsv'
    'qsvh265enc'  = 'qsv'
    'amfh264enc'  = 'amfcodec'
    'amfh265enc'  = 'amfcodec'
}
$whitelist = $plugins | ForEach-Object { "gst$_.dll" }
$resolved = [ordered]@{}
# 判据是「元素的插件在白名单里」——这才是包能不能用的条件；上表写错某个元素归属时
# 只告警并把实际归属记进 plugin-map.json，不拿注释的笔误卡构建。
foreach ($element in $expected.Keys) {
    $file = Get-ElementPluginFile $element
    if (-not $file) { throw "完整运行时里找不到元素 $element，或它的插件不在白名单里。" }
    if ($whitelist -notcontains $file) { throw "元素 $element 属于白名单外的 $file。" }
    if ($file -ne "gst$($expected[$element]).dll") {
        Write-Warning "元素 $element 实际属于 $file，与清单注释的 gst$($expected[$element]).dll 不同。"
    }
    $resolved[$element] = $file
}
foreach ($element in $optional.Keys) {
    $file = Get-ElementPluginFile $element
    if (-not $file) { $resolved[$element] = '（构建机未注册）'; continue }
    if ($whitelist -notcontains $file) { throw "元素 $element 属于白名单外的 $file。" }
    $resolved[$element] = $file
}
$resolved | ConvertTo-Json -Depth 3 | Set-Content (Join-Path $logs 'plugin-map.json') -Encoding utf8

# --- DLL 依赖闭包 ------------------------------------------------------------
# app-local CRT：主程序、插件扫描器与插件不能依赖远端预装 VC++ Redistributable。
# 只取 VS 官方 Redist 目录，禁止从 System32 或调试工具链复制 DLL。
# https://learn.microsoft.com/en-us/cpp/windows/redistributing-visual-cpp-files
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
$vs = & $vswhere -latest -version '[17.0,18.0)' -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if ($LASTEXITCODE -ne 0 -or -not $vs) { throw '找不到 Visual Studio 2022 C++ 工具链。' }

# 依赖用同一套工具链里的 dumpbin 解析：CRT 已经要求 VS 2022，不额外引入依赖。
$dumpbin = Get-ChildItem (Join-Path $vs 'VC/Tools/MSVC/*/bin/Hostx64/x64/dumpbin.exe') -File -ErrorAction SilentlyContinue |
    Sort-Object FullName -Descending | Select-Object -First 1
if ($dumpbin) {
    $dumpbinPath = $dumpbin.FullName
} else {
    $command = Get-Command dumpbin.exe -ErrorAction SilentlyContinue
    if (-not $command) { throw '找不到 dumpbin.exe，无法解析 DLL 依赖。' }
    $dumpbinPath = $command.Source
}

$bin = Join-Path $runtime 'bin'
$binDlls = @{}
foreach ($file in Get-ChildItem $bin -File -Filter '*.dll') { $binDlls[$file.Name.ToLowerInvariant()] = $file.FullName }

function Get-DllReferences {
    param([string]$File)
    $output = (& $dumpbinPath /nologo /dependents $File 2>$null) -join "`n"
    if ($LASTEXITCODE -ne 0) { throw "dumpbin 解析失败：$File" }
    # 普通导入与 delay load 两段都是「一行一个文件名」；只认整行就是 DLL 名的行，
    # 标题行与摘要行因此不会混进来。系统 DLL 不在 bin 目录里，取交集时自然落选。
    $references = [regex]::Matches($output, '(?im)^[ \t]*([A-Za-z0-9_\-\.\+]+\.dll)[ \t]*$')
    return @($references | ForEach-Object { $_.Groups[1].Value.ToLowerInvariant() })
}

# 主 exe 这时还没编出来，用它链接的 GStreamer 库当种子：
# 上一轮完整包的 loaded-modules.txt 显示进程只从包内加载这几个库及其依赖。
$seeds = @(
    'gstreamer-1.0-0.dll', 'gstapp-1.0-0.dll', 'gstvideo-1.0-0.dll',
    'gstaudio-1.0-0.dll', 'gstwebrtc-1.0-0.dll', 'gstsdp-1.0-0.dll'
)
$keepDlls = [System.Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
$pending = [System.Collections.Generic.Queue[string]]::new()
foreach ($seed in $seeds) {
    if (-not $binDlls.ContainsKey($seed)) { throw "运行时 bin 里没有主程序要链接的 $seed。" }
    if ($keepDlls.Add($seed)) { $pending.Enqueue($binDlls[$seed]) }
}
# 插件与命令行工具也是入口：插件的依赖走 PATH 从包内 bin 解析。
$tools = @('gst-inspect-1.0.exe', 'gst-launch-1.0.exe')
$roots = @($pluginFiles) + @($env:GST_PLUGIN_SCANNER_1_0) + ($tools | ForEach-Object { Join-Path $bin $_ })
foreach ($root in $roots) {
    if (-not (Test-Path $root)) { throw "运行时缺少依赖分析入口：$root" }
    $pending.Enqueue($root)
}
while ($pending.Count -gt 0) {
    foreach ($dependency in @(Get-DllReferences $pending.Dequeue())) {
        if ($binDlls.ContainsKey($dependency) -and $keepDlls.Add($dependency)) {
            $pending.Enqueue($binDlls[$dependency])
        }
    }
}

# --- 删除白名单以外的一切 ----------------------------------------------------
# share 只留 licenses：法律要求随包携带各组件的版权材料（见 setup 脚本注释）。
# include、lib/*.lib、lib/pkgconfig、share/locale、etc 与除两个诊断工具外的 bin/*.exe
# 都不参与运行，一并删除。
$keep = [System.Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
[void]$keep.Add('hearth-gstreamer-source.json')
[void]$keep.Add('libexec/gstreamer-1.0/gst-plugin-scanner.exe')
foreach ($name in $keepDlls) { [void]$keep.Add("bin/$name") }
foreach ($name in $tools) { [void]$keep.Add("bin/$name") }
foreach ($name in $plugins) { [void]$keep.Add("lib/gstreamer-1.0/gst$name.dll") }
$removed = 0
foreach ($file in Get-ChildItem $runtime -File -Recurse) {
    $relative = [IO.Path]::GetRelativePath($runtime, $file.FullName).Replace('\', '/')
    if ($relative.StartsWith('share/licenses/', [StringComparison]::OrdinalIgnoreCase)) { continue }
    if ($keep.Contains($relative)) { continue }
    Remove-Item -LiteralPath $file.FullName -Force
    $removed++
}
foreach ($directory in (Get-ChildItem $runtime -Directory -Recurse | Sort-Object { $_.FullName.Length } -Descending)) {
    if (-not (Get-ChildItem $directory.FullName -Force)) { Remove-Item -LiteralPath $directory.FullName -Force }
}

$crt = Get-ChildItem (Join-Path $vs 'VC/Redist/MSVC/*/x64/Microsoft.VC143.CRT') -Directory |
    Sort-Object {
        $info = (Get-Item (Join-Path $_.FullName 'vcruntime140.dll')).VersionInfo
        [version]::new($info.FileMajorPart, $info.FileMinorPart, $info.FileBuildPart, $info.FilePrivatePart)
    } -Descending | Select-Object -First 1
if (-not $crt) { throw '找不到 Microsoft.VC143.CRT 的 x64 可再分发目录。' }
Copy-Item (Join-Path $crt.FullName '*.dll') $bin -Force

# 硬编元素会按 GPU/驱动动态注册，CI 无 GPU 时不能要求编码器 factory 存在；
# 但承载它们的插件 DLL 必须随包发出。
$required = @(
    'bin/gstreamer-1.0-0.dll', 'bin/gst-inspect-1.0.exe', 'bin/gst-launch-1.0.exe',
    'bin/vcruntime140.dll', 'bin/msvcp140.dll',
    'libexec/gstreamer-1.0/gst-plugin-scanner.exe', 'share/licenses'
) + ($plugins | ForEach-Object { "lib/gstreamer-1.0/gst$_.dll" })
foreach ($relative in $required) {
    if (-not (Test-Path (Join-Path $runtime $relative))) { throw "运行时缺少必需资源：$relative" }
}
# NSIS 按源路径去重，同一 bin DLL 映射到两个目标时只会保留一份。
# 必须物理复制到独立目录，不能再次映射 runtime/bin 或使用目录链接。
$rootDlls = Join-Path $target 'windows-root-dlls'
if (Test-Path $rootDlls) { Remove-Item $rootDlls -Recurse -Force }
New-Item -ItemType Directory -Force $rootDlls | Out-Null
Copy-Item (Join-Path $bin '*.dll') $rootDlls -Force
Copy-Item (Join-Path $PSScriptRoot 'README-windows.md') $runtime -Force
Get-ChildItem $runtime -File -Recurse | Sort-Object FullName | ForEach-Object {
    [ordered]@{
        path = [IO.Path]::GetRelativePath($runtime, $_.FullName).Replace('\', '/')
        size = $_.Length
        sha256 = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    }
} | ConvertTo-Json -Depth 3 | Set-Content (Join-Path $logs 'runtime-manifest.json') -Encoding utf8

$files = @(Get-ChildItem $runtime -File -Recurse)
$megabytes = [math]::Round((($files | Measure-Object Length -Sum).Sum / 1MB), 1)
$binCount = @(Get-ChildItem $bin -File -Filter '*.dll').Count
[ordered]@{
    plugins = $plugins.Count
    bin_dlls = $binCount
    removed_files = $removed
    kept_files = $files.Count
    megabytes = $megabytes
} | ConvertTo-Json | Set-Content (Join-Path $logs 'runtime-trim.json') -Encoding utf8
Write-Host "运行时已裁剪：$($plugins.Count) 个插件、$binCount 个 bin DLL（含 app-local CRT），删除 $removed 个文件，剩 $($files.Count) 个共 $megabytes MB。"
Write-Host "运行时：$runtime；主 exe 同目录 DLL 的独立资源源目录：$rootDlls。"
