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
#   .\android\push-apk.ps1 -Password '<NAS_PASSWORD>'   # password auth (no SSH key needed)

[CmdletBinding()]
param(
    # Optional release-notes text. When set, a release-notes-v<version>.txt
    # is written locally and pushed alongside the APK.
    [string]$Notes,
    [switch]$DryRun,
    [switch]$BuildOnly,   # build + version-rename locally, but do NOT push to NAS
    # NAS password; when set, uses SSH_ASKPASS to feed it non-interactively.
    [string]$Password
)

$ErrorActionPreference = "Stop"

# ---- helpers ---------------------------------------------------------------
function Write-Step([string]$t) { Write-Host ""; Write-Host $t -ForegroundColor Cyan }
function Write-Ok([string]$t) { Write-Host $t -ForegroundColor Green }
function Write-Err([string]$t) { Write-Host $t -ForegroundColor Red }

# ---- config (mirror deploy-nas.ps1) ----------------------------------------
$NAS_HOST   = "192.168.31.235"
$NAS_USER   = "fnos-momo"
$NAS_PORT   = 22
$REMOTE_DIR = "/vol1/docker/home-datacenter/data/releases"

$RepoRoot   = Split-Path -Parent $PSScriptRoot
$AndroidDir = Join-Path $RepoRoot "android"
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
    Write-Step "==> Building debug APK (gradlew.bat assembleDebug)"
    & "$AndroidDir\gradlew.bat" assembleDebug --console=plain
    if ($LASTEXITCODE -ne 0) {
        Write-Err "ERROR: Gradle build failed (exit $LASTEXITCODE)."
        exit 1
    }
}

$srcApk  = Join-Path $AndroidDir "app\build\outputs\apk\debug\app-debug.apk"
if (-not (Test-Path $srcApk)) {
    Write-Err "ERROR: build output not found: $srcApk"
    exit 1
}

# --- versioned rename ---------------------------------------------------------
$versionedApk   = Join-Path $AndroidDir "app-debug-v$versionName.apk"
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
    Write-Step "==> Pushing to NAS ($NAS_USER@$NAS_HOST`:$REMOTE_DIR)"
    $target = "${NAS_USER}@${NAS_HOST}:${REMOTE_DIR}/"
    if ($Notes -and (Test-Path $versionedNotes)) {
        scp -o ConnectTimeout=10 -P $NAS_PORT $versionedApk $versionedNotes $target
    } else {
        scp -o ConnectTimeout=10 -P $NAS_PORT $versionedApk $target
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Err "ERROR: scp failed (exit $LASTEXITCODE)."
        exit 1
    }
}
finally {
    if ($script:AskpassFile -and (Test-Path $script:AskpassFile)) {
        Remove-Item $script:AskpassFile -Force
    }
}

Write-Ok "==> Pushed app-debug-v$versionName.apk to NAS."
if ($Notes) { Write-Ok "==> Pushed release-notes-v$versionName.txt to NAS." }