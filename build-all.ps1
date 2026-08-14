# build-all.ps1 — Orchestrate build + deploy for all three home-datacenter tiers.
#
# Each tier keeps its own toolchain & build system; this script is a thin
# orchestrator that runs them in order and fails fast on error. Nothing is
# cross-compiled into Gradle — Gradle stays Android-only (see README §构建).
#
#   Tier 1  Android client   → Gradle build in D:\Projects\Android
#                              + optional APK push to NAS releases dir
#   Tier 2  Go backend       → packaged by deploy-nas.ps1, built inside
#                              the NAS api Dockerfile (no local go build)
#   Tier 3  Web frontend     → packaged by deploy-nas.ps1, built inside
#                              the NAS web Dockerfile (no local npm build)
#
# Tiers 2 & 3 are deployed together by deploy-nas.ps1 (docker compose builds
# them on the NAS), so "server" is one step here.
#
# Usage:
#   .\build-all.ps1                          # build release APK + deploy NAS
#   .\build-all.ps1 -Flavor debug            # build debug APK instead
#   .\build-all.ps1 -PushApk                 # also push the APK to NAS releases
#   .\build-all.ps1 -PushApk -Notes "..."    # attach release notes
#   .\build-all.ps1 -Password '@Fnos324'     # NAS password auth (no SSH key)
#   .\build-all.ps1 -SkipAndroid             # only deploy backend + web
#   .\build-all.ps1 -SkipDeploy              # only build Android, no NAS
#   .\build-all.ps1 -DryRun                  # preview, touch nothing
#
# Exit codes: 0 = all steps succeeded; non-zero = first failing step.

[CmdletBinding()]
param(
    # Android flavor to build: "debug" (signed with project debug keystore)
    # or "release" (signed with the official release keystore).
    [ValidateSet("debug", "release")]
    [string]$Flavor = "release",

    # Push the built APK to the NAS releases directory (in-app updater wire).
    [switch]$PushApk,

    # Skips the Android Gradle build entirely.
    [switch]$SkipAndroid,

    # Skips the NAS deploy (backend + web) entirely.
    [switch]$SkipDeploy,

    # Release-notes text for the pushed APK (only used with -PushApk).
    [string]$Notes,

    # NAS password for deploy / push when no SSH key is installed.
    [string]$Password,

    # Preview commands without touching the NAS or running heavy builds.
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"
$RepoRoot = $PSScriptRoot
$AndroidRoot = "D:\Projects\Android"

if (-not (Test-Path $AndroidRoot)) {
    Write-Error "ERROR: Android project not found at $AndroidRoot"
    exit 1
}

# ---- Tier 1: Android client -----------------------------------------------
if (-not $SkipAndroid) {
    $task = "assemble" + $Flavor.Substring(0, 1).ToUpper() + $Flavor.Substring(1)
    Write-Host "==> [1/3] Android: gradlew $task (flavor=$Flavor)" -ForegroundColor Cyan
    if ($DryRun) {
        Write-Host "    (DRY RUN) would run: gradlew $task in $AndroidRoot" -ForegroundColor Yellow
    } else {
        Push-Location $AndroidRoot
        try {
            & ".\gradlew.bat" $task --no-daemon
            if ($LASTEXITCODE -ne 0) {
                Write-Error "ERROR: gradlew $task failed (exit $LASTEXITCODE)"
                exit 1
            }
        } finally {
            Pop-Location
        }
    }

    if ($PushApk) {
        Write-Host "==> [1b/3] Pushing $Flavor APK to NAS releases" -ForegroundColor Cyan
        # Hashtable splatting (not array) so the -DryRun switch is bound
        # correctly across the script call boundary.
        $pushArgs = @{ Flavor = $Flavor }
        if ($Notes) { $pushArgs.Notes = $Notes }
        if ($Password) { $pushArgs.Password = $Password }
        if ($DryRun) { $pushArgs.DryRun = $true }
        & "$AndroidRoot\push-apk.ps1" @pushArgs
        if ($LASTEXITCODE -ne 0) {
            Write-Error "ERROR: push-apk.ps1 failed (exit $LASTEXITCODE)"
            exit 1
        }
    } elseif (-not $DryRun) {
        Write-Host "    APK not pushed (add -PushApk to upload to NAS)." -ForegroundColor Yellow
    }
} else {
    Write-Host "==> [1/3] Android skipped (-SkipAndroid)" -ForegroundColor DarkGray
}

# ---- Tier 2+3: Go backend + Web frontend -----------------------------------
# deploy-nas.ps1 packages the whole repo (excluding data/.git/node_modules/...)
# and runs `docker compose up -d --build` on the NAS, which builds the Go API
# binary and the Web dist inside their Dockerfiles. Pass -NoBuild to deploy-nas
# only when compose.yaml/.env changed without source changes.
if (-not $SkipDeploy) {
    Write-Host "==> [2/3] Deploying Go backend + Web frontend to NAS" -ForegroundColor Cyan
    # Hashtable splatting so -DryRun is bound correctly across the call.
    $deployArgs = @{}
    if ($Password) { $deployArgs.Password = $Password }
    if ($DryRun) { $deployArgs.DryRun = $true }
    & "$RepoRoot\deploy-nas.ps1" @deployArgs
    if ($LASTEXITCODE -ne 0) {
        Write-Error "ERROR: deploy-nas.ps1 failed (exit $LASTEXITCODE)"
        exit 1
    }
} else {
    Write-Host "==> [2/3] NAS deploy skipped (-SkipDeploy)" -ForegroundColor DarkGray
}

Write-Host ""
Write-Host "==> [3/3] build-all complete." -ForegroundColor Green
exit 0