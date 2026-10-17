#!/usr/bin/env pwsh
<#
.SYNOPSIS
    从 src-tauri/icons/app-icon.svg 生成各平台图标。
.DESCRIPTION
    图标以 SVG 为唯一源，生成物被 .gitignore 忽略、不纳入版本库。
    编译（cargo/tauri dev、tauri build、scripts/build.ps1）前必须已有 icons/icon.ico，
    因此本脚本由 build.ps1 与 npm 的 pretauri 钩子复用。
    用法: pwsh ./scripts/gen-icons.ps1 [-Force]
#>
[CmdletBinding()]
param(
    # 忽略时间戳缓存，强制重新生成。
    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $tauriCli = Join-Path $root 'node_modules/@tauri-apps/cli/tauri.js'
    if (-not (Test-Path $tauriCli)) { throw '未找到 Tauri CLI，请先执行 npm install' }

    $iconSvg = Join-Path $root 'src-tauri/icons/app-icon.svg'
    $iconIco = Join-Path $root 'src-tauri/icons/icon.ico'
    $stale = $Force -or (-not (Test-Path $iconIco)) -or ((Get-Item $iconSvg).LastWriteTimeUtc -gt (Get-Item $iconIco).LastWriteTimeUtc)
    if (-not $stale) {
        Write-Host '==> 图标已是最新，跳过生成'
        return
    }

    Write-Host '==> 生成图标（源: src-tauri/icons/app-icon.svg）'
    & node $tauriCli icon $iconSvg
    if ($LASTEXITCODE -ne 0) { throw '图标生成失败' }
}
finally {
    Pop-Location
}
