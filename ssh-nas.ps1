# ssh-nas.ps1 — Convenient SSH wrapper for the NAS.
#
# Solves two pain points vs. raw ssh from PowerShell:
#   1. Password auth — auto-provides the NAS password via SSH_ASKPASS
#      (no interactive prompt, no sshpass needed on Windows).
#   2. Shell quoting — PowerShell mangles bash commands containing
#      quotes, parentheses, $vars, etc. The -File mode uploads a local
#      script via scp and executes it remotely, so the script content
#      is passed verbatim with ZERO escaping issues.
#
# Usage:
#   .\ssh-nas.ps1 'docker ps'                       # run a simple command
#   .\ssh-nas.ps1 -File diag.sh                     # upload + run a script
#   .\ssh-nas.ps1 -File diag.sh -Download out.txt   # download script output
#   .\ssh-nas.ps1 'uptime' -Password 'xxx'          # override password
#   .\ssh-nas.ps1 'echo hi' -Host 192.168.1.3       # override host
#
# -File mode: the file is scp'd to /tmp/ssh-nas-script.<pid>.sh, executed
# with `bash`, and deleted afterwards. Use this for anything non-trivial.
#
# -Download: captures the remote script's stdout+stderr to a temp file and
# downloads it to the local path you specify. Useful for grepping long
# output locally.

[CmdletBinding()]
param(
    # Inline command to run. Ignored if -File is given. Quote carefully —
    # for anything beyond a simple `docker ps`, prefer -File.
    [Parameter(Position = 0)]
    [string]$Command,

    # Local script file to upload and execute remotely. Avoids all
    # PowerShell→bash escaping issues.
    [string]$File,

    # Local path to download the remote stdout/stderr to. Optional.
    [string]$Download,

    [string]$NasHost = "192.168.1.3",
    [string]$User = "fnos-momo",
    [int]$Port = 22,
    [string]$Password = "@Fnos324"
)

$ErrorActionPreference = "Stop"

# ---- Askpass setup (same mechanism as deploy-nas.ps1) ----
$askpass = [System.IO.Path]::GetTempFileName() + "-askpass.bat"
"@echo $Password" | Set-Content $askpass -Encoding ASCII
$env:SSH_ASKPASS = $askpass
$env:SSH_ASKPASS_REQUIRE = "force"
$env:DISPLAY = "1"

function Remove-Askpass {
    if (Test-Path $askpass) { Remove-Item $askpass -ErrorAction SilentlyContinue }
}
trap { Remove-Askpass; break }
Register-EngineEvent PowerShell.Exiting -Action { Remove-Item $askpass -ErrorAction SilentlyContinue } | Out-Null

$sshOpts = @("-p", "$Port",
             "-o", "StrictHostKeyChecking=no",
             "-o", "UserKnownHostsFile=NUL",
             "-o", "ConnectTimeout=10",
             "-o", "PreferredAuthentications=password",
             "-o", "PubkeyAuthentication=no",
             "-o", "NumberOfPasswordPrompts=1")
$scpOpts = @("-P", "$Port") + $sshOpts[2..($sshOpts.Length - 1)]

function Invoke-SSH {
    param([string]$RemoteCmd)
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        # Out-Host is critical: without it, ssh's stderr (e.g. "Warning:
        # Permanently added ...") pollutes the pipeline and corrupts the
        # function's return value (PowerShell merges all pipeline output
        # with the `return` value into the caller's $result).
        & ssh @sshOpts "$User@$NasHost" $RemoteCmd 2>&1 | Out-Host
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $prevEAP
    }
    return $code
}

function Invoke-SCP {
    param([string]$LocalPath, [string]$RemoteDest)
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

# ---- Mode: file upload + execute ----
if ($File) {
    if (-not (Test-Path $File)) {
        Write-Error "File not found: $File"
        Remove-Askpass
        exit 1
    }
    $pid_ = $PID
    $remoteScript = "/tmp/ssh-nas-script.$pid_.sh"
    $remoteOut = "/tmp/ssh-nas-out.$pid_.txt"

    Write-Host "==> Uploading $File -> $remoteScript" -ForegroundColor Cyan
    $code = Invoke-SCP -LocalPath $File -RemoteDest "${User}@${NasHost}:$remoteScript"
    if ($code -ne 0) {
        Write-Error "scp upload failed."
        Remove-Askpass
        exit $code
    }

    Write-Host "==> Executing remotely..." -ForegroundColor Cyan
    # bash <script> > <out> 2>&1; then cat the out so it streams to our stdout too.
    $execCmd = "bash $remoteScript > $remoteOut 2>&1; cat $remoteOut; rm -f $remoteScript $remoteOut"
    $code = Invoke-SSH -RemoteCmd $execCmd
    if ($Download) {
        Write-Host "==> (re-running for -Download capture)" -ForegroundColor Yellow
        $remoteScript2 = "/tmp/ssh-nas-script2.$pid_.sh"
        $remoteOut2 = "/tmp/ssh-nas-out2.$pid_.txt"
        Invoke-SCP -LocalPath $File -RemoteDest "${User}@${NasHost}:$remoteScript2" | Out-Null
        Invoke-SSH -RemoteCmd "bash $remoteScript2 > $remoteOut2 2>&1; rm -f $remoteScript2" | Out-Null
        Invoke-SCP -LocalPath "${User}@${NasHost}:$remoteOut2" -RemoteDest $Download | Out-Null
        Invoke-SSH -RemoteCmd "rm -f $remoteOut2" | Out-Null
        Write-Host "    Output saved to: $Download" -ForegroundColor Green
    }
    Remove-Askpass
    exit $code
}

# ---- Mode: inline command ----
if (-not $Command) {
    Write-Host "Usage: .\ssh-nas.ps1 'command'  |  .\ssh-nas.ps1 -File script.sh" -ForegroundColor Yellow
    Remove-Askpass
    exit 1
}

# Pass the command directly (same pattern as deploy-nas.ps1). ssh sends
# it to the remote shell verbatim. For commands containing parentheses,
# $vars, nested quotes, etc., use -File mode to avoid bash parsing issues.
$code = Invoke-SSH -RemoteCmd $Command
Remove-Askpass
exit $code
