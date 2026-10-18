#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Tauri 应用代理事件路径测量：构建 devstub 与探针，跑吞吐/延迟/重连/慢消费者，输出 JSON 结果。
.DESCRIPTION
    结果默认写入 build/evidence/relay-<时间戳>.json（build/ 为构建产物目录）。
    用法: pwsh ./scripts/relay-retest.ps1
#>
[CmdletBinding()]
param(
    [int]$Rate = 1000,
    [int]$DurationSec = 8,
    [int]$SlowSec = 3,
    [string]$OutDir = 'build/evidence'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    Write-Host '==> 构建 devstub'
    $devstub = Join-Path $root 'build/devstub.exe'
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $devstub) | Out-Null
    & go build -o $devstub ./cmd/devstub
    if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/devstub 失败' }

    Write-Host '==> 构建 relay_probe'
    & cargo build --manifest-path (Join-Path $root 'src-tauri/Cargo.toml') --bin relay_probe
    if ($LASTEXITCODE -ne 0) { throw 'cargo build --bin relay_probe 失败' }

    $probe = Join-Path $root 'src-tauri/target/debug/relay_probe.exe'
    if (-not (Test-Path $probe)) { throw "未找到探针: $probe" }

    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $resolvedOutDir = if ([System.IO.Path]::IsPathRooted($OutDir)) { $OutDir } else { Join-Path $root $OutDir }
    New-Item -ItemType Directory -Force -Path $resolvedOutDir | Out-Null
    $out = Join-Path $resolvedOutDir "relay-$stamp.json"

    Write-Host "==> 运行复测（$Rate 事件/秒，吞吐 ${DurationSec}s，慢消费者 ${SlowSec}s）"
    & $probe --devstub $devstub --rate $Rate --duration-sec $DurationSec --slow-sec $SlowSec --out $out
    if ($LASTEXITCODE -ne 0) { throw "relay_probe 失败，退出码 $LASTEXITCODE" }

    Write-Host "==> 结果: $out"
}
finally {
    Pop-Location
}
