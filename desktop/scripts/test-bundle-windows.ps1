#Requires -Version 7.0
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not $IsWindows) { throw '安装包启动检查必须在 Windows 上运行。' }
$target = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../src-tauri/target'))
$logs = Join-Path $target 'windows-logs'
New-Item -ItemType Directory -Force $logs | Out-Null
$install = Join-Path ([IO.Path]::GetTempPath()) ('Hearth Windows smoke ' + [guid]::NewGuid().ToString('N'))
$installers = @(Get-ChildItem (Join-Path $target 'x86_64-pc-windows-msvc/release/bundle/nsis/*-setup.exe'))
if ($installers.Count -ne 1) { throw "应有且仅有一个 NSIS 安装器，实际找到 $($installers.Count) 个。" }
$saved = @{}
$variables = @(Get-ChildItem Env: | Where-Object { $_.Name -match '^(GST|GSTREAMER|PKG_CONFIG|HEARTH_DESKTOP_)' })
foreach ($entry in $variables) { $saved[$entry.Name] = $entry.Value }
$saved['PATH'] = $env:PATH
$process = $null
$transcript = Join-Path $logs 'installed-smoke.log'
Start-Transcript -Path $transcript -Force | Out-Null

try {
    # /D 必须位于最后且不能加引号；NSIS 将其后的整段作为含空格的路径。
    $setup = Start-Process $installers[0].FullName -ArgumentList "/S /D=$install" -PassThru -Wait
    if ($setup.ExitCode -ne 0) { throw "NSIS 静默安装失败：exit=$($setup.ExitCode)" }
    $exe = Join-Path $install 'hearth-desktop.exe'
    if (-not (Test-Path $exe)) { throw "安装后找不到主程序：$exe" }

    # 校验真实安装结果，而不仅是 build 前的 staging 目录。
    $manifest = Get-Content (Join-Path $logs 'runtime-manifest.json') -Raw | ConvertFrom-Json
    foreach ($file in $manifest) {
        $installed = Join-Path $install "gstreamer/$($file.path)"
        if (-not (Test-Path $installed) -or (Get-FileHash $installed -Algorithm SHA256).Hash -ne $file.sha256) {
            throw "安装后的运行时文件缺失或不一致：$($file.path)"
        }
        if ($file.path -match '^bin/[^/]+\.dll$') {
            $besideExe = Join-Path $install ([IO.Path]::GetFileName($file.path))
            if (-not (Test-Path $besideExe) -or (Get-FileHash $besideExe -Algorithm SHA256).Hash -ne $file.sha256) {
                throw "主 exe 旁缺少启动 DLL 或版本不一致：$($file.path)"
            }
        }
    }

    # 不继承构建机 SDK、全局 GStreamer PATH、插件路径或注册表缓存。
    foreach ($entry in $variables) { [Environment]::SetEnvironmentVariable($entry.Name, $null, 'Process') }
    $env:PATH = "$env:SystemRoot\System32;$env:SystemRoot;$env:SystemRoot\System32\Wbem"
    $runtime = Join-Path $install 'gstreamer'
    $env:GST_PLUGIN_SYSTEM_PATH_1_0 = Join-Path $runtime 'lib/gstreamer-1.0'
    $env:GST_PLUGIN_PATH_1_0 = ''
    $env:GST_PLUGIN_SCANNER_1_0 = Join-Path $runtime 'libexec/gstreamer-1.0/gst-plugin-scanner.exe'
    $env:GST_REGISTRY_1_0 = Join-Path $logs 'gst-registry.bin'
    $env:PATH = "$(Join-Path $runtime 'bin');$env:PATH"
    $inspect = Join-Path $runtime 'bin/gst-inspect-1.0.exe'
    foreach ($element in @('d3d11screencapturesrc', 'wasapi2src', 'appsrc', 'appsink',
        'videoconvert', 'videoscale', 'videorate', 'jpegenc', 'h264parse', 'h265parse',
        'opusenc', 'whipclientsink')) {
        & $inspect $element *> (Join-Path $logs "gst-$element.log")
        if ($LASTEXITCODE -ne 0) { throw "打包运行时无法加载元素：$element" }
    }
    & (Join-Path $runtime 'bin/gst-launch-1.0.exe') -q videotestsrc num-buffers=5 '!' videoconvert '!' videoscale '!' 'video/x-raw,width=320,height=180' '!' fakesink *> (Join-Path $logs 'gst-scale-smoke.log')
    if ($LASTEXITCODE -ne 0) { throw '打包运行时的视频转换/缩放管线检查失败。' }

    # GUI 从纯系统 PATH 启动，不替 Rust 初始化补插件路径；验证应用自己的定位契约。
    Get-ChildItem Env: | Where-Object { $_.Name -match '^(GST|GSTREAMER)' } |
        ForEach-Object { [Environment]::SetEnvironmentVariable($_.Name, $null, 'Process') }
    $env:PATH = "$env:SystemRoot\System32;$env:SystemRoot;$env:SystemRoot\System32\Wbem"
    $process = Start-Process $exe -WorkingDirectory $env:TEMP -PassThru `
        -RedirectStandardOutput (Join-Path $logs 'hearth-stdout.log') `
        -RedirectStandardError (Join-Path $logs 'hearth-stderr.log')
    if ($process.WaitForExit(20000)) {
        throw "GUI 提前退出，启动检查失败：exit=$($process.ExitCode)，请查看 hearth-stderr.log。"
    }
    $process.Refresh()
    $modules = @($process.Modules | ForEach-Object { $_.FileName })
    $modules | Set-Content (Join-Path $logs 'loaded-modules.txt') -Encoding utf8
    $gst = @($modules | Where-Object { [IO.Path]::GetFileName($_) -eq 'gstreamer-1.0-0.dll' })
    if ($gst.Count -ne 1 -or -not $gst[0].StartsWith("$install\", [StringComparison]::OrdinalIgnoreCase)) {
        throw 'GUI 未加载安装目录内的 GStreamer DLL。'
    }
    if ($process.MainWindowHandle -eq 0) {
        throw 'GUI 进程仍存活但没有主窗口；不能判为启动通过，请在交互式 Windows 会话复核。'
    }
    if (Select-String -Path (Join-Path $logs 'hearth-stderr.log') -Pattern 'GStreamer 初始化失败|Failed to load plugin' -Quiet) {
        throw 'GUI 日志报告 GStreamer 初始化或插件加载失败。'
    }
    Write-Host "GUI 启动通过，窗口：$($process.MainWindowTitle)。此结果不代表 GPU 原生采集验收通过。"
} finally {
    if ($process -and -not $process.HasExited) { Stop-Process -Id $process.Id -Force }
    Get-ChildItem Env: | Where-Object { $_.Name -match '^(GST|GSTREAMER|PKG_CONFIG|HEARTH_DESKTOP_)' } |
        ForEach-Object { [Environment]::SetEnvironmentVariable($_.Name, $null, 'Process') }
    foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') }
    Stop-Transcript | Out-Null
    # 安装目录保留在临时目录内供失败诊断；不要触碰测试用户已有的 Hearth 安装。
}
