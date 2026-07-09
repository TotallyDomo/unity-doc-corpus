[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"

# Quiet-on-success: swallow go build chatter (it re-enters agent context on every later
# turn). Full output only on failure, or with -Verbose. Errors stay fully verbose.
function Invoke-GoStep {
    param([string]$Label, [string[]]$GoArgs)
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $out = & go @GoArgs 2>&1
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prevEAP
    if ($code -ne 0) {
        $out | ForEach-Object { Write-Host $_ }
        throw "$Label failed with exit code $code"
    }
    $out | ForEach-Object { Write-Verbose ([string]$_) }
}

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$OutDir = Join-Path $RepoRoot "bin"
$GoRoot = Join-Path $RepoRoot "go"
$Exe = Join-Path $OutDir "unity-doc-corpus.exe"
$BenchmarkExe = Join-Path $OutDir "unity-doc-corpus-benchmark.exe"

New-Item -ItemType Directory -Force $OutDir | Out-Null

# Go's default module/build caches are used unless GOMODCACHE/GOCACHE are already set in the
# environment. Set them yourself to relocate the caches; the script no longer hardcodes them.

Push-Location $GoRoot
try {
    Invoke-GoStep "go build" @("build", "-trimpath", "-ldflags=-s -w", "-o", $Exe, ".")
    Invoke-GoStep "go build (benchmark)" @("build", "-trimpath", "-ldflags=-s -w", "-o", $BenchmarkExe, ".\cmd\unity-doc-corpus-benchmark")
}
finally {
    Pop-Location
}

if (!(Test-Path $Exe)) {
    throw "Go build did not produce $Exe"
}
if (!(Test-Path $BenchmarkExe)) {
    throw "Go build did not produce $BenchmarkExe"
}

Write-Host $Exe
Write-Host $BenchmarkExe
