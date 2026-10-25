param(
    [int]$ListenPort = 8888,
    [switch]$Background
)

$script = "D:\Projects\home-datacenter\tools\dsh_remote_proxy.js"

if (-not (Test-Path $script)) {
    Write-Error "Proxy script not found: $script"
    exit 1
}

# Stop existing proxy on that port if running
$existing = Get-NetTCPConnection -LocalPort $ListenPort -State Listen -ErrorAction SilentlyContinue
if ($existing) {
    Write-Host "Stopping existing proxy on port $ListenPort (PID: $($existing.OwningProcess))..." -ForegroundColor Yellow
    Stop-Process -Id $existing.OwningProcess -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1
}

if ($Background) {
    Write-Host "Starting DSH Remote Proxy in background on port $ListenPort..." -ForegroundColor Cyan
    Start-Process -FilePath "node" -ArgumentList "`"$script`"" -WindowStyle Hidden
    Start-Sleep -Seconds 1
    Write-Host "[OK] Proxy started in background." -ForegroundColor Green
    Write-Host "Tailscale: http://100.99.78.100:$ListenPort"
    Write-Host "LAN:       http://192.168.31.51:$ListenPort"
} else {
    Write-Host "Starting DSH Remote Proxy on port $ListenPort (Ctrl+C to exit)..." -ForegroundColor Cyan
    & node "$script"
}
