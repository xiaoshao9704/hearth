#Requires -Version 7.0
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not $IsWindows) { throw '此脚本仅用于 Windows 打包。' }
$target = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../src-tauri/target'))
$runtime = Join-Path $target 'windows-gstreamer'
if (-not (Test-Path (Join-Path $runtime 'hearth-gstreamer-source.json'))) {
    throw '请先运行 setup-gst-windows.ps1，生成固定版本的完整运行时快照。'
}

# app-local CRT：主程序、插件扫描器与插件不能依赖远端预装 VC++ Redistributable。
# 只取 VS 官方 Redist 目录，禁止从 System32 或调试工具链复制 DLL。
# https://learn.microsoft.com/en-us/cpp/windows/redistributing-visual-cpp-files
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
$vs = & $vswhere -latest -version '[17.0,18.0)' -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if ($LASTEXITCODE -ne 0 -or -not $vs) { throw '找不到 Visual Studio 2022 C++ 工具链。' }
$crt = Get-ChildItem (Join-Path $vs 'VC/Redist/MSVC/*/x64/Microsoft.VC143.CRT') -Directory |
    Sort-Object {
        $info = (Get-Item (Join-Path $_.FullName 'vcruntime140.dll')).VersionInfo
        [version]::new($info.FileMajorPart, $info.FileMinorPart, $info.FileBuildPart, $info.FilePrivatePart)
    } -Descending | Select-Object -First 1
if (-not $crt) { throw '找不到 Microsoft.VC143.CRT 的 x64 可再分发目录。' }
Copy-Item (Join-Path $crt.FullName '*.dll') (Join-Path $runtime 'bin') -Force

# 硬编元素会按 GPU/驱动动态注册，CI 无 GPU 时不能要求编码器 factory 存在；
# 但承载它们的插件 DLL 必须随完整运行时发出。
$required = @(
    'bin/gstreamer-1.0-0.dll', 'bin/gst-inspect-1.0.exe', 'bin/gst-launch-1.0.exe',
    'bin/vcruntime140.dll', 'bin/msvcp140.dll',
    'libexec/gstreamer-1.0/gst-plugin-scanner.exe',
    'lib/gstreamer-1.0/gstd3d11.dll', 'lib/gstreamer-1.0/gstwasapi2.dll',
    'lib/gstreamer-1.0/gstmediafoundation.dll', 'lib/gstreamer-1.0/gstnvcodec.dll',
    'lib/gstreamer-1.0/gstqsv.dll', 'lib/gstreamer-1.0/gstamfcodec.dll',
    'lib/gstreamer-1.0/gstvideoconvertscale.dll', 'lib/gstreamer-1.0/gstapp.dll',
    'share/licenses'
)
foreach ($relative in $required) {
    if (-not (Test-Path (Join-Path $runtime $relative))) { throw "运行时缺少必需资源：$relative" }
}
# NSIS 按源路径去重，同一 bin DLL 映射到两个目标时只会保留一份。
# 必须物理复制到独立目录，不能再次映射 runtime/bin 或使用目录链接。
$rootDlls = Join-Path $target 'windows-root-dlls'
if (Test-Path $rootDlls) { Remove-Item $rootDlls -Recurse -Force }
New-Item -ItemType Directory -Force $rootDlls | Out-Null
Copy-Item (Join-Path $runtime 'bin/*.dll') $rootDlls -Force
Copy-Item (Join-Path $PSScriptRoot 'README-windows.md') $runtime -Force
$logs = Join-Path $target 'windows-logs'
New-Item -ItemType Directory -Force $logs | Out-Null
Get-ChildItem $runtime -File -Recurse | Sort-Object FullName | ForEach-Object {
    [ordered]@{
        path = [IO.Path]::GetRelativePath($runtime, $_.FullName).Replace('\', '/')
        size = $_.Length
        sha256 = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    }
} | ConvertTo-Json -Depth 3 | Set-Content (Join-Path $logs 'runtime-manifest.json') -Encoding utf8
Write-Host "完整运行时已准备：$runtime；主 exe 同目录 DLL 的独立资源源目录：$rootDlls。"
