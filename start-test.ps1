# Build and run an isolated Masamong instance for Windows development.
#   powershell -ExecutionPolicy Bypass -File .\start-test.ps1
#   powershell -ExecutionPolicy Bypass -File .\start-test.ps1 -Reset
#   powershell -ExecutionPolicy Bypass -File .\start-test.ps1 -Check
#   powershell -ExecutionPolicy Bypass -File .\start-test.ps1 -Stop
param(
  [switch]$Reset,
  [switch]$Check,
  [switch]$Stop
)

$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$profileDir = [Environment]::GetFolderPath('UserProfile')
$root = if ([string]::IsNullOrWhiteSpace($env:MASAMONG_TRY_ROOT)) { Join-Path $profileDir '.masamong-try' } else { [IO.Path]::GetFullPath($env:MASAMONG_TRY_ROOT) }
$binDir = Join-Path $here 'build\try'
$cli = Join-Path $binDir 'masamong.exe'
$daemon = Join-Path $binDir 'masamongd.exe'

# The folder this script creates is marked; -Reset deletes only a folder carrying that marker.
$marker = '.masamong-try-root'
$markerValue = 'Created by start-test.ps1; -Reset may delete this folder.'

function Test-OwnTryRoot([string]$Dir) {
  $path = Join-Path $Dir $marker
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { return $false }
  try { return [string]::Equals([IO.File]::ReadAllText($path), $markerValue, [StringComparison]::Ordinal) }
  catch { return $false }
}

# Standard error is kept apart: under $ErrorActionPreference = 'Stop', Windows PowerShell 5.1 turns a
# single stderr line (masamong logs progress there) into a terminating error.
function Invoke-TryCLI([string[]]$Arguments) {
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $all = @(& $cli @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
  } finally { $ErrorActionPreference = $previous }
  if ($exitCode -ne 0) { throw "Masamong command failed ($exitCode): $(($all | ForEach-Object { "$_" }) -join ' ')" }
  return @($all | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] })
}

function Get-TryStatus {
  if (-not (Test-Path -LiteralPath $cli -PathType Leaf)) { return $null }
  $text = (Invoke-TryCLI @('--data-root', $root, '--output', 'json', 'status') | Out-String).Trim()
  return ($text | ConvertFrom-Json -ErrorAction Stop).output
}

function Stop-TryService {
  if (-not (Test-Path -LiteralPath $cli -PathType Leaf)) { return }
  $null = Invoke-TryCLI @('--data-root', $root, '--output', 'json', 'stop')
  $deadline = [DateTime]::UtcNow.AddSeconds(30)
  do {
    $status = Get-TryStatus
    if (-not $status -or -not $status.running) { return }
    Start-Sleep -Milliseconds 250
  } while ([DateTime]::UtcNow -lt $deadline)
  throw "The test service did not stop. Check $root\logs\daemon.log."
}

if ($Stop) {
  Stop-TryService
  Write-Host 'The isolated test service is stopped.'
  return
}

Set-Location -LiteralPath $here
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw 'Go 1.27 or newer is required. Install it from https://go.dev/dl/ and retry.'
}
$requiredLine = Get-Content -LiteralPath (Join-Path $here 'go.mod') | Where-Object { $_ -match '^go\s+(\d+\.\d+(?:\.\d+)?)\s*$' } | Select-Object -First 1
if (-not $requiredLine) { throw 'Could not read the required Go version from go.mod.' }
$required = [version]([regex]::Match($requiredLine, '^go\s+(\d+\.\d+(?:\.\d+)?)').Groups[1].Value)
$installedText = (& go env GOVERSION) -replace '^go', ''
if ($installedText -notmatch '^\d+\.\d+(?:\.\d+)?') { throw "Could not read Go version '$installedText'." }
$installed = [version]$Matches[0]
if ($installed -lt $required) { throw "Go $required or newer is required (found $installed). Install an update from https://go.dev/dl/." }

