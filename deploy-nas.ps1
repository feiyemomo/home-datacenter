# deploy-nas.ps1 — Push home-datacenter to the NAS and (re)deploy via docker compose.
#
# Native Windows 11 tools only: ssh, scp, tar (bsdtar 3.x), PowerShell.
# No rsync, no WSL, no Git Bash, no scoop install needed.
#
# Workflow:
#   1. tar project → temp .tar.gz (with excludes; data/.git/node_modules/.env ...)
#   2. scp tarball to NAS
#   3. SSH: extract tarball on NAS, remove tarball
#   4. Verify .env exists on NAS (refuse to deploy if missing — would
#      silently use empty JWT_SECRET and break auth)
#   5. SSH: docker compose up -d --build
#   6. Print service status
#
# Prerequisites (one-time setup):
#   1. SSH key auth — run this script with -SetupKeys once to install
#      your public key on the NAS:
#        .\deploy-nas.ps1 -SetupKeys
#      Until you do this, every ssh/scp call below will prompt for the
#      NAS password interactively.
#   2. docker compose plugin (v2) on the NAS. fnos ships Docker; verify:
#        ssh -p 22 fnos-momo@192.168.31.234 'docker compose version'
#
# First-time deploy only:
#   The script refuses to overwrite the NAS's .env (each environment
#   must have its own JWT_SECRET + MQTT_PASSWORD). Create it manually
#   on the NAS before the first deploy:
#        ssh -p 22 fnos-momo@192.168.31.234
#        cd /vol1/docker/home-datacenter
#        cp .env.example .env
#        # edit .env: JWT_SECRET=$(openssl rand -hex 32), MQTT_PASSWORD=...
#        # regenerate mosquitto passwd to match (see README §2)
#   Subsequent deploys just re-run .\deploy-nas.ps1.
#
# Usage:
#   .\deploy-nas.ps1                          # normal deploy (requires SSH key auth)
#   .\deploy-nas.ps1 -Password '@Fnos324'     # use password auth (no SSH key needed)
#   .\deploy-nas.ps1 -DryRun                  # show what would be packed, don't touch NAS
#   .\deploy-nas.ps1 -NoBuild                 # docker compose up -d without --build
#                                             # (use when only compose.yaml/.env changed)
#   .\deploy-nas.ps1 -Logs                    # after deploy, tail `api` logs (Ctrl-C to exit)
#   .\deploy-nas.ps1 -SetupKeys               # one-time: install SSH public key on NAS
#                                             # (use -Password to avoid interactive prompt
#                                             #  during key installation)

[CmdletBinding()]
param(
    [switch]$DryRun,
    [switch]$NoBuild,
    [switch]$Logs,
    [switch]$SetupKeys,
    # NAS password. When set, uses SSH_ASKPASS to feed the password to
    # ssh/scp non-interactively (Windows OpenSSH has no sshpass). When
    # omitted, falls back to default ssh behavior (publickey, or
    # interactive password prompt if no key is installed).
    [string]$Password
)

# ============== CONFIG (edit to match your NAS) ==============
$NAS_HOST   = "192.168.31.234"
$NAS_USER   = "fnos-momo"
$NAS_PORT   = 22
$REMOTE_PATH = "/vol1/docker/home-datacenter"
# =============================================================

$ErrorActionPreference = "Stop"
$ProjectRoot = $PSScriptRoot
Set-Location $ProjectRoot

# ---- Sanity checks: all three ship with Windows 11 by default ----
foreach ($cmd in @("ssh", "scp", "tar")) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
        Write-Error "ERROR: $cmd not found on PATH. Windows 11 ships ssh/scp/tar by default — enable OpenSSH Client via Settings > Apps > Optional Features > Add 'OpenSSH Client'."
        exit 1
    }
}
if (-not (Test-Path "compose.yaml")) {
    Write-Error "ERROR: compose.yaml not found in $ProjectRoot — run from the repo root."
    exit 1
}

