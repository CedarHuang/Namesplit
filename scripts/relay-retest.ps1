#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Tauri 应用事件中继测量：启动 sidecar 与探针，跑吞吐、延迟与慢消费者场景，输出 JSON。
.DESCRIPTION
    默认使用真实 namesplit-core（loadgen 驱动查询）；-Mode devstub 保留旧假 sidecar 模式。
    结果默认写入 build/evidence/relay-retest.json。
    用法: pwsh ./scripts/relay-retest.ps1 [-Mode core|devstub] [-Rate 1100]
#>
[CmdletBinding()]
param(
    [ValidateSet('core', 'devstub')]
    [string]$Mode = 'core',
    [int]$Rate = 1100,
    [int]$DurationSec = 8,
    [int]$SlowSec = 3,
    [string]$OutFile = 'build/evidence/relay-retest.json'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
$mockProcess = $null
$tempRoot = Join-Path $root ("build/relay-retest/" + (Get-Date -Format 'yyyyMMdd-HHmmss'))

function Start-MockUpstream {
    param([string]$Answer)

    $stdout = Join-Path $tempRoot 'mock.out'
    $stderr = Join-Path $tempRoot 'mock.err'
    $process = Start-Process -FilePath (Join-Path $root 'build/mockupstream.exe') -ArgumentList @('-a', $Answer) `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru -NoNewWindow

    $deadline = (Get-Date).AddSeconds(5)
    while ((Get-Date) -lt $deadline) {
        if (Test-Path $stdout) {
            $line = Get-Content $stdout -First 1 -ErrorAction SilentlyContinue
            if ($line) {
                try {
                    $json = $line | ConvertFrom-Json
                    if ($json.udp) { return @{ Process = $process; Url = $json.udp } }
                }
                catch { }
            }
        }
        Start-Sleep -Milliseconds 50
    }

    throw 'mock 上游未就绪'
}

# 真实内核会绑定 127.0.0.1:53；端口被占用时内核启动失败，提前检查并给出明确错误。
function Test-DnsPortConflict {
    param([ValidateSet('Tcp', 'Udp')][string]$Protocol)

    $socketType = if ($Protocol -eq 'Tcp') {
        [System.Net.Sockets.SocketType]::Stream
    }
    else {
        [System.Net.Sockets.SocketType]::Dgram
    }
    $protocolType = if ($Protocol -eq 'Tcp') {
        [System.Net.Sockets.ProtocolType]::Tcp
    }
    else {
        [System.Net.Sockets.ProtocolType]::Udp
    }

    $socket = [System.Net.Sockets.Socket]::new(
        [System.Net.Sockets.AddressFamily]::InterNetwork, $socketType, $protocolType)
    try {
        # 独占绑定：即使占用者未设置独占标志，也能探测到已有监听。
        $socket.ExclusiveAddressUse = $true
        $socket.Bind([System.Net.IPEndPoint]::new([System.Net.IPAddress]::Loopback, 53))
        if ($Protocol -eq 'Tcp') { $socket.Listen(1) }
        return $false
    }
    catch {
        return $true
    }
    finally {
        $socket.Dispose()
    }
}

try {
    New-Item -ItemType Directory -Force -Path $tempRoot, (Join-Path $tempRoot 'LocalAppData/Namesplit') | Out-Null
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent (Join-Path $root $OutFile)) | Out-Null

    $conflicts = @()
    foreach ($protocol in @('Tcp', 'Udp')) {
        if (Test-DnsPortConflict -Protocol $protocol) { $conflicts += $protocol }
    }
    if ($conflicts.Count -gt 0) {
        throw "127.0.0.1:53 的 $($conflicts -join '、') 已被占用，请先停止占用该端口的进程"
    }

    Write-Host '==> 构建 sidecar 与探针'
    & go build -o (Join-Path $root 'build/namesplit-core.exe') ./cmd/namesplit-core
    if ($LASTEXITCODE -ne 0) { throw '构建 namesplit-core 失败' }
    & go build -o (Join-Path $root 'build/loadgen.exe') ./cmd/loadgen
    if ($LASTEXITCODE -ne 0) { throw '构建 loadgen 失败' }
    & go build -o (Join-Path $root 'build/mockupstream.exe') ./cmd/mockupstream
    if ($LASTEXITCODE -ne 0) { throw '构建 mockupstream 失败' }
    & go build -o (Join-Path $root 'build/devstub.exe') ./cmd/devstub
    if ($LASTEXITCODE -ne 0) { throw '构建 devstub 失败' }
    & cargo build --manifest-path (Join-Path $root 'src-tauri/Cargo.toml') --bin relay_probe
    if ($LASTEXITCODE -ne 0) { throw '构建 relay_probe 失败' }

    $probe = Join-Path $root 'src-tauri/target/debug/relay_probe.exe'
    $out = Join-Path $root $OutFile

    if ($Mode -eq 'core') {
        $mock = Start-MockUpstream -Answer '192.0.2.1'
        $mockProcess = $mock.Process

        $config = @"
schema_version = 1
rules = []
local_records = []
[settings]
default_group = "default"
log_ring_size = 5000
log_persist = true
ptr_auto = true
[upstreams.mock]
name = "mock"
url = "$($mock.Url)"
timeout_ms = 5000
[groups.default]
name = "default"
upstreams = ["mock"]
"@
        Set-Content -LiteralPath (Join-Path $tempRoot 'LocalAppData/Namesplit/config.toml') -Value $config -Encoding utf8

        Write-Host "==> 真实内核中继测量（loadgen ${Rate} QPS）"
        & $probe --core (Join-Path $root 'build/namesplit-core.exe') `
            --loadgen (Join-Path $root 'build/loadgen.exe') `
            --local-app-data (Join-Path $tempRoot 'LocalAppData') `
            --rate $Rate --duration-sec $DurationSec --slow-sec $SlowSec --out $out
    }
    else {
        Write-Host '==> devstub 中继测量'
        & $probe --devstub (Join-Path $root 'build/devstub.exe') `
            --rate $Rate --duration-sec $DurationSec --slow-sec $SlowSec --out $out
    }

    if ($LASTEXITCODE -ne 0) { throw "relay_probe 失败，退出码 $LASTEXITCODE" }

    # 探针退出码之外再核对证据 JSON，防止探针未来退化后脚本仍然放行。
    if (-not (Test-Path -LiteralPath $out)) { throw "relay_probe 未生成证据文件: $out" }
    $evidence = Get-Content -LiteralPath $out -Raw -Encoding utf8 | ConvertFrom-Json
    $failures = @()
    if ($evidence.auth.wrong_token_status -ne 401) {
        $failures += "auth.wrong_token_status=$($evidence.auth.wrong_token_status)，期望 401"
    }
    if ($evidence.thresholds.rate_met -ne $true) {
        $failures += "thresholds.rate_met=$($evidence.thresholds.rate_met)"
    }
    if ($evidence.thresholds.latency_met -ne $true) {
        $failures += "thresholds.latency_met=$($evidence.thresholds.latency_met)"
    }
    if ($null -ne $evidence.reconnect -and $evidence.thresholds.reconnect_met -ne $true) {
        $failures += "thresholds.reconnect_met=$($evidence.thresholds.reconnect_met)"
    }
    if ($failures.Count -gt 0) {
        throw "中继测量证据未达标：$($failures -join '；')"
    }

    Write-Host ("结果: {0}" -f $out)
}
finally {
    if ($null -ne $mockProcess -and -not $mockProcess.HasExited) {
        Stop-Process -Id $mockProcess.Id -Force -ErrorAction SilentlyContinue
    }
    Pop-Location
}
