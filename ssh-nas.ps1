# ssh-nas.ps1 — Interactive SSH toolbox for the home-datacenter NAS.
#
# A convenient wrapper around ssh/scp for the daily NAS operations that
# deploy-nas.ps1 doesn't cover: quick command execution, service status,
# log tailing, container management, and disk usage. It reuses the same
# SSH options / password-auth mechanism as deploy-nas.ps1 so the two
# scripts stay consistent.
#
# Usage:
#   .\ssh-nas.ps1                    # interactive menu
#   .\ssh-nas.ps1 -Command "docker ps"   # run one command, print, exit
#   .\ssh-nas.ps1 -Menu              # force interactive menu (default)
#   .\ssh-nas.ps1 -Password '<YOUR_PASSWORD>'  # password auth (no SSH key)
#
# Interactive menu items:
#   1) 服务状态      docker compose ps
#   2) 查看日志      docker compose logs -f api (Ctrl-C 退出)
#   3) 容器列表      docker ps
#   4) 磁盘占用      df -h + du of data dir
#   5) 转码缓存      du of .transcode-cache
#   6) 清理转码缓存  remove cache older than N days
#   7) 健康检查      curl API /health
#   8) 自定义命令    任意 NAS 命令
#   9) 退出

[CmdletBinding()]
param(
    [string]$Command,
    [switch]$Menu,
    # NAS password. Same mechanism as deploy-nas.ps1 (SSH_ASKPASS).
    [string]$Password,
    # Force acquiring an H3C Oasis Cloud relay tunnel even if LAN is reachable.
    [switch]$ForceH3C,
    # Keep the H3C relay tunnel alive after script completes (do not auto-delete).
    [switch]$KeepTunnel
)

# ============== CONFIG (keep in sync with deploy-nas.ps1) ==============
$NAS_HOST   = if ($env:NAS_HOST) { $env:NAS_HOST } else { "100.90.67.71" }
$NAS_USER   = if ($env:NAS_USER) { $env:NAS_USER } else { "momo" }
$NAS_PORT   = if ($env:NAS_PORT) { [int]$env:NAS_PORT } else { 22 }
$REMOTE_PATH = "/vol1/docker/home-datacenter"
# =======================================================================

$ProjectRoot = $PSScriptRoot

# ---- H3C Relay & Endpoint Management ----
$RelayScript = Join-Path $ProjectRoot "tools\h3c_relay.py"
$RelayTunnelId = $null
$RelayCreatedNew = $false

function Cleanup-RelayTunnel {
    if ($script:RelayCreatedNew -and $script:RelayTunnelId -and (-not $KeepTunnel)) {
        Write-Host "==> Cleaning up temporary H3C relay tunnel: $($script:RelayTunnelId)..." -ForegroundColor Cyan
        & python $RelayScript release $script:RelayTunnelId 2>$null | Out-Null
        $script:RelayTunnelId = $null
        $script:RelayCreatedNew = $false
    }
}

function Cleanup-All {
    Remove-Askpass
    Cleanup-RelayTunnel
}

trap { Cleanup-All; break }
Register-EngineEvent PowerShell.Exiting -Action { Cleanup-All } | Out-Null

function Resolve-NasEndpoint {
    if (Test-Path $RelayScript) {
        try {
            $relayArgs = @("$RelayScript", "acquire", "--json")
            if ($ForceH3C) { $relayArgs += "--force-h3c" }
            $jsonOut = & python @relayArgs 2>$null
            if ($LASTEXITCODE -eq 0 -and $jsonOut) {
                $ep = $jsonOut | ConvertFrom-Json
                if ($ep -and $ep.host -and $ep.port) {
                    $script:NAS_HOST = $ep.host
                    $script:NAS_PORT = [int]$ep.port
                    $script:NAS_USER = if ($ep.user) { $ep.user } else { $script:NAS_USER }
                    $script:RelayTunnelId = $ep.tunnel_id
                    $script:RelayCreatedNew = [bool]$ep.created_new
                    Write-Host "==> Connected via Tier [$(($ep.tier).ToUpper())]: $($script:NAS_USER)@$($script:NAS_HOST):$($script:NAS_PORT)" -ForegroundColor Green
                    return
                }
            }
        } catch {
            Write-Warning "Failed to query H3C relay resolver: $_"
        }
    }
    Write-Host "==> Using default NAS endpoint: $script:NAS_USER@$script:NAS_HOST`:$script:NAS_PORT" -ForegroundColor Yellow
}

