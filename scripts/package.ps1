#!/usr/bin/env pwsh
<#
.SYNOPSIS
    生成 Namesplit 便携 zip、SHA256 清单与体积报告。
.DESCRIPTION
    默认先执行 scripts/build.ps1（Go sidecar + Tauri 应用），再把
    Namesplit.exe、namesplit-core.exe、LICENSE、README.md
    打进 build/release/Namesplit-<版本>-win-x64.zip。
    用法:
      pwsh ./scripts/package.ps1
      pwsh ./scripts/package.ps1 -SkipBuild
#>
[CmdletBinding()]
param(
    [switch]$SkipBuild
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    if (-not $SkipBuild) {
        & (Join-Path $PSScriptRoot 'build.ps1')
        if ($LASTEXITCODE -ne 0) { throw '构建失败' }
    }

    $buildDir = Join-Path $root 'build'
    $metaFile = Join-Path $buildDir 'version.json'
    if (-not (Test-Path -LiteralPath $metaFile)) {
        throw "缺少 $metaFile；请先执行 scripts/build.ps1 或去掉 -SkipBuild"
    }
    $meta = Get-Content -LiteralPath $metaFile -Raw | ConvertFrom-Json
    $version = $meta.version
    if (-not $version) { throw "version.json 缺少 version 字段" }

    $required = @(
        (Join-Path $buildDir 'Namesplit.exe'),
        (Join-Path $buildDir 'namesplit-core.exe'),
        (Join-Path $root 'LICENSE'),
        (Join-Path $root 'README.md')
    )
    foreach ($path in $required) {
        if (-not (Test-Path -LiteralPath $path)) { throw "打包缺少文件: $path" }
    }

    $stage = Join-Path $buildDir 'package/Namesplit'
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $stage | Out-Null

    Copy-Item (Join-Path $buildDir 'Namesplit.exe') $stage
    Copy-Item (Join-Path $buildDir 'namesplit-core.exe') $stage
    Copy-Item (Join-Path $root 'LICENSE') $stage
    Copy-Item (Join-Path $root 'README.md') $stage

    $releaseDir = Join-Path $buildDir 'release'
    New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null

    $zipName = "Namesplit-$version-win-x64.zip"
    $zipPath = Join-Path $releaseDir $zipName
    if (Test-Path -LiteralPath $zipPath) { Remove-Item -LiteralPath $zipPath -Force }
    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zipPath -CompressionLevel Optimal

    $hash = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    "$hash  $zipName" | Set-Content -LiteralPath (Join-Path $releaseDir 'SHA256SUMS.txt') -Encoding ascii

    $zipSize = (Get-Item -LiteralPath $zipPath).Length
    $limit = 30 * 1MB
    $sizeMiB = $zipSize / 1MB

    Write-Host '==> 发布产物:'
    Write-Host ("    {0}  {1:N2} MiB" -f $zipName, $sizeMiB)
    Write-Host ("    SHA256 {0}" -f $hash)
    Write-Host ("    目录 {0}" -f $releaseDir)
    if ($zipSize -gt $limit) {
        Write-Warning ("便携包 {0:N2} MiB 超过 30 MiB 目标，需要在后续里程碑裁剪" -f $sizeMiB)
    }
    else {
        Write-Host ("    体积门禁: {0:N2} MiB ≤ 30 MiB" -f $sizeMiB)
    }
}
finally {
    Pop-Location
}
