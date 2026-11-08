# PowerShell runner for Home Datacenter E2E test track
param(
    [int]$Tier,
    [int]$WorkOrder,
    [switch]$Verbose,
    [string]$JsonReport,
    [string]$MdReport
)

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RunnerScript = Join-Path $ScriptDir "run_e2e.py"

$ArgsList = @()
if ($Tier) { $ArgsList += "--tier", $Tier }
if ($WorkOrder) { $ArgsList += "--wo", $WorkOrder }
if ($Verbose) { $ArgsList += "-v" }
if ($JsonReport) { $ArgsList += "--json-report", $JsonReport }
if ($MdReport) { $ArgsList += "--md-report", $MdReport }

python $RunnerScript @ArgsList
exit $LASTEXITCODE
