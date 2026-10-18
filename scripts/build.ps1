#!/usr/bin/env pwsh
<#
.SYNOPSIS
    构建 Namesplit 的两个产物：Go 核心进程与 Tauri 应用。
.DESCRIPTION
    版本由 git describe 推导（发布版本 = tag）。产物输出到 build/。
    用法: pwsh ./scripts/build.ps1
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $desc = (& git describe --tags --always --dirty 2>$null)
    if ($LASTEXITCODE -ne 0 -or -not $desc) {
        throw '无法从 git 读取版本：需要 git 仓库且至少有一次提交'
    }
    $desc = $desc.Trim()
    if ($desc -match '^v\d') {
        $version = $desc.Substring(1)
    }
    elseif ($desc -match '^\d+\.\d+\.\d+') {
        $version = $desc
    }
    else {
        $version = "0.0.0-dev+$desc"
    }
    $commit = (& git rev-parse --short HEAD).Trim()

    $outDir = Join-Path $root 'build'
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null

    # 构建元数据供打包/发布脚本读取；避免 version 逻辑在多处重复。
    $meta = @{
        version  = $version
        commit   = $commit
        built_at = (Get-Date).ToUniversalTime().ToString('o')
    }
    $meta | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $outDir 'version.json') -Encoding utf8

    Write-Host "==> 版本 $version（$commit）"

    # 1. Go 核心进程
    $ldflags = "-s -w -X github.com/CedarHuang/Namesplit/internal/version.Version=$version -X github.com/CedarHuang/Namesplit/internal/version.Commit=$commit"
    $coreExe = Join-Path $outDir 'namesplit-core.exe'
    & go build -trimpath -ldflags $ldflags -o $coreExe ./cmd/namesplit-core
    if ($LASTEXITCODE -ne 0) { throw 'Go 核心进程构建失败' }

    # 2. sidecar 作为 Tauri externalBin：tauri-build 要求文件名为 <name>-<target-triple>.exe
    #    （v1 只支持 Win11 x64，因此三元组固定）。tauri-build 会把它复制到应用 exe 同目录。
    $triple = 'x86_64-pc-windows-msvc'
    $binDir = Join-Path $root 'src-tauri/binaries'
    New-Item -ItemType Directory -Force -Path $binDir | Out-Null
    Copy-Item $coreExe (Join-Path $binDir "namesplit-core-$triple.exe") -Force

    # 直接调用 Tauri CLI：npm run 会吞掉部分命令行参数
    $tauriCli = Join-Path $root 'node_modules/@tauri-apps/cli/tauri.js'
    if (-not (Test-Path $tauriCli)) { throw '未找到 Tauri CLI，请先执行 npm install' }

    # 3. 图标：以 src-tauri/icons/app-icon.svg 为唯一源，生成各尺寸 ico/png（生成物不入库）
    & (Join-Path $PSScriptRoot 'gen-icons.ps1')

    # 4. Tauri 应用：版本以内联 JSON 合并注入，不修改纳入版本库的 tauri.conf.json
    $versionOverride = @{ version = $version } | ConvertTo-Json -Compress
    & node $tauriCli build -c $versionOverride
    if ($LASTEXITCODE -ne 0) { throw 'Tauri 应用构建失败' }

    $exe = Join-Path $root 'src-tauri/target/release/namesplit.exe'
    if (-not (Test-Path $exe)) { throw "未找到 Tauri 应用产物: $exe" }
    Copy-Item $exe (Join-Path $outDir 'Namesplit.exe') -Force
    $sidecarCopied = Join-Path $root 'src-tauri/target/release/namesplit-core.exe'
    if (-not (Test-Path $sidecarCopied)) { throw "tauri-build 未复制 externalBin 到产物目录: $sidecarCopied" }
    Copy-Item $sidecarCopied (Join-Path $outDir 'namesplit-core.exe') -Force

    Write-Host '==> 构建完成:'
    Get-ChildItem $outDir | ForEach-Object {
        Write-Host ("    {0}  {1:N1} MB" -f $_.Name, ($_.Length / 1MB))
    }
}
finally {
    Pop-Location
}