# ---- Password auth setup (SSH_ASKPASS mechanism) ----
# Windows OpenSSH has no sshpass. When -Password is given, we create a
# tiny askpass batch script that echoes the password, and point
# SSH_ASKPASS + SSH_ASKPASS_REQUIRE=force at it. This makes ssh/scp
# non-interactive (no TTY prompt) so they work inside scripts/CI.
# The askpass file is deleted on exit. If -Password is omitted, ssh
# uses its default behavior (publickey, then interactive password).
$script:AskpassFile = ""
if ($Password) {
    $script:AskpassFile = [System.IO.Path]::GetTempFileName() + "-askpass.bat"
    # Batch file that echoes the password. The %1 argument is the
    # prompt string ssh passes (e.g. "fnos-momo@192.168.31.234's password:").
    "@echo $Password" | Set-Content $script:AskpassFile -Encoding ASCII
    $env:SSH_ASKPASS = $script:AskpassFile
    $env:SSH_ASKPASS_REQUIRE = "force"
    $env:DISPLAY = "1"  # some OpenSSH builds require DISPLAY to be set
}
function Remove-Askpass {
    if ($script:AskpassFile -and (Test-Path $script:AskpassFile)) {
        Remove-Item $script:AskpassFile -ErrorAction SilentlyContinue
    }
}
# Always clean up askpass on exit (normal exit, Ctrl-C, uncaught error).
trap { Remove-Askpass; break }
Register-EngineEvent PowerShell.Exiting -Action { Remove-Askpass } | Out-Null

# ---- SSH/SCP wrappers (apply common options consistently) ----
# Common options:
#   -o StrictHostKeyChecking=no  — don't fail on first connect (no known_hosts entry)
#   -o UserKnownHostsFile=NUL    — don't pollute known_hosts with the NAS entry
#   -o ConnectTimeout=10         — fail fast if NAS is unreachable
# When $Password is set, also force password auth and disable pubkey
# (avoids "Permission denied (publickey)" when no key is installed).
function Invoke-NasSSH {
    param([Parameter(Mandatory)][string]$RemoteCmd)
    $sshOpts = @("-p", "$NAS_PORT",
                 "-o", "StrictHostKeyChecking=no",
                 "-o", "UserKnownHostsFile=NUL",
                 "-o", "ConnectTimeout=10")
    if ($Password) {
        $sshOpts += @("-o", "PreferredAuthentications=password",
                      "-o", "PubkeyAuthentication=no",
                      "-o", "NumberOfPasswordPrompts=1")
    }
    # Temporarily relax ErrorActionPreference: ssh's stderr (e.g.
    # "Warning: Permanently added ... to known hosts") is treated as a
    # terminating error under "Stop" mode, which prevents $LASTEXITCODE
    # from being checked. We rely on explicit exit-code checks instead.
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        & ssh @sshOpts "$NAS_USER@$NAS_HOST" $RemoteCmd 2>&1 | Out-Host
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prevEAP
    }
    return $code
}

function Invoke-NasSCP {
    param([Parameter(Mandatory)][string]$LocalPath,
          [Parameter(Mandatory)][string]$RemoteDest)
    $scpOpts = @("-P", "$NAS_PORT",
                 "-o", "StrictHostKeyChecking=no",
                 "-o", "UserKnownHostsFile=NUL",
                 "-o", "ConnectTimeout=10")
    if ($Password) {
        $scpOpts += @("-o", "PreferredAuthentications=password",
                      "-o", "PubkeyAuthentication=no",
                      "-o", "NumberOfPasswordPrompts=1")
    }
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        & scp @scpOpts $LocalPath $RemoteDest 2>&1 | Out-Host
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prevEAP
    }
    return $code
}

# ---- SSH key setup (one-time, -SetupKeys flag) ----
if ($SetupKeys) {
    Write-Host "==> Setting up SSH key auth to $NAS_USER@$NAS_HOST" -ForegroundColor Cyan
    $keyPath = "$env:USERPROFILE\.ssh\id_ed25519"
    if (-not (Test-Path $keyPath)) {
        Write-Host "    Generating ed25519 key pair..."
        ssh-keygen -t ed25519 -f $keyPath -N '""' | Out-Null
    }
    $pubKey = Get-Content "$keyPath.pub"
    if ($Password) {
        Write-Host "    Installing public key on NAS (using -Password for auth)..."
    } else {
        Write-Host "    Installing public key on NAS (will prompt for NAS password one last time)..."
    }
    # Windows OpenSSH has no ssh-copy-id; do it manually.
    $installCmd = "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '$pubKey' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
    $exitCode = Invoke-NasSSH -RemoteCmd $installCmd
    if ($exitCode -ne 0) {
        Write-Error "ERROR: failed to install SSH key. Check NAS password / network."
        Remove-Askpass
        exit 1
    }
    Write-Host "==> SSH key installed. Test with: ssh -p $NAS_PORT $NAS_USER@$NAS_HOST 'echo ok'" -ForegroundColor Green
    Write-Host "    Subsequent deploys won't prompt for password."
    Remove-Askpass
    exit 0
}

$BuildFlag = if ($NoBuild) { "" } else { "--build" }

