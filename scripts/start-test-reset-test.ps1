$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$repo = Split-Path -Parent $here
$startTest = Join-Path $repo 'start-test.ps1'
$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("masamong-start-test-reset-" + [guid]::NewGuid().ToString('N'))
$tryRoot = Join-Path $tempRoot '.masamong-try'
$sentinel = Join-Path $tryRoot 'keep.txt'
$previous = $env:MASAMONG_TRY_ROOT

try {
  New-Item -ItemType Directory -Path $tryRoot -Force | Out-Null
  Set-Content -LiteralPath $sentinel -Value 'must survive reset' -NoNewline
  $env:MASAMONG_TRY_ROOT = $tryRoot
  $previousPreference = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $output = @(& powershell.exe -NoProfile -ExecutionPolicy Bypass -File $startTest -Reset 2>&1)
    $exitCode = $LASTEXITCODE
  } finally { $ErrorActionPreference = $previousPreference }
  if ($exitCode -eq 0) { throw 'start-test.ps1 -Reset unexpectedly accepted an unmarked .masamong-try folder.' }
  if (-not (Test-Path -LiteralPath $sentinel -PathType Leaf)) { throw 'start-test.ps1 -Reset deleted the unmarked folder.' }
  if (($output -join "`n") -notmatch 'no \.masamong-try-root file') { throw "unexpected refusal message: $($output -join ' ')" }
  Set-Content -LiteralPath (Join-Path $tryRoot '.masamong-try-root') -Value 'not written by start-test.ps1' -NoNewline
  $ErrorActionPreference = 'Continue'
  try {
    $output = @(& powershell.exe -NoProfile -ExecutionPolicy Bypass -File $startTest -Reset 2>&1)
    $exitCode = $LASTEXITCODE
  } finally { $ErrorActionPreference = $previousPreference }
  if ($exitCode -eq 0) { throw 'start-test.ps1 -Reset accepted a marker with unexpected content.' }
  if (-not (Test-Path -LiteralPath $sentinel -PathType Leaf)) { throw 'start-test.ps1 -Reset deleted the folder with a forged marker.' }
  Write-Host 'start-test.ps1 -Reset preserved an unmarked folder named .masamong-try.'
  Write-Host 'start-test.ps1 -Reset rejected a marker with unexpected content.'
} finally {
  $env:MASAMONG_TRY_ROOT = $previous
  $tempFull = [IO.Path]::GetFullPath($tempRoot)
  $tempBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
  if (-not $tempFull.StartsWith($tempBase, [StringComparison]::OrdinalIgnoreCase)) { throw "refusing cleanup outside the temp directory: $tempFull" }
  if (Test-Path -LiteralPath $tempFull) { Remove-Item -LiteralPath $tempFull -Recurse -Force }
}
