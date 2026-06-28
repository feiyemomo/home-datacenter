# Home Datacenter WebSocket test script
# Usage:
#   $key = "<your-access-key>"
#   .\scripts\test_ws.ps1

param(
    [Parameter(Mandatory=$true)]
    [string]$AccessKey,

    [string]$BaseUrl = "http://localhost:8080"
)

$ErrorActionPreference = "Stop"

# 1. Bind to get JWT
Write-Host "[1/4] Binding device..." -ForegroundColor Cyan
$body = @{ user_id = 1; access_key = $AccessKey } | ConvertTo-Json
$resp = Invoke-RestMethod -Uri "$BaseUrl/api/v1/auth/bind" -Method POST -Body $body -ContentType "application/json"
$token = $resp.data.token
Write-Host "  Token: $($token.Substring(0,20))..." -ForegroundColor Green

# 2. Connect WebSocket
Write-Host "[2/4] Connecting WebSocket..." -ForegroundColor Cyan
$ws = New-Object System.Net.WebSockets.ClientWebSocket
# Use a long-lived token (no timeout) so the connection stays open.
$cts = New-Object System.Threading.CancellationTokenSource
$wsUri = $BaseUrl.Replace("http://", "ws://").Replace("https://", "wss://") + "/api/v1/ws?token=$token"
$ws.ConnectAsync([Uri]$wsUri, $cts.Token).Wait()
Write-Host "  State: $($ws.State)" -ForegroundColor Green

if ($ws.State -ne 'Open') {
    Write-Host "  FAILED to connect" -ForegroundColor Red
    exit 1
}

# 3. Receive initial online_list message
Write-Host "[3/4] Receiving initial message..." -ForegroundColor Cyan
$buf = New-Object byte[] 4096
$rseg = [ArraySegment[byte]]::new($buf)
$task = $ws.ReceiveAsync($rseg, $cts.Token)
$task.Wait()
$msg = [Text.Encoding]::UTF8.GetString($buf, 0, $task.Result.Count)
Write-Host "  RX: $msg" -ForegroundColor Yellow

# 4. Send heartbeat and receive ack
Write-Host "[4/4] Sending heartbeat..." -ForegroundColor Cyan
$hb = '{"type":"heartbeat"}'
$bytes = [Text.Encoding]::UTF8.GetBytes($hb)
$sseg = [ArraySegment[byte]]::new($bytes)
$ws.SendAsync($sseg, [System.Net.WebSockets.WebSocketMessageType]::Text, $true, $cts.Token).Wait()
Write-Host "  TX: $hb" -ForegroundColor Gray

# Receive ack
$task2 = $ws.ReceiveAsync($rseg, $cts.Token)
$task2.Wait()
$msg2 = [Text.Encoding]::UTF8.GetString($buf, 0, $task2.Result.Count)
Write-Host "  RX: $msg2" -ForegroundColor Yellow

# 5. Subscribe to a topic
Write-Host "Subscribing to device.1..." -ForegroundColor Cyan
$sub = '{"type":"subscribe","topic":"device.1"}'
$bytes2 = [Text.Encoding]::UTF8.GetBytes($sub)
$sseg2 = [ArraySegment[byte]]::new($bytes2)
$ws.SendAsync($sseg2, [System.Net.WebSockets.WebSocketMessageType]::Text, $true, $cts.Token).Wait()
Write-Host "  TX: $sub" -ForegroundColor Gray

Write-Host "`nDone. WebSocket is open. Press Ctrl+C to exit." -ForegroundColor Green
Write-Host "Listening for incoming messages..." -ForegroundColor Cyan

# Keep listening for messages.
# Poll the receive task with short waits so we can interleave heartbeat sends.
Write-Host "`nListening for messages (Ctrl+C to exit)..." -ForegroundColor Cyan
Write-Host ""

$heartbeatTimer = [System.Diagnostics.Stopwatch]::StartNew()
try {
    while ($ws.State -eq 'Open') {
        # Start a single receive operation (only one outstanding at a time).
        $task3 = $ws.ReceiveAsync($rseg, $cts.Token)

        # Poll with 1-second sleeps so we can send heartbeats while waiting.
        while (-not $task3.IsCompleted -and $ws.State -eq 'Open') {
            Start-Sleep -Milliseconds 1000

            if ($heartbeatTimer.Elapsed.TotalSeconds -ge 25) {
                $heartbeatTimer.Restart()
                $hbBytes = [Text.Encoding]::UTF8.GetBytes('{"type":"heartbeat"}')
                $hbSeg = [ArraySegment[byte]]::new($hbBytes)
                try {
                    $ws.SendAsync($hbSeg, [System.Net.WebSockets.WebSocketMessageType]::Text, $true, $cts.Token).Wait()
                    $ts = Get-Date -Format "HH:mm:ss"
                    Write-Host "  [$ts] TX: heartbeat" -ForegroundColor DarkGray
                } catch {
                    Write-Host "  Heartbeat failed: $($_.Exception.Message)" -ForegroundColor Red
                    break
                }
            }
        }

        if ($ws.State -ne 'Open') { break }

        if ($task3.Result.Count -gt 0) {
            $msg3 = [Text.Encoding]::UTF8.GetString($buf, 0, $task3.Result.Count)
            $ts = Get-Date -Format "HH:mm:ss"
            Write-Host "  [$ts] RX: $msg3" -ForegroundColor Yellow
        }
    }
} catch {
    Write-Host "`nConnection closed: $($_.Exception.Message)" -ForegroundColor Yellow
} finally {
    $heartbeatTimer.Stop()
    if ($ws.State -eq 'Open') {
        try { $ws.CloseAsync([System.Net.WebSockets.WebSocketCloseStatus]::NormalClosure, "bye", $cts.Token).Wait() } catch {}
    }
    $ws.Dispose()
    Write-Host "Disconnected." -ForegroundColor Cyan
}
