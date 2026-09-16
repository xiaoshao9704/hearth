#Requires -Version 7.0
# 把 hearth 服务端编成 Tauri 的 externalBin（sidecar），供桌面端「在本机运行服务器」用。
# 与 build-sidecar.sh 同一份取舍，只是 Windows 侧：
#   - 复用同一份服务端代码与 service CLI，不为桌面端另做一套常驻逻辑。
#   - 前端产物必须在 go build 之前拷进 server/internal/webui/dist（见 CLAUDE.md 单二进制一节），
#     否则 sidecar 起来只有 API 没有页面。
#   - 文件名后缀是 Tauri 约定的 host triple，打包时会被去掉，装到机器上就叫 hearth.exe。
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$PSNativeCommandUseErrorActionPreference = $true

$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$hostLine = @(& rustc -vV) | Where-Object { $_ -like 'host: *' } | Select-Object -First 1
if (-not $hostLine) { throw '取不到 rustc host triple' }
$triple = $hostLine.Substring(6).Trim()

$webDist = Join-Path $root 'web/dist'
$embedDist = Join-Path $root 'server/internal/webui/dist'
if (-not (Test-Path $webDist)) { throw "缺少前端产物 $webDist，先 npm --prefix web run build" }

# .keep 是 git 里唯一留下的文件，重建后补回去
if (Test-Path $embedDist) { Remove-Item $embedDist -Recurse -Force }
New-Item -ItemType Directory -Force $embedDist | Out-Null
New-Item -ItemType File -Force (Join-Path $embedDist '.keep') | Out-Null
Copy-Item (Join-Path $webDist '*') $embedDist -Recurse -Force

$out = Join-Path $root "desktop/src-tauri/binaries/hearth-$triple.exe"
New-Item -ItemType Directory -Force (Split-Path $out) | Out-Null
Push-Location (Join-Path $root 'server')
try {
    & go build -o $out ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "go build 失败：exit=$LASTEXITCODE" }
} finally {
    Pop-Location
}

$size = [math]::Round((Get-Item $out).Length / 1MB, 1)
Write-Host "sidecar 已生成：$out（$size MB）"
