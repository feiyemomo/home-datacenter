# push-apk.ps1 — Build the Android Gradle project and push the APK to the NAS.
#
# Native Windows 11 tools only: ssh, scp, gradlew (via the repo's wrapper).
#
# Workflow:
#   1. Build the debug APK (`gradlew.bat assembleDebug`)
#   2. Copy it to a versioned name `app-debug-v<versionName>.apk`
#   3. Write a `release-notes-v<versionName>.txt` (optional, via -Notes)
#   4. scp both to NAS `data/releases/`
#   5. Print the pushed files
#
# Version comes from app/build.gradle.kts (versionName). NAS host/user/path
# mirror deploy-nas.ps1.
#
# Usage:
#   .\android\push-apk.ps1                        # build + push, no release notes
#   .\android\push-apk.ps1 -Notes "v1.8.44 ..."   # build + push + write release notes
#   .\android\push-apk.ps1 -DryRun                # show what would be pushed, don't build/touch NAS
#   .\android\push-apk.ps1 -Password '<YOUR_PASSWORD>' # password auth (no SSH key needed)

[CmdletBinding()]
param(
    # Optional release-notes text. When set, a release-notes-v<version>.txt
    # is written locally and pushed alongside the APK.
    [string]$Notes,
    [switch]$DryRun,
    [switch]$BuildOnly,   # build + version-rename locally, but do NOT push to NAS
    # NAS password; when set, uses SSH_ASKPASS to feed it non-interactively.
    [string]$Password,
    # Build flavor to push: "release" (default, official keystore
    # signed) or "debug". Release chooses app-release.apk, assembles via assembleRelease.
    [ValidateSet("debug", "release")]
    [string]$Flavor = "release",
    # Force acquiring an H3C Oasis Cloud relay tunnel even if LAN is reachable.
    [switch]$ForceH3C,
    # Keep the H3C relay tunnel alive after script completes (do not auto-delete).
    [switch]$KeepTunnel
)

$ErrorActionPreference = "Stop"

# ---- helpers ---------------------------------------------------------------
function Write-Step([string]$t) { Write-Host ""; Write-Host $t -ForegroundColor Cyan }
function Write-Ok([string]$t) { Write-Host $t -ForegroundColor Green }
function Write-Err([string]$t) { Write-Host $t -ForegroundColor Red }

# ---- config (mirror deploy-nas.ps1) ----------------------------------------
$NAS_HOST   = "100.90.67.71"
$NAS_USER   = "momo"
$NAS_PORT   = 22
$REMOTE_DIR = "/vol1/docker/home-datacenter/data/releases"

$RepoRoot   = Split-Path -Parent $PSScriptRoot
$AndroidDir = Join-Path $RepoRoot "android"

# ---- H3C Relay & Endpoint Management ----
$RelayScript = Join-Path $RepoRoot "tools\h3c_relay.py"
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

trap { Cleanup-RelayTunnel; break }
Register-EngineEvent PowerShell.Exiting -Action { Cleanup-RelayTunnel } | Out-Null

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
$GradleFile = Join-Path $AndroidDir "app\build.gradle.kts"

Set-Location $AndroidDir

# ---- sanity checks ----------------------------------------------------------
foreach ($cmd in @("ssh", "scp")) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
        Write-Err "ERROR: $cmd not found on PATH. Windows 11 ships ssh/scp by default."
        exit 1
    }
}
if (-not (Test-Path $GradleFile)) {
    Write-Err "ERROR: app/build.gradle.kts not found under $AndroidDir — run from the repo root."
    exit 1
}

# --- read versionName + versionCode from build.gradle.kts --------------------
$gradleText = Get-Content $GradleFile -Raw
$versionName = [regex]::Match($gradleText, 'versionName\s*=\s*"([^"]+)"').Groups[1].Value
$versionCode = [regex]::Match($gradleText, 'versionCode\s*=\s*(\d+)').Groups[1].Value
if (-not $versionName) {
    Write-Err "ERROR: could not read versionName from $GradleFile."
    exit 1
}
Write-Step "Version: $versionName (versionCode $versionCode)"