$ErrorActionPreference = "Stop"

# ---- Password auth setup (SSH_ASKPASS) ----
$script:AskpassFile = ""
if ($Password) {
    $script:AskpassFile = [System.IO.Path]::GetTempFileName() + "-askpass.bat"
    "@echo $Password" | Set-Content $script:AskpassFile -Encoding ASCII
    $env:SSH_ASKPASS = $script:AskpassFile
    $env:SSH_ASKPASS_REQUIRE = "force"
    $env:DISPLAY = "1"
}
function Remove-Askpass {
    if ($script:AskpassFile -and (Test-Path $script:AskpassFile)) {
        Remove-Item $script:AskpassFile -ErrorAction SilentlyContinue
    }
}

function Invoke-NasSSH {
    param([Parameter(Mandatory)][string]$RemoteCmd)
    $sshOpts = @("-p", "$script:NAS_PORT",
                 "-o", "StrictHostKeyChecking=accept-new",
                 "-o", "BatchMode=yes",
                 "-o", "ConnectTimeout=10")
    if ($Password) {
        $sshOpts += @("-o", "PreferredAuthentications=password",
                      "-o", "PubkeyAuthentication=no",
                      "-o", "NumberOfPasswordPrompts=1")
    }
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        & ssh @sshOpts "$script:NAS_USER@$script:NAS_HOST" $RemoteCmd 2>&1 | Out-Host
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prevEAP
    }
    return $code
}

Resolve-NasEndpoint

function Show-Menu {
    Write-Host ""
    Write-Host "========== NAS SSH 工具箱 ($script:NAS_USER@$script:NAS_HOST`:$script:NAS_PORT) ==========" -ForegroundColor Cyan
    Write-Host "  1) 服务状态        docker compose ps"
    Write-Host "  2) 查看日志        docker compose logs -f api"
    Write-Host "  3) 容器列表        docker ps"
    Write-Host "  4) 磁盘占用        df -h + data 目录占用"
    Write-Host "  5) 转码缓存        .transcode-cache 占用"
    Write-Host "  6) 清理转码缓存    删除 N 天前的缓存"
    Write-Host "  7) 健康检查        curl API /health"
    Write-Host "  8) 自定义命令      输入任意 NAS 命令"
    Write-Host "  9) 退出"
    Write-Host "================================================" -ForegroundColor Cyan
}

function Invoke-MenuItem {
    param([int]$Choice)
    switch ($Choice) {
        1 { Invoke-NasSSH "cd '$REMOTE_PATH' && docker compose ps" }
        2 { Invoke-NasSSH "cd '$REMOTE_PATH' && docker compose logs -f api" }
        3 { Invoke-NasSSH "docker ps" }
        4 { Invoke-NasSSH "df -h /vol1; echo '---'; du -sh '$REMOTE_PATH/data' 2>/dev/null; du -sh '$REMOTE_PATH/data/frigate/recordings' 2>/dev/null" }
        5 { Invoke-NasSSH "du -sh '$REMOTE_PATH/data/recordings/.transcode-cache' 2>/dev/null; echo '---'; find '$REMOTE_PATH/data/recordings/.transcode-cache' -name '*.mp4' 2>/dev/null | wc -l" }
        6 {
            $days = Read-Host "删除几天前的缓存 (默认 7)"
            if ([string]::IsNullOrWhiteSpace($days)) { $days = "7" }
            Invoke-NasSSH "find '$REMOTE_PATH/data/recordings/.transcode-cache' -name '*.mp4' -mtime +$days -delete 2>/dev/null; echo '清理完成'; du -sh '$REMOTE_PATH/data/recordings/.transcode-cache' 2>/dev/null"
        }
        7 { Invoke-NasSSH "curl -s http://localhost:8080/health; echo" }
        8 {
            $cmd = Read-Host "输入 NAS 命令"
            if ($cmd) { Invoke-NasSSH $cmd }
        }
        9 { Write-Host "再见" -ForegroundColor Green; exit 0 }
        default { Write-Host "无效选项" -ForegroundColor Red }
    }
}

# ---- Single-shot command mode ----
if ($Command) {
    $exitCode = Invoke-NasSSH $Command
    Remove-Askpass
    exit $exitCode
}

# ---- Interactive menu mode ----
while ($true) {
    Show-Menu
    $choice = Read-Host "请选择"
    if ($choice -match '^\d+$') {
        Invoke-MenuItem ([int]$choice)
    } else {
        Write-Host "请输入数字" -ForegroundColor Red
    }
}
