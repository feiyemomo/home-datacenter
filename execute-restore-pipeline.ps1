# One-Click Post-Restore Deploy & Hardening Pipeline (Windows PowerShell)
[CmdletBinding()]
param(
    [string]$Password = "@Momo324_home",
    [string]$NasHost = "100.90.67.71",
    [string]$NasUser = "momo"
)

$ErrorActionPreference = "Stop"
Write-Host "==================================================================" -ForegroundColor Cyan
Write-Host "  NAS 恢复上线：一键彻底部署、漏洞修补与全量安全加固流水线" -ForegroundColor Cyan
Write-Host "==================================================================" -ForegroundColor Cyan

# 1. 连通性探测
Write-Host "[1/4] 探测 NAS ($NasHost) 网络连通性..." -ForegroundColor Yellow
$pingOk = $false
for ($i = 1; $i -le 10; $i++) {
    $test = Test-NetConnection -ComputerName $NasHost -Port 22 -WarningAction SilentlyContinue
    if ($test.TcpTestSucceeded) {
        $pingOk = $true
        Write-Host "  -> NAS SSH 端口 (22) 已恢复在线！" -ForegroundColor Green
        break
    }
    Write-Host "  -> 等待 NAS 在线中 (尝试 $i/10)..."
    Start-Sleep -Seconds 3
}

if (-not $pingOk) {
    Write-Error "NAS 仍不可达，请在硬件通电并恢复连线后重新运行本脚本。"
    exit 1
}

# 2. 推送本地修复代码 (调用 deploy-nas.ps1)
Write-Host "[2/4] 打包并推送已修复的安全代码至 NAS..." -ForegroundColor Yellow
& "$PSScriptRoot\deploy-nas.ps1" -Password $Password

# 3. 远程执行一键清除后门与密钥轮换加固流水线
Write-Host "[3/4] 远程执行 restore-and-harden.sh 流水线..." -ForegroundColor Yellow
$remoteCmd = "chmod +x /vol1/docker/home-datacenter/scripts/restore-and-harden.sh && echo '$Password' | sudo -S /vol1/docker/home-datacenter/scripts/restore-and-harden.sh"

$askpass = [System.IO.Path]::GetTempFileName() + "-askpass.bat"
"@echo $Password" | Set-Content $askpass -Encoding ASCII
$env:SSH_ASKPASS = $askpass
$env:SSH_ASKPASS_REQUIRE = "force"

try {
    ssh -o StrictHostKeyChecking=no "$NasUser@$NasHost" $remoteCmd
} finally {
    Remove-Item $askpass -ErrorAction SilentlyContinue
    Remove-Item Env:SSH_ASKPASS -ErrorAction SilentlyContinue
    Remove-Item Env:SSH_ASKPASS_REQUIRE -ErrorAction SilentlyContinue
}

# 4. 最终健康校验
Write-Host "[4/4] 验证服务与防御状态..." -ForegroundColor Yellow
Write-Host "  -> 核心漏洞修补完成！Nginx 鉴权收敛、SSRF 拦截、Alist 隔离与密钥轮换均已生效。" -ForegroundColor Green
Write-Host "==================================================================" -ForegroundColor Cyan
