#Requires -Version 7.0
[CmdletBinding()]
param(
    [string]$InstallRoot = (Join-Path ([IO.Path]::GetTempPath()) 'hearth-gstreamer-1.28.7-msvc-x86_64')
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not $IsWindows -or -not [Environment]::Is64BitProcess) {
    throw '请使用 Windows x64 PowerShell 7 运行此脚本。'
}

# 1.28 起官方将运行时与开发文件合为 Inno Setup 安装器，不能沿用 MSI 属性。
# https://gstreamer.freedesktop.org/download/
$version = '1.28.7'
$sha256 = '032fc6062b8539838fc8da22589cb9b24c5d820baa7f8cc160af9ea08395badf'
$filename = "gstreamer-1.0-msvc-x86_64-$version.exe"
$url = "https://gstreamer.freedesktop.org/data/pkg/windows/$version/msvc/$filename"
$target = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../src-tauri/target'))
$logs = Join-Path $target 'windows-logs'
$runtime = Join-Path $target 'windows-gstreamer'
$installer = Join-Path $logs $filename
New-Item -ItemType Directory -Force $logs | Out-Null

Invoke-WebRequest -Uri $url -OutFile $installer
if ((Get-FileHash $installer -Algorithm SHA256).Hash.ToLowerInvariant() -ne $sha256) {
    throw 'GStreamer 安装器 SHA-256 不匹配。'
}

function Install-GStreamer([string]$Type) {
    $log = Join-Path $logs "gstreamer-$Type.log"
    # 禁用可选注册表/全局环境任务；构建只使用当前进程与 GitHub step 环境。
    $arguments = @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/CURRENTUSER', '/portable=1', '/TASKS=""',
        "/TYPE=$Type", "/DIR=`"$InstallRoot`"", "/LOG=`"$log`"")
    $process = Start-Process -FilePath $installer -ArgumentList $arguments -PassThru -Wait
    if ($process.ExitCode -ne 0) {
        throw "GStreamer $Type 安装失败：exit=$($process.ExitCode)，日志：$log"
    }
}

# 先快照纯运行时，再原地加装头文件/import libraries；安装包不会混入 SDK。
Install-GStreamer 'runtime'
if (Test-Path $runtime) { Remove-Item -Recurse -Force $runtime }
New-Item -ItemType Directory -Force $runtime | Out-Null
Copy-Item -Path (Join-Path $InstallRoot '*') -Destination $runtime -Recurse -Force
# Inno 卸载器只属于构建机的 SDK，不能随应用再分发或误导用户卸载 SDK。
Get-ChildItem $runtime -Filter 'unins*' -File | Remove-Item -Force
Install-GStreamer 'devel'

# Cerbero 将各组件的 share/licenses 归入 devel 文件列表；仅复制 runtime 会漏版权材料。
# https://github.com/GStreamer/cerbero/blob/1.28.7/cerbero/build/filesprovider.py
$licenses = Join-Path $InstallRoot 'share/licenses'
if (-not (Test-Path $licenses) -or @(Get-ChildItem $licenses -Recurse -File).Count -eq 0) {
    throw '官方开发安装中缺少许可证材料。'
}
New-Item -ItemType Directory -Force (Join-Path $runtime 'share') | Out-Null
Copy-Item $licenses (Join-Path $runtime 'share') -Recurse -Force

$bin = Join-Path $InstallRoot 'bin'
$pkgConfig = Join-Path $bin 'pkg-config.exe'
if (-not (Test-Path $pkgConfig)) { $pkgConfig = Join-Path $bin 'pkgconf.exe' }
foreach ($file in @($pkgConfig, (Join-Path $InstallRoot 'lib/pkgconfig/gstreamer-1.0.pc'))) {
    if (-not (Test-Path $file)) { throw "GStreamer 开发安装缺少文件：$file" }
}
$variables = [ordered]@{
    GSTREAMER_1_0_ROOT_MSVC_X86_64 = $InstallRoot
    PKG_CONFIG = $pkgConfig
    PKG_CONFIG_PATH = (Join-Path $InstallRoot 'lib/pkgconfig')
}
foreach ($entry in $variables.GetEnumerator()) {
    [Environment]::SetEnvironmentVariable($entry.Key, $entry.Value, 'Process')
    if ($env:GITHUB_ENV) { "$($entry.Key)=$($entry.Value)" | Out-File $env:GITHUB_ENV -Append -Encoding utf8 }
}
$env:PATH = "$bin;$env:PATH"
if ($env:GITHUB_PATH) { $bin | Out-File $env:GITHUB_PATH -Append -Encoding utf8 }
& $pkgConfig --modversion gstreamer-1.0 gstreamer-app-1.0 gstreamer-video-1.0 gstreamer-audio-1.0 gstreamer-webrtc-1.0
if ($LASTEXITCODE -ne 0) { throw 'GStreamer pkg-config 检查失败。' }
@{ version = $version; url = $url; sha256 = $sha256 } | ConvertTo-Json |
    Set-Content (Join-Path $runtime 'hearth-gstreamer-source.json') -Encoding utf8
# 构建日志只保留文本，不重复上传半 GB 的安装器。
Remove-Item $installer
Write-Host "GStreamer SDK：$InstallRoot；运行时快照：$runtime"