$env:MASAMONG_DATA_ROOT = $root
$env:MASAMONG_SECRET_STORE = 'file'
if ($Reset) {
  Stop-TryService
  $profileFull = [IO.Path]::GetFullPath($profileDir).TrimEnd('\')
  $rootFull = [IO.Path]::GetFullPath($root).TrimEnd('\')
  $rootDrive = [IO.Path]::GetPathRoot($rootFull).TrimEnd('\')
  $protected = @(
    $profileFull,
    [IO.Path]::GetFullPath((Split-Path -Parent $profileFull)).TrimEnd('\'),
    [IO.Path]::GetFullPath((Join-Path $env:APPDATA 'Masamong')).TrimEnd('\'),
    [IO.Path]::GetFullPath($env:APPDATA).TrimEnd('\'),
    [IO.Path]::GetFullPath($env:SystemRoot).TrimEnd('\'),
    $rootDrive
  )
  if ($protected | Where-Object { [string]::Equals($rootFull, $_, [StringComparison]::OrdinalIgnoreCase) }) {
    throw "Refusing to reset a broad or production data folder: $root"
  }
  if ((Test-Path -LiteralPath $root) -and -not (Test-OwnTryRoot $rootFull)) {
    throw "Refusing to delete ${root}: it was not created by this script (no $marker file in it). Delete it yourself, or point MASAMONG_TRY_ROOT at a new folder."
  }
  if (Test-Path -LiteralPath $root) {
    Write-Host "Removing isolated test data: $root"
    Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction Stop
    if (Test-Path -LiteralPath $root) { throw "Could not remove $root" }
  }
} else {
  # Stop before rebuilding: Windows keeps a running executable image locked.
  Stop-TryService
}

if (-not (Test-Path -LiteralPath $binDir)) { New-Item -ItemType Directory -Path $binDir -Force -ErrorAction Stop | Out-Null }
$version = (& git -C $here describe --tags --always --dirty 2>$null)
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($version)) { $version = 'dev' }
$ldflags = "-s -w -X masamong/internal/application.Version=$($version.Trim())"
Write-Host "Building the Windows test binaries from $here ..."
& go build -trimpath -ldflags $ldflags -o $cli ./cmd/masamong
if ($LASTEXITCODE -ne 0) { throw 'Building masamong.exe failed.' }
& go build -trimpath -ldflags $ldflags -o $daemon ./cmd/masamongd
if ($LASTEXITCODE -ne 0) { throw 'Building masamongd.exe failed.' }

if ($Check) {
  Write-Host 'Running go vet and go test ...'
  & go vet ./...
  if ($LASTEXITCODE -ne 0) { throw 'go vet failed; the test service was not started.' }
  $previousOffline = $env:MASAMONG_OFFLINE
  $env:MASAMONG_OFFLINE = '1'
  try {
    & go test ./... -count=1
    if ($LASTEXITCODE -ne 0) { throw 'go test failed; the test service was not started.' }
  } finally { $env:MASAMONG_OFFLINE = $previousOffline }
}

# Mark only a folder this script creates. An existing empty folder still belongs to its owner.
if (-not (Test-Path -LiteralPath $root)) {
  New-Item -ItemType Directory -Path $root -Force -ErrorAction Stop | Out-Null
  Set-Content -LiteralPath (Join-Path $root $marker) -Value $markerValue -NoNewline -ErrorAction Stop
}
Write-Host "Starting the isolated test service and opening its signed-in console ($root) ..."
$opened = (Invoke-TryCLI @('--data-root', $root, '--output', 'json', 'open', '--no-tray') | Out-String).Trim()
$openResult = ($opened | ConvertFrom-Json -ErrorAction Stop).output
$status = Get-TryStatus
if (-not $status -or -not $status.running) { throw "The test service did not start. Check $root\logs\daemon.log." }
$log = Join-Path $root 'logs\daemon.log'
$deadline = [DateTime]::UtcNow.AddSeconds(10)
while (-not (Test-Path -LiteralPath $log) -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }

Write-Host ''
Write-Host "Test data: $root"
Write-Host "Console:   $($openResult.url)"
Write-Host 'Secret store: files in this test root; no production keys are reused.'
Write-Host 'Logs follow below. Press Ctrl+C to stop the test service.'
Write-Host ''

$tailJob = $null
try {
  if (Test-Path -LiteralPath $log) {
    $tailJob = Start-Job -ArgumentList $log -ScriptBlock { param($Path) Get-Content -LiteralPath $Path -Tail 20 -Wait -ErrorAction SilentlyContinue }
  }
  while ($true) {
    if ($tailJob) {
      foreach ($line in @(Receive-Job -Job $tailJob -ErrorAction SilentlyContinue)) { Write-Host $line }
    }
    Start-Sleep -Seconds 1
    $status = Get-TryStatus
    if (-not $status -or -not $status.running) { break }
  }
} finally {
  if ($tailJob) {
    Stop-Job -Job $tailJob -ErrorAction SilentlyContinue
    Remove-Job -Job $tailJob -Force -ErrorAction SilentlyContinue
  }
  Stop-TryService
  Write-Host ''
  Write-Host 'The isolated test service is stopped.'
}
