#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Namesplit 验收脚本骨架：12 条端到端验收用例。
.DESCRIPTION
    当前只建立骨架与可自动化的冒烟检查；逐条验收用例在对应功能实现后填充。
    用法:
      pwsh ./scripts/acceptance.ps1                 # 冒烟 + 用例清单
      pwsh ./scripts/acceptance.ps1 -List           # 只列用例
      pwsh ./scripts/acceptance.ps1 -DnsServer 127.0.0.1
#>
[CmdletBinding()]
param(
    [string]$DnsServer = '127.0.0.1',
    [string]$DataDir = (Join-Path $env:LOCALAPPDATA 'Namesplit'),
    [switch]$List
)

$ErrorActionPreference = 'Stop'

# 每条用例在实现对应功能后填充 Script 字段；当前只保留验收描述与判据。
$cases = @(
    @{ Id = '1';  Name = '按域名分流';      Script = $null }
    @{ Id = '2';  Name = '本地映射';        Script = $null }
    @{ Id = '3';  Name = '规则优先级';      Script = $null }
    @{ Id = '4';  Name = '日志完整性';      Script = $null }
    @{ Id = '5';  Name = '实时性';          Script = $null }
    @{ Id = '6';  Name = '手动配置与退出提醒'; Script = $null }
    @{ Id = '7';  Name = '上游容错';        Script = $null }
    @{ Id = '8';  Name = '端口冲突';        Script = $null }
    @{ Id = '9';  Name = '性能';            Script = $null }
    @{ Id = '10'; Name = '清理无残留';      Script = $null }
    @{ Id = '11'; Name = '便携与权限';      Script = $null }
    @{ Id = '12'; Name = '历史日志回溯';    Script = $null }
)

if ($List) {
    $cases | Select-Object Id, Name | Format-Table -AutoSize
    return
}

# --- 环境冒烟检查 -----------------------------------------------------------

$failures = 0

Write-Host '== 环境冒烟检查 =='

$dataDirOk = Test-Path -LiteralPath $DataDir
Write-Host ("  数据目录 {0}: {1}" -f $DataDir, $(if ($dataDirOk) { '存在' } else { '不存在（应用未启动过）' }))

try {
    $resp = Resolve-DnsName -Name 'example.com' -Type A -Server $DnsServer -DnsOnly -QuickTimeout -ErrorAction Stop
    $a = @($resp | Where-Object { $_.Type -eq 'A' })
    if ($a.Count -gt 0) {
        Write-Host ("  解析 {0} 成功（{1} 条 A）" -f $DnsServer, $a.Count)
    }
    else {
        Write-Host "  解析返回无 A 记录"
        $failures++
    }
}
catch {
    Write-Host ("  解析 {0} 失败: {1}" -f $DnsServer, $_.Exception.Message)
    $failures++
}

# --- 待实现用例清单 ---------------------------------------------------------

Write-Host ''
Write-Host '== 待实现验收用例（PRD §11）=='
$cases | ForEach-Object {
    Write-Host ("  [{0,2}] {1}" -f $_.Id, $_.Name)
}
Write-Host ''
Write-Host '说明：当前只提供骨架；用例在对应功能实现后补 Script 并接入 CI。'

if ($failures -gt 0) {
    Write-Host ("冒烟检查失败: {0} 项" -f $failures)
    exit 1
}

Write-Host '冒烟检查通过。'