# ---- Excluded paths ----
# data/           — runtime data (sqlite, mosquitto state, frigate recordings); NAS has its own
# .git/           — VCS metadata, not needed for deploy
# node_modules/   — rebuilt inside the web Dockerfile
# build-host/     — forked go2rtc source, no longer used (Frigate is a prebuilt image)
# *.log / log_*.txt — build/iteration logs
# .env            — each env has its own secrets; MUST NOT overwrite
# *.py / __pycache__/ — ad-hoc diagnostic scripts (keep them local)
# .vscode/ .idea/ .DS_Store Thumbs.db — IDE / OS cruft
# .token.txt      — ad-hoc JWT, never leave the dev machine
# services/api/*.exe / server — Go build artifacts
# web/dist/ web/tsconfig.tsbuildinfo — web build cache
$Excludes = @(
    "./data", "./.git", "./build-host",
    "./web/node_modules", "./web/dist",
    "./.env", "./.env.local",
    "./.token.txt", "./sdp_offer.txt",
    "./.vscode", "./.idea",
    ".DS_Store", "Thumbs.db",
    "*.log", "log_*.txt",
    "*.py", "__pycache__",
    "*.exe", "./services/api/server",
    "./web/tsconfig.tsbuildinfo", "./web/tsconfig.node.tsbuildinfo",
    # mosquitto passwd is environment-specific (each env has its own
    # MQTT_PASSWORD). Must NOT overwrite the NAS's passwd file — same
    # rationale as excluding .env. Generate on the NAS with:
    #   docker run --rm -v ./deploy/mosquitto:/work eclipse-mosquitto:2 \
    #     mosquitto_passwd -c -b /work/passwd home-datacenter <password>
    "./deploy/mosquitto/passwd"
)

# ---- Dry run: pack + list, don't touch NAS ----
# Evaluated BEFORE the SSH steps so -DryRun works without a reachable
# NAS (useful for previewing what would be transferred).
if ($DryRun) {
    Write-Host "==> (DRY RUN) Packing project to preview" -ForegroundColor Cyan
    $tmpTar = [System.IO.Path]::GetTempFileName() + ".tar.gz"
    $listArgs = @("-czf", $tmpTar, "-C", $ProjectRoot)
    foreach ($ex in $Excludes) { $listArgs += @("--exclude", $ex) }
    $listArgs += "."
    & tar @listArgs 2>&1 | Out-Null
    $files = tar -tzf $tmpTar 2>$null
    $sizeMB = [math]::Round((Get-Item $tmpTar).Length / 1MB, 2)
    Remove-Item $tmpTar -ErrorAction SilentlyContinue
    Write-Host "    Would pack $($files.Count) files ($sizeMB MB)" -ForegroundColor Yellow
    Write-Host "    Target: $NAS_USER@$NAS_HOST`:$REMOTE_PATH"
    Write-Host "    Top-level entries:"
    $files | Where-Object { $_ -match '^\./[^/]+$' } | ForEach-Object { Write-Host "      $_" }
    Write-Host ""
    Write-Host "==> Dry run complete. Re-run without -DryRun to actually deploy." -ForegroundColor Green
    exit 0
}

# ---- Step 1: ensure remote dir exists ----
Write-Host "==> [1/5] Ensuring remote directory: $REMOTE_PATH" -ForegroundColor Cyan
$exitCode = Invoke-NasSSH -RemoteCmd "mkdir -p '$REMOTE_PATH'"
if ($exitCode -ne 0) {
    Write-Error "ERROR: cannot mkdir on NAS via SSH. Check host/user/port and run .\deploy-nas.ps1 -SetupKeys first (or pass -Password)."
    Remove-Askpass
    exit 1
}

# ---- Step 2: create tarball ----
Write-Host "==> [2/5] Packing project (excludes: data/ .git/ node_modules/ build-host/ .env *.log)" -ForegroundColor Cyan

$tmpTar = [System.IO.Path]::GetTempFileName() + ".tar.gz"
$tarArgs = @("-czf", $tmpTar, "-C", $ProjectRoot)
foreach ($ex in $Excludes) {
    $tarArgs += @("--exclude", $ex)
}
$tarArgs += "."

Write-Host "    Packing to $tmpTar ..."
& tar @tarArgs
if ($LASTEXITCODE -ne 0) {
    Write-Error "ERROR: tar failed to create archive."
    Remove-Item $tmpTar -ErrorAction SilentlyContinue
    exit 1
}
$sizeMB = [math]::Round((Get-Item $tmpTar).Length / 1MB, 2)
Write-Host "    Tarball size: $sizeMB MB"