# --- build -------------------------------------------------------------------
if (-not $DryRun) {
    $buildTask = if ($Flavor -eq "release") { "assembleRelease" } else { "assembleDebug" }
    Write-Step "==> Building ${Flavor} APK (gradlew.bat $buildTask)"
    & "$AndroidDir\gradlew.bat" $buildTask --console=plain
    if ($LASTEXITCODE -ne 0) {
        Write-Err "ERROR: Gradle build failed (exit $LASTEXITCODE)."
        exit 1
    }
}

$srcApk  = Join-Path $AndroidDir "app\build\outputs\apk\$Flavor\app-$Flavor.apk"
if (-not (Test-Path $srcApk)) {
    Write-Err "ERROR: build output not found: $srcApk"
    exit 1
}

# --- versioned rename ---------------------------------------------------------
$versionedApk   = Join-Path $AndroidDir "app-$Flavor-v$versionName.apk"
$versionedNotes = Join-Path $AndroidDir "release-notes-v$versionName.txt"

if (-not $DryRun) {
    Copy-Item $srcApk $versionedApk -Force
    Write-Ok "APK -> $versionedApk"
}

# --- optional release notes ---------------------------------------------------
if ($Notes -and -not $DryRun) {
    $notesBody = "v$versionName (versionCode $versionCode)`r`n`r`n$Notes"
    Set-Content -Path $versionedNotes -Value $notesBody -Encoding UTF8
    Write-Ok "Notes -> $versionedNotes"
}

# --- push ---------------------------------------------------------------------
if ($BuildOnly -or $DryRun) {
    if ($DryRun) {
        Write-Host ""
        Write-Host "DRY RUN: would scp to $NAS_USER@$NAS_HOST`:$REMOTE_DIR" -ForegroundColor Yellow
        Write-Host "  - $versionedApk" -ForegroundColor Yellow
        if ($Notes) { Write-Host "  - $versionedNotes" -ForegroundColor Yellow }
    }
    Write-Ok "Done (local only)."
    exit 0
}

# Resolve fastest NAS endpoint (LAN -> H3C Relay -> Tailscale)
Resolve-NasEndpoint

# password auth via SSH_ASKPASS
$script:AskpassFile = ""
if ($Password) {
    $script:AskpassFile = [System.IO.Path]::GetTempFileName() + "-askpass.bat"
    "@echo $Password" | Set-Content $script:AskpassFile -Encoding ASCII
    $env:SSH_ASKPASS = $script:AskpassFile
    $env:SSH_ASKPASS_REQUIRE = "force"
    $env:DISPLAY = "1"
}
try {
    Write-Step "==> Pushing to NAS ($script:NAS_USER@$script:NAS_HOST`:$REMOTE_DIR)"
    $target = "${script:NAS_USER}@${script:NAS_HOST}:${REMOTE_DIR}/"
    $scpArgs = @("-P", "$script:NAS_PORT", "-C",
                 "-o", "ConnectTimeout=10",
                 "-o", "BatchMode=yes",
                 "-o", "StrictHostKeyChecking=accept-new")
    if ($Notes -and (Test-Path $versionedNotes)) {
        & scp @scpArgs $versionedApk $versionedNotes $target
    } else {
        & scp @scpArgs $versionedApk $target
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Err "ERROR: scp failed (exit $LASTEXITCODE)."
        Cleanup-RelayTunnel
        exit 1
    }
}
finally {
    if ($script:AskpassFile -and (Test-Path $script:AskpassFile)) {
        Remove-Item $script:AskpassFile -Force
    }
}

Write-Ok "==> Pushed app-$Flavor-v$versionName.apk to NAS."
if ($Notes) { Write-Ok "==> Pushed release-notes-v$versionName.txt to NAS." }
Cleanup-RelayTunnel
exit 0