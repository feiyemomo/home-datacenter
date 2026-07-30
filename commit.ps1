# Home Datacenter — one-click git commit and push
# Usage:
#   .\commit.ps1                              # interactive
#   .\commit.ps1 -Message "fix: ..."          # non-interactive
#   .\commit.ps1 -DryRun                      # preview only
param(
    [string]$Message,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

# --- Helper functions for colored output ---

function Write-Step([string]$text) {
    Write-Host ""
    Write-Host $text -ForegroundColor Cyan
}

function Write-Ok([string]$text) {
    Write-Host $text -ForegroundColor Green
}

function Write-Warn([string]$text) {
    Write-Host $text -ForegroundColor Yellow
}

function Write-Err([string]$text) {
    Write-Host $text -ForegroundColor Red
}

# --- Step 1: Show git status ---

Write-Step "==> Step 1/6: Git status (git status --short)"
$statusOutput = & git status --short
if ($LASTEXITCODE -ne 0) {
    Write-Err "ERROR: 'git status' failed. Make sure you are inside a git repository."
    exit 1
}

if (-not $statusOutput) {
    Write-Warn "No changes detected. Nothing to commit."
    exit 0
}

Write-Host $statusOutput

# --- Step 2: Show diff stat ---

Write-Step "==> Step 2/6: Diff summary (git diff --stat HEAD)"
$diffStat = & git diff --stat HEAD
if ($LASTEXITCODE -ne 0) {
    Write-Err "ERROR: 'git diff --stat HEAD' failed."
    exit 1
}
if ($diffStat) {
    Write-Host $diffStat
} else {
    Write-Warn "(no diff output — changes may be untracked files only)"
}

# --- Step 3: Determine commit message ---

$commitMessage = $Message

if (-not $commitMessage) {
    Write-Step "==> Step 3/6: Enter commit message"
    $commitMessage = Read-Host "Enter commit message"
}

if ([string]::IsNullOrWhiteSpace($commitMessage)) {
    Write-Err "ERROR: Commit message is empty. Aborting."
    exit 1
}

Write-Host "Commit message: $commitMessage"

# --- DryRun short-circuit ---

if ($DryRun) {
    Write-Step "==> Dry run — no changes will be made"
    Write-Warn "DRY RUN: The following actions would be performed:"
    Write-Warn "  - git add -A"
    Write-Warn "  - git commit -m `"$commitMessage`""
    Write-Warn "  - git push"
    exit 0
}

# --- Step 4: Stage all changes ---

Write-Step "==> Step 4/6: Staging changes (git add -A)"
& git add -A
if ($LASTEXITCODE -ne 0) {
    Write-Err "ERROR: 'git add -A' failed."
    exit 1
}
Write-Ok "Staged."

# --- Step 5: Commit ---

Write-Step "==> Step 5/6: Committing (git commit -m ...)"
& git commit -m $commitMessage
if ($LASTEXITCODE -ne 0) {
    Write-Err "ERROR: 'git commit' failed."
    exit 1
}

# --- Step 6: Push ---
# Try with default config (may include a proxy). If it fails, retry without
# proxy in case the configured proxy cannot reach the remote (e.g. GitHub).

Write-Step "==> Step 6/6: Pushing (git push)"
& git push
$pushExitCode = $LASTEXITCODE
if ($pushExitCode -ne 0) {
    Write-Warn "WARNING: 'git push' failed (exit code $pushExitCode), possibly due to an unreachable proxy. Retrying without proxy..."
    & git -c http.proxy= -c https.proxy= push
    $retryExitCode = $LASTEXITCODE
    if ($retryExitCode -ne 0) {
        Write-Err "ERROR: 'git push' failed even without proxy (exit code $retryExitCode)."
        exit 1
    }
    Write-Ok "Pushed via direct connection (no proxy)."
}

# --- Confirmation ---

$commitHash = & git rev-parse --short HEAD
if ($LASTEXITCODE -ne 0) {
    Write-Err "ERROR: Could not retrieve commit hash."
    exit 1
}

Write-Host ""
Write-Ok "==> Commit $commitHash pushed successfully."
Write-Ok "Message: $commitMessage"