# ---- Step 3: scp tarball + extract on NAS ----
Write-Host "==> [3/5] Transferring to NAS and extracting" -ForegroundColor Cyan
# scp the tarball INTO $REMOTE_PATH (not as a sibling). The remote
# extract step does `cd $REMOTE_PATH && tar -xzf deploy.tar.gz`, so
# the file must land inside the directory.
$exitCode = Invoke-NasSCP -LocalPath $tmpTar -RemoteDest "${NAS_USER}@${NAS_HOST}:$REMOTE_PATH/deploy.tar.gz"
if ($exitCode -ne 0) {
    Write-Error "ERROR: scp failed."
    Remove-Item $tmpTar -ErrorAction SilentlyContinue
    Remove-Askpass
    exit 1
}
Remove-Item $tmpTar -ErrorAction SilentlyContinue

# Extract on remote, then remove the tarball. --overwrite --no-same-owner
# avoids permission issues when extracting as non-root. Also chmod +x
# the mosquitto entrypoint — Windows tar doesn't preserve Unix execute
# bits, so shell scripts arrive as 0644 and Docker can't exec them.
$exitCode = Invoke-NasSSH -RemoteCmd "cd '$REMOTE_PATH' && tar -xzf deploy.tar.gz --overwrite --no-same-owner && rm deploy.tar.gz && chmod +x deploy/mosquitto/docker-entrypoint.sh"
if ($exitCode -ne 0) {
    Write-Error "ERROR: remote tar extract failed."
    Remove-Askpass
    exit 1
}

# ---- Step 4: refuse to deploy if .env missing on NAS ----
# Without .env, docker compose substitutes empty strings and the API
# refuses to start (jwt.secret < 32 chars). Fail loudly here.
Write-Host "==> [4/5] Verifying .env exists on NAS" -ForegroundColor Cyan
$exitCode = Invoke-NasSSH -RemoteCmd "test -f '$REMOTE_PATH/.env'"
if ($exitCode -ne 0) {
    Write-Host "ERROR: $REMOTE_PATH/.env NOT found on NAS." -ForegroundColor Red
    Write-Host ""
    Write-Host "First-time deploy — create it manually on the NAS:"
    Write-Host "  ssh -p $NAS_PORT $NAS_USER@$NAS_HOST"
    Write-Host "  cd $REMOTE_PATH"
    Write-Host "  cp .env.example .env"
    Write-Host "  # edit .env: JWT_SECRET=`$(openssl rand -hex 32), MQTT_PASSWORD=..."
    Write-Host "  # regenerate mosquitto passwd to match (see README section 2)"
    Write-Host ""
    Write-Host "Then re-run .\deploy-nas.ps1"
    Remove-Askpass
    exit 1
}

# ---- Step 5: build + start services on the NAS ----
Write-Host "==> [5/5] Building and starting services on NAS (docker compose up -d $BuildFlag)" -ForegroundColor Cyan
$composeCmd = "cd '$REMOTE_PATH' && docker compose up -d $BuildFlag 2>&1"
$exitCode = Invoke-NasSSH -RemoteCmd $composeCmd
if ($exitCode -ne 0) {
    Write-Error "ERROR: docker compose up failed on NAS. Check the output above."
    Remove-Askpass
    exit 1
}

# ---- Status ----
Write-Host ""
Write-Host "==> Service status:" -ForegroundColor Cyan
Invoke-NasSSH -RemoteCmd "cd '$REMOTE_PATH' && docker compose ps" | Out-Host

Write-Host ""
Write-Host "==> Deploy complete." -ForegroundColor Green
Write-Host "    Web UI:  http://$NAS_HOST/"
Write-Host "    API:     http://$NAS_HOST`:8080/health"
Write-Host "    Logs:    ssh -p $NAS_PORT $NAS_USER@$NAS_HOST 'cd $REMOTE_PATH && docker compose logs -f api'"

Remove-Askpass

# ---- Optional: tail api logs ----
# Re-setup askpass for the log tail (was cleaned up above).
if ($Logs) {
    if ($Password) {
        $script:AskpassFile = [System.IO.Path]::GetTempFileName() + "-askpass.bat"
        "@echo $Password" | Set-Content $script:AskpassFile -Encoding ASCII
        $env:SSH_ASKPASS = $script:AskpassFile
        $env:SSH_ASKPASS_REQUIRE = "force"
        $env:DISPLAY = "1"
    }
    Write-Host ""
    Write-Host "==> Tailing api logs (Ctrl-C to exit)..." -ForegroundColor Cyan
    Invoke-NasSSH -RemoteCmd "cd '$REMOTE_PATH' && docker compose logs -f api" | Out-Host
    Remove-Askpass
}
