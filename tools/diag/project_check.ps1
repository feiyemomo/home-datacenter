param(
    [switch]$Web,
    [switch]$Android,
    [switch]$Api,
    [string]$AndroidTest
)

$ErrorActionPreference = "Continue"

function Check-Web {
    Write-Host "==> [Web Frontend] Running TypeScript Check (tsc --noEmit)..." -ForegroundColor Cyan
    Push-Location "D:\Projects\home-datacenter\web"
    try {
        npx tsc --noEmit
        if ($LASTEXITCODE -eq 0) {
            Write-Host " [Web] TypeScript check passed cleanly." -ForegroundColor Green
        } else {
            Write-Host " [Web] TypeScript check found errors (Exit Code $LASTEXITCODE)." -ForegroundColor Red
        }
    } finally {
        Pop-Location
    }
}

function Check-Api {
    Write-Host "==> [Go API Backend] Running Go Vet & Compile Check..." -ForegroundColor Cyan
    Push-Location "D:\Projects\home-datacenter\services\api"
    try {
        go vet ./internal/... ./cmd/...
        if ($LASTEXITCODE -ne 0) {
            Write-Host " [Go API] go vet found issues." -ForegroundColor Red
        } else {
            go test -run=^$ ./internal/...
            if ($LASTEXITCODE -eq 0) {
                Write-Host " [Go API] Code compiles and passes package checks." -ForegroundColor Green
            } else {
                Write-Host " [Go API] Package compilation failed." -ForegroundColor Red
            }
        }
    } finally {
        Pop-Location
    }
}

function Check-Android {
    Write-Host "==> [Android Client] Setting up JDK 21 & running check..." -ForegroundColor Cyan
    $env:JAVA_HOME = "D:\AndroidStudio\jbr"
    $env:PATH = "$env:JAVA_HOME\bin;$env:PATH"
    Push-Location "D:\Projects\Android"
    try {
        if ($AndroidTest) {
            Write-Host "    Running test class: $AndroidTest"
            .\gradlew.bat testDebugUnitTest --tests $AndroidTest
        } else {
            Write-Host "    Running compileDebugKotlin..."
            .\gradlew.bat compileDebugKotlin
        }
        if ($LASTEXITCODE -eq 0) {
            Write-Host " [Android] Check succeeded." -ForegroundColor Green
        } else {
            Write-Host " [Android] Check failed (Exit Code $LASTEXITCODE)." -ForegroundColor Red
        }
    } finally {
        Pop-Location
    }
}

if (-not $Web -and -not $Android -and -not $Api) {
    # Default: Run Web and API quick checks
    Check-Web
    Check-Api
} else {
    if ($Web) { Check-Web }
    if ($Api) { Check-Api }
    if ($Android) { Check-Android }
}
