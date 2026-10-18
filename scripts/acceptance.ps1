#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Namesplit 端到端测试脚本。
.DESCRIPTION
    启动真实 namesplit-core 与 mock 上游，在临时数据目录内运行端到端检查。
    需要 127.0.0.1:53 可用（应用监听该地址）。
    用法:
      pwsh ./scripts/acceptance.ps1
      pwsh ./scripts/acceptance.ps1 -Cases routing,priority -OutFile build/evidence/acceptance.json
#>
[CmdletBinding()]
param(
    [string[]]$Cases = @('routing', 'local', 'priority', 'logs', 'failover'),
    [string]$OutFile = 'build/evidence/acceptance.json'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
$script:processes = @()
$script:results = @()
$script:ready = $null
$coreProcess = $null
$tempRoot = Join-Path $root ("build/acceptance/" + (Get-Date -Format 'yyyyMMdd-HHmmss'))

function New-Token {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    return (($bytes | ForEach-Object { $_.ToString('x2') }) -join '')
}

function Start-MockUpstream {
    param([string]$Answer, [int]$DelayMs = 0)

    $stdout = Join-Path $tempRoot ("mock-" + [guid]::NewGuid().ToString('N') + ".out")
    $stderr = Join-Path $tempRoot ("mock-" + [guid]::NewGuid().ToString('N') + ".err")
    $arguments = @('-a', $Answer, '-ttl', '60')
    if ($DelayMs -gt 0) { $arguments += @('-delay', "${DelayMs}ms") }

    $process = Start-Process -FilePath (Join-Path $root 'build/mockupstream.exe') -ArgumentList $arguments `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru -NoNewWindow
    $script:processes += $process

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

    throw "mock 上游未在 5s 内就绪: $stdout"
}

function Wait-CoreReady {
    param([string]$StdoutPath, [int]$Seconds = 10)

    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-Path $StdoutPath) {
            $text = Get-Content $StdoutPath -Raw -ErrorAction SilentlyContinue
            if ($text -match 'NAMESPLIT_READY (\{[^\r\n]+\})') {
                return ($Matches[1] | ConvertFrom-Json)
            }
        }
        Start-Sleep -Milliseconds 50
    }

    throw '等待 core READY 超时'
}

function Invoke-Api {
    param([string]$Method, [string]$Path, $Body = $null)

    $headers = @{ 'X-Namesplit-Token' = $token }
    $uri = "http://127.0.0.1:$($script:ready.port)$Path"
    if ($null -ne $Body) {
        return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers -ContentType 'application/json' -Body ($Body | ConvertTo-Json -Depth 12)
    }

    return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers
}

function Wait-Api {
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        try {
            Invoke-Api -Method Get -Path '/api/health' | Out-Null
            return
        }
        catch { Start-Sleep -Milliseconds 100 }
    }
    throw 'API 未就绪'
}

function Resolve-A {
    param([string]$Name)

    try {
        $response = Resolve-DnsName -Name $Name -Type A -Server 127.0.0.1 -DnsOnly -QuickTimeout -ErrorAction Stop
        return @($response | Where-Object { $_.Type -eq 'A' } | ForEach-Object { $_.IPAddress })
    }
    catch {
        return @()
    }
}

function Wait-Events {
    param([string]$Domain, [int]$Count = 1, [int]$Seconds = 5)

    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        $page = Invoke-Api -Method Get -Path '/api/queries?limit=500'
        $matched = @($page.items | Where-Object { $_.domain -eq $Domain })
        if ($matched.Count -ge $Count) { return $matched }
        Start-Sleep -Milliseconds 100
    }

    return @()
}

function Add-Result {
    param([string]$Id, [string]$Name, [bool]$Ok, [string]$Detail)

    $script:results += [ordered]@{ id = $Id; name = $Name; ok = $Ok; detail = $Detail }
    $status = if ($Ok) { 'PASS' } else { 'FAIL' }
    Write-Host ("  [CASE {0}] {1}: {2} {3}" -f $Id, $Name, $status, $Detail)
}

function Assert-Address {
    param([string]$Name, [string]$Expected, [string]$Context)

    $addresses = Resolve-A -Name $Name
    if ($addresses -notcontains $Expected) {
        throw ("{0}: {1} 解析为 [{2}]，期望 {3}" -f $Context, $Name, ($addresses -join ', '), $Expected)
    }
}

try {
    New-Item -ItemType Directory -Force -Path $tempRoot, (Join-Path $tempRoot 'LocalAppData/Namesplit') | Out-Null
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent (Join-Path $root $OutFile)) | Out-Null

    Write-Host '==> 构建 namesplit-core 与 mockupstream'
    & go build -o (Join-Path $root 'build/namesplit-core.exe') ./cmd/namesplit-core
    if ($LASTEXITCODE -ne 0) { throw '构建 namesplit-core 失败' }
    & go build -o (Join-Path $root 'build/mockupstream.exe') ./cmd/mockupstream
    if ($LASTEXITCODE -ne 0) { throw '构建 mockupstream 失败' }

    Write-Host '==> 启动 mock 上游'
    $mock1 = Start-MockUpstream -Answer '192.0.2.1'
    $mock2 = Start-MockUpstream -Answer '192.0.2.2'
    $mock3 = Start-MockUpstream -Answer '192.0.2.3'
    $mockFast = Start-MockUpstream -Answer '192.0.2.4'
    $mockSlow = Start-MockUpstream -Answer '192.0.2.9' -DelayMs 2000

    $config = @"
schema_version = 1

rules = [
  "DOMAIN, api.example.com, group-b",
  "DOMAIN-SUFFIX, example.com, group-a",
  "DOMAIN-SUFFIX, google.test, global",
  "DOMAIN-SUFFIX, corp.local, corp",
  "DOMAIN-SUFFIX, slow.test, slowgroup",
  "DOMAIN-SUFFIX, dead.test, deadgroup",
]

local_records = [
  "A, nas.home, 60, true, 192.168.1.10",
]

[settings]
default_group = "cn"
log_ring_size = 5000
log_persist = true
ptr_auto = true

[upstreams.g1]
name = "g1"
url = "$($mock1.Url)"
timeout_ms = 5000

[upstreams.g2]
name = "g2"
url = "$($mock2.Url)"
timeout_ms = 5000

[upstreams.g3]
name = "g3"
url = "$($mock3.Url)"
timeout_ms = 5000

[upstreams.fast]
name = "fast"
url = "$($mockFast.Url)"
timeout_ms = 5000

[upstreams.slow]
name = "slow"
url = "$($mockSlow.Url)"
timeout_ms = 5000

[upstreams.dead]
name = "dead"
url = "tcp://127.0.0.1:1"
timeout_ms = 300

[groups.global]
name = "global"
upstreams = ["g1"]

[groups.corp]
name = "corp"
upstreams = ["g2"]

[groups.cn]
name = "cn"
upstreams = ["g3"]

[groups.group-a]
name = "group-a"
upstreams = ["g1"]

[groups.group-b]
name = "group-b"
upstreams = ["g2"]

[groups.slowgroup]
name = "slowgroup"
upstreams = ["slow", "fast"]

[groups.deadgroup]
name = "deadgroup"
upstreams = ["dead"]
"@
    Set-Content -LiteralPath (Join-Path $tempRoot 'LocalAppData/Namesplit/config.toml') -Value $config -Encoding utf8

    $token = New-Token
    $coreStdout = Join-Path $tempRoot 'core.out'
    $coreStderr = Join-Path $tempRoot 'core.err'

    Write-Host '==> 启动 namesplit-core'
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
    $script:processes += $coreProcess

    $script:ready = Wait-CoreReady -StdoutPath $coreStdout
    Wait-Api

    $dnsStatus = (Invoke-Api -Method Get -Path '/api/status').dns
    if (-not ($dnsStatus.udp -contains '127.0.0.1:53')) {
        throw "DNS 未监听 127.0.0.1:53（可能被占用）: $($dnsStatus | ConvertTo-Json -Compress)"
    }

    foreach ($case in $Cases) {
        switch ($case) {
            'routing' {
                try {
                    Assert-Address -Name 'www.google.test' -Expected '192.0.2.1' -Context '按域名分流'
                    Assert-Address -Name 'api.corp.local' -Expected '192.0.2.2' -Context '按域名分流'
                    Assert-Address -Name 'other.test' -Expected '192.0.2.3' -Context '按域名分流'

                    $events = Wait-Events -Domain 'www.google.test.'
                    if ($events.Count -lt 1 -or $events[0].group -ne 'global') {
                        throw '分流事件的上游组与预期不一致'
                    }
                    Add-Result -Id $case -Name '按域名分流' -Ok $true -Detail '三个域名分别命中 global/corp/cn'
                }
                catch {
                    Add-Result -Id $case -Name '按域名分流' -Ok $false -Detail $_.Exception.Message
                }
            }
            'local' {
                try {
                    Assert-Address -Name 'nas.home' -Expected '192.168.1.10' -Context '本地映射'
                    $events = Wait-Events -Domain 'nas.home.'
                    if ($events.Count -lt 1) { throw '缺少本地记录事件' }
                    if ($events[0].source -ne 'local' -or $events[0].upstream -ne '' -or $events[0].group -ne '') {
                        throw "事件字段不是本地应答: $($events[0] | ConvertTo-Json -Compress)"
                    }
                    Resolve-A -Name 'nas.home' | Out-Null
                    Add-Result -Id $case -Name '本地映射' -Ok $true -Detail '返回本地 IP，source=local 且无上游'
                }
                catch {
                    Add-Result -Id $case -Name '本地映射' -Ok $false -Detail $_.Exception.Message
                }
            }
            'priority' {
                try {
                    Assert-Address -Name 'api.example.com' -Expected '192.0.2.2' -Context '规则优先级'
                    Assert-Address -Name 'www.example.com' -Expected '192.0.2.1' -Context '规则优先级'
                    Assert-Address -Name 'example.com' -Expected '192.0.2.1' -Context '规则优先级'
                    Add-Result -Id $case -Name '规则优先级' -Ok $true -Detail '精确规则先于后缀，后缀包含 apex'
                }
                catch {
                    Add-Result -Id $case -Name '规则优先级' -Ok $false -Detail $_.Exception.Message
                }
            }
            'logs' {
                try {
                    for ($i = 0; $i -lt 100; $i++) {
                        Resolve-A -Name "log-$i.test" | Out-Null
                    }

                    $deadline = (Get-Date).AddSeconds(10)
                    $items = @()
                    while ((Get-Date) -lt $deadline) {
                        $page = Invoke-Api -Method Get -Path '/api/queries?limit=500'
                        $items = @($page.items)
                        if ($items.Count -ge 100) { break }
                        Start-Sleep -Milliseconds 100
                    }
                    if ($items.Count -lt 100) { throw "历史记录只有 $($items.Count) 条" }

                    foreach ($item in $items | Select-Object -First 100) {
                        foreach ($field in @('time', 'domain', 'qtype', 'source', 'group', 'upstream', 'upstream_name', 'rcode_name', 'protocol')) {
                            if ([string]::IsNullOrEmpty([string]$item.$field)) {
                                throw "字段 $field 为空: $($item.domain)"
                            }
                        }
                        if (@($item.answers).Count -eq 0) { throw "answers 为空: $($item.domain)" }
                    }

                    Add-Result -Id $case -Name '日志完整性' -Ok $true -Detail '100 条记录全部可见且字段非空'
                }
                catch {
                    Add-Result -Id $case -Name '日志完整性' -Ok $false -Detail $_.Exception.Message
                }
            }
            'failover' {
                try {
                    Assert-Address -Name 'slow.test' -Expected '192.0.2.4' -Context '上游容错'

                    Resolve-A -Name 'dead.test' | Out-Null
                    $events = Wait-Events -Domain 'dead.test.'
                    if ($events.Count -lt 1) { throw '缺少全部上游失败的事件' }
                    if ($events[0].rcode -ne 2 -or [string]::IsNullOrEmpty($events[0].error)) {
                        throw "失败事件字段不符: $($events[0] | ConvertTo-Json -Compress)"
                    }

                    Add-Result -Id $case -Name '上游容错' -Ok $true -Detail '并发组取快上游，全失败返回 SERVFAIL'
                }
                catch {
                    Add-Result -Id $case -Name '上游容错' -Ok $false -Detail $_.Exception.Message
                }
            }
            default {
                Add-Result -Id $case -Name '未实现用例' -Ok $false -Detail '未知用例'
            }
        }
    }
}
finally {
    if ($null -ne $coreProcess -and -not $coreProcess.HasExited) {
        try { Invoke-Api -Method Post -Path '/api/shutdown' | Out-Null } catch { }
        Start-Sleep -Milliseconds 300
    }
    foreach ($process in $script:processes) {
        if ($null -ne $process -and -not $process.HasExited) {
            Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        }
    }

    $ok = ($script:results.Count -gt 0) -and (@($script:results | Where-Object { -not $_.ok }).Count -eq 0)
    $outPath = Join-Path $root $OutFile
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $outPath) | Out-Null
    [ordered]@{
        generated_at = (Get-Date).ToUniversalTime().ToString('o')
        cases        = $script:results
        ok           = $ok
    } | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $outPath -Encoding utf8

    Write-Host ''
    $summary = if ($ok) { '通过' } else { '失败' }
    Write-Host ("结果: {0}" -f $summary)
    Write-Host ("证据: {0}" -f $outPath)

    Pop-Location

    if (-not $ok) { exit 1 }
}


