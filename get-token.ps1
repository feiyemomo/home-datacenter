# Home Datacenter — get test JWT token
# Usage:
#   .\get-token.ps1                          # LAN (default)
#   .\get-token.ps1 -BaseUrl "https://dashboard.feiyemomo.top"  # relay
#   .\get-token.ps1 -Copy                    # copy to clipboard
#   . .\get-token.ps1                        # dot-source to export $env:HC_TOKEN
#                                             # into the current shell
#
# Uses the built-in test AccessKey (user_id=1). The token is long-lived
# (365 days, matching the JWT exp claim) and is intended for local
# testing against the Home Datacenter API. The token is:
#   - printed to stdout              (pipe-friendly)
#   - stored in $env:HC_TOKEN        (so subsequent API calls in the
#                                     same shell can reuse it; requires
#                                     dot-sourcing to escape this script's
#                                     scope)
#   - copied to the clipboard        (when -Copy is given)

[CmdletBinding()]
param(
    [string]$BaseUrl = "http://192.168.1.3:8080",
    [switch]$Copy
)

$ErrorActionPreference = "Stop"

# ---- Built-in test AccessKey (do NOT use in production) ----
$UserId    = 1
$AccessKey = "ebc94f7fe99b497a9bdec7bd45add929360e4386be3cad96a8b3922d1d680a05"

$bindUrl = "$BaseUrl/api/v1/auth/bind"

Write-Host "==> Requesting JWT from $bindUrl" -ForegroundColor Cyan
Write-Host "    user_id:    $UserId"

$body = @{ user_id = $UserId; access_key = $AccessKey } | ConvertTo-Json -Compress

try {
    $resp = Invoke-RestMethod -Uri $bindUrl `
                              -Method POST `
                              -Body $body `
                              -ContentType "application/json"
} catch {
    Write-Host "ERROR: token request failed." -ForegroundColor Red
    # Invoke-RestMethod drops the response body into $_.ErrorDetails.Message
    # (when the server returns JSON) or $_.Exception.Message otherwise.
    if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
        Write-Host "    Server response: $($_.ErrorDetails.Message)" -ForegroundColor Red
    } else {
        Write-Host "    $($_.Exception.Message)" -ForegroundColor Red
    }
    Write-Host "    URL: $bindUrl" -ForegroundColor Red
    exit 1
}

# Response envelope: { code: 0, message: "success", data: { token: "..." } }
$token = $resp.data.token
if (-not $token) {
    Write-Host "ERROR: response did not contain a token." -ForegroundColor Red
    Write-Host "    Raw response: $($resp | ConvertTo-Json -Compress)" -ForegroundColor Red
    exit 1
}

# Export to the environment so subsequent API calls in the same shell
# can use it (e.g. `Invoke-RestMethod -Headers @{ Authorization = "Bearer $env:HC_TOKEN" }`).
# Note: only persists in the caller's shell if this script is dot-sourced.
$env:HC_TOKEN = $token

Write-Host "==> Token acquired." -ForegroundColor Green
# Print the token to stdout. Plain Write-Output (not Write-Host) so the
# token can be captured via piping: $t = .\get-token.ps1
Write-Output $token

if ($Copy) {
    try {
        Set-Clipboard -Value $token
        Write-Host "==> Token copied to clipboard." -ForegroundColor Green
    } catch {
        Write-Host "WARNING: failed to copy to clipboard: $($_.Exception.Message)" -ForegroundColor Red
    }
}
