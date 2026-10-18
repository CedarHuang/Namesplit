#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Namesplit 负载验证：1,000 QPS 持续压测 namesplit-core 并记录证据。
.DESCRIPTION
    需要 127.0.0.1:53 可用。默认持续 300 秒；结果写到 build/evidence/loadtest.json。
    用法: pwsh ./scripts/loadtest.ps1 [-Qps 1000] [-DurationSec 300]
#>
[CmdletBinding()]
param(
    [int]$Qps = 1000,
    [int]$DurationSec = 300,
    [string]$OutFile = 'build/evidence/loadtest.json'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
$processes = @()
$coreProcess = $null
$tempRoot = Join-Path $root ("build/loadtest/" + (Get-Date -Format 'yyyyMMdd-HHmmss'))

function New-Token {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    return (($bytes | ForEach-Object { $_.ToString('x2') }) -join '')
}

try {
    New-Item -ItemType Directory -Force -Path $tempRoot, (Join-Path $tempRoot 'LocalAppData/Namesplit') | Out-Null

    Write-Host '==> 构建'
    & go build -o (Join-Path $root 'build/namesplit-core.exe') ./cmd/namesplit-core
    if ($LASTEXITCODE -ne 0) { throw '构建 namesplit-core 失败' }
    & go build -o (Join-Path $root 'build/mockupstream.exe') ./cmd/mockupstream
    if ($LASTEXITCODE -ne 0) { throw '构建 mockupstream 失败' }
    & go build -o (Join-Path $root 'build/loadgen.exe') ./cmd/loadgen
    if ($LASTEXITCODE -ne 0) { throw '构建 loadgen 失败' }

    $mockOut = Join-Path $tempRoot 'mock.out'
    $mockErr = Join-Path $tempRoot 'mock.err'
    $mock = Start-Process -FilePath (Join-Path $root 'build/mockupstream.exe') -ArgumentList @('-a', '192.0.2.1') `
        -RedirectStandardOutput $mockOut -RedirectStandardError $mockErr -PassThru -NoNewWindow
    $processes += $mock

    $deadline = (Get-Date).AddSeconds(5)
    $mockUrl = $null
    while ((Get-Date) -lt $deadline -and -not $mockUrl) {
        if (Test-Path $mockOut) {
            $line = Get-Content $mockOut -First 1 -ErrorAction SilentlyContinue
            if ($line) { try { $mockUrl = ($line | ConvertFrom-Json).udp } catch { } }
        }
        Start-Sleep -Milliseconds 50
    }
    if (-not $mockUrl) { throw 'mock 上游未就绪' }

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
url = "$mockUrl"
timeout_ms = 5000
[groups.default]
name = "default"
upstreams = ["mock"]
"@
    Set-Content -LiteralPath (Join-Path $tempRoot 'LocalAppData/Namesplit/config.toml') -Value $config -Encoding utf8

    $token = New-Token
    $coreStdout = Join-Path $tempRoot 'core.out'
    $coreStderr = Join-Path $tempRoot 'core.err'

    $oldToken = $env:NAMESPLIT_TOKEN
    $oldLocalAppData = $env:LOCALAPPDATA
    $env:NAMESPLIT_TOKEN = $token
    $env:LOCALAPPDATA = (Join-Path $tempRoot 'LocalAppData')
    try {
        $coreProcess = Start-Process -FilePath (Join-Path $root 'build/namesplit-core.exe') `
            -RedirectStandardOutput $coreStdout -RedirectStandardError $coreStderr -PassThru -NoNewWindow
    }
    finally {
        $env:NAMESPLIT_TOKEN = $oldToken
        $env:LOCALAPPDATA = $oldLocalAppData
    }
    $processes += $coreProcess

    $deadline = (Get-Date).AddSeconds(10)
    $ready = $null
    while ((Get-Date) -lt $deadline -and -not $ready) {
        if (Test-Path $coreStdout) {
            $text = Get-Content $coreStdout -Raw -ErrorAction SilentlyContinue
            if ($text -match 'NAMESPLIT_READY (\{[^\r\n]+\})') { $ready = $Matches[1] | ConvertFrom-Json }
        }
        Start-Sleep -Milliseconds 50
    }
    if (-not $ready) { throw 'core 未就绪' }

    $headers = @{ 'X-Namesplit-Token' = $token }
    $apiBase = "http://127.0.0.1:$($ready.port)"
    $status = Invoke-RestMethod -Uri "$apiBase/api/status" -Headers $headers
    if (-not ($status.dns.udp -contains '127.0.0.1:53')) {
        throw "DNS 未监听 127.0.0.1:53: $($status.dns | ConvertTo-Json -Compress)"
    }

    Write-Host "==> 压测 ${Qps} QPS × ${DurationSec}s"
    $loadOutput = & (Join-Path $root 'build/loadgen.exe') -addr '127.0.0.1:53' -qps $Qps -duration "${DurationSec}s"
    if ($LASTEXITCODE -ne 0) { throw "loadgen 失败: $loadOutput" }
    $loadResult = $loadOutput | ConvertFrom-Json

    $stats = Invoke-RestMethod -Uri "$apiBase/api/stats" -Headers $headers
    Start-Sleep -Seconds 2
    $statsAfter = Invoke-RestMethod -Uri "$apiBase/api/stats" -Headers $headers
    $page = Invoke-RestMethod -Uri "$apiBase/api/queries?limit=500" -Headers $headers

    try { Invoke-RestMethod -Method Post -Uri "$apiBase/api/shutdown" -Headers $headers | Out-Null } catch { }
    Start-Sleep -Milliseconds 500

    $dbPath = Join-Path $tempRoot 'LocalAppData/Namesplit/querylog.db'
    $dbBytes = 0
    if (Test-Path $dbPath) { $dbBytes = (Get-Item $dbPath).Length }
    foreach ($suffix in @('-wal', '-shm')) {
        if (Test-Path ($dbPath + $suffix)) { $dbBytes += (Get-Item ($dbPath + $suffix)).Length }
    }
    $dbLimitBytes = 100 * 1024 * 1024

    # 逐项判定，任一项不满足都必须以非零退出码结束，避免只看证据文件存在就判为通过。
    $historyItems = @($page.items).Count
    $sent = [int64]$loadResult.sent
    $queriesAfter = [int64]$statsAfter.counters.queries
    $persistErrors = [int64]$statsAfter.counters.persist_errors
    $checks = [ordered]@{
        loadgen_failed_zero   = ([int64]$loadResult.failed -eq 0)
        loadgen_ok_rate       = ([int64]$loadResult.ok -ge ($Qps * $DurationSec * 0.99))
        db_bytes_within_limit = ($dbBytes -le $dbLimitBytes)
        queries_match_sent    = ($queriesAfter -eq $sent)
        persist_errors_zero   = ($persistErrors -eq 0)
        history_not_empty     = ($historyItems -gt 0)
    }
    $failedChecks = @($checks.GetEnumerator() | Where-Object { -not $_.Value } | ForEach-Object { $_.Key })
    $ok = ($failedChecks.Count -eq 0)

    $outPath = Join-Path $root $OutFile
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $outPath) | Out-Null
    [ordered]@{
        generated_at     = (Get-Date).ToUniversalTime().ToString('o')
        load             = $loadResult
        sent             = $sent
        stats            = $stats
        stats_after      = $statsAfter
        counters_queries = $queriesAfter
        history_page     = @{ items = $historyItems; persistence_enabled = $page.persistence_enabled }
        db_bytes         = $dbBytes
        db_bytes_limit   = $dbLimitBytes
        checks           = $checks
        ok               = $ok
    } | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $outPath -Encoding utf8

    if (-not $ok) {
        $failureDetails = $failedChecks | ForEach-Object {
            switch ($_) {
                'loadgen_failed_zero' { "loadgen.failed=$($loadResult.failed)" }
                'loadgen_ok_rate' { "loadgen.ok=$($loadResult.ok) 低于 $($Qps * $DurationSec * 0.99)" }
                'db_bytes_within_limit' { "db_bytes=$dbBytes 超过 $dbLimitBytes" }
                'queries_match_sent' { "counters.queries=$queriesAfter 与 sent=$sent 不一致" }
                'persist_errors_zero' { "persist_errors=$persistErrors" }
                'history_not_empty' { "history_page.items=$historyItems" }
            }
        }
        throw ("负载验收失败：{0}（证据已写入 {1}）" -f ($failureDetails -join '；'), $outPath)
    }

    Write-Host ("结果: {0}" -f $outPath)
}
finally {
    foreach ($process in $processes) {
        if ($null -ne $process -and -not $process.HasExited) {
            Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        }
    }

    Pop-Location
}
