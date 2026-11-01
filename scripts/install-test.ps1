# Checks install.ps1's helpers without touching the real user PATH, install folder or services.
#   powershell -ExecutionPolicy Bypass -File scripts\install-test.ps1
# CI runs it on windows-latest. It exits 1 on the first failed check.
$ErrorActionPreference = 'Stop'
$processPathBefore = $env:Path
$realEnvKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $false)
$realUserPathBefore = $null
$realUserPathKindBefore = [Microsoft.Win32.RegistryValueKind]::ExpandString
if ($null -ne $realEnvKey) {
  try {
    $realUserPathBefore = $realEnvKey.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if ($null -ne $realUserPathBefore) { $realUserPathKindBefore = $realEnvKey.GetValueKind('Path') }
  } finally { $realEnvKey.Close() }
}
$root = Split-Path -Parent $PSScriptRoot
$failures = 0
function Check([bool]$Condition, [string]$Message) {
  if ($Condition) { Write-Host "ok   $Message" } else { Write-Host "FAIL $Message"; $script:failures++ }
}

$env:MASAMONG_INSTALL_LIBRARY = '1'
$scratchKey = 'Software\MasamongInstallTest-' + [guid]::NewGuid().ToString('N')
$env:MASAMONG_INSTALL_ENV_KEY = $scratchKey
. (Join-Path $root 'install.ps1')
Remove-Item Env:MASAMONG_INSTALL_LIBRARY

$tmp = Join-Path ([IO.Path]::GetTempPath()) ('masamong-install-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  # --- WIN-3: the user PATH keeps its type and its unexpanded %VARS% ---
  $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($scratchKey)
  $original = '%USERPROFILE%\bin;C:\x;%SystemRoot%\system32'
  $key.SetValue('Path', $original, [Microsoft.Win32.RegistryValueKind]::ExpandString)
  $key.Close()
  $added = Join-Path $tmp 'apps\Masamong'
  Add-UserPathEntry $added
  $now = Get-UserPath
  Check ($now.Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString) 'PATH stays REG_EXPAND_SZ after an add'
  Check ($now.Value -eq ($original + ';' + $added)) 'PATH keeps %USERPROFILE% and %SystemRoot% unexpanded and appends the entry'
  Add-UserPathEntry $added
  Check ((Get-UserPath).Value -eq $now.Value) 'adding the same entry twice does not duplicate it'
  Add-UserPathEntry ($added.ToUpperInvariant() + '\')
  Check ((Get-UserPath).Value -eq $now.Value) 'an entry that differs only in case or trailing slash is recognised'
  Remove-UserPathEntry $added
  $back = Get-UserPath
  Check ($back.Value -eq $original -and $back.Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString) 'removing the entry restores the original value and type'
  Check (Has-PathEntry $original (Join-Path $env:USERPROFILE 'bin')) 'a %USERPROFILE% entry is recognised after expansion'

  # --- WIN-4: the folder is proven writable before anything is stopped ---
  $blocker = Join-Path $tmp 'file-not-folder'
  Set-Content -LiteralPath $blocker -Value 'x'
  $message = ''
  try { Assert-Writable (Join-Path $blocker 'sub') } catch { $message = $_.Exception.Message }
  Check ($message -match 'Cannot write to' -and $message -match 'Nothing was stopped') 'an unwritable install folder fails early with advice'
  $okDir = Join-Path $tmp 'writable'
  Assert-Writable $okDir
  Check ((Test-Path $okDir -PathType Container) -and -not (Get-ChildItem -Force $okDir)) 'a writable folder is created and the probe is removed'

  # --- WIN-4: a failed swap puts the old programs back ---
  $dir = Join-Path $tmp 'install'
  New-Item -ItemType Directory -Path $dir | Out-Null
  Set-Content -LiteralPath (Join-Path $dir 'masamong.exe') -Value 'old-cli'
  Set-Content -LiteralPath (Join-Path $dir 'masamongd.exe') -Value 'old-daemon'
  Set-Content -LiteralPath (Join-Path $dir 'masamong.exe.new') -Value 'new-cli'
  # masamongd.exe.new is missing: the second move fails after the first already happened
  $failed = $false
  try { Install-Staged $dir @('masamong.exe', 'masamongd.exe') '.new' } catch { $failed = $true }
  Check $failed 'a swap that cannot finish reports failure'
  Check ((Get-Content (Join-Path $dir 'masamong.exe')) -eq 'old-cli' -and (Get-Content (Join-Path $dir 'masamongd.exe')) -eq 'old-daemon') 'the old programs are back after a failed swap'
  Check (-not (Test-Path (Join-Path $dir 'masamong.exe.old')) -and -not (Test-Path (Join-Path $dir 'masamong.exe.new'))) 'no staging or backup files are left behind'
  Set-Content -LiteralPath (Join-Path $dir 'masamong.exe.new') -Value 'new-cli'
  Set-Content -LiteralPath (Join-Path $dir 'masamongd.exe.new') -Value 'new-daemon'
  Install-Staged $dir @('masamong.exe', 'masamongd.exe') '.new'
  Check ((Get-Content (Join-Path $dir 'masamong.exe')) -eq 'new-cli' -and (Get-Content (Join-Path $dir 'masamongd.exe')) -eq 'new-daemon' -and
         -not (Test-Path (Join-Path $dir 'masamong.exe.old'))) 'a complete swap installs the new programs and removes the backups'

  # --- FA-02: manifests limit uninstall to files Masamong owns ---
  $managed = Join-Path $tmp 'managed-files'
  New-Item -ItemType Directory -Path $managed | Out-Null
  Set-Content -LiteralPath (Join-Path $managed 'masamong.exe') -Value 'owned'
  Set-Content -LiteralPath (Join-Path $managed 'masamongd.exe') -Value 'not-listed'
  Set-Content -LiteralPath (Join-Path $managed 'unrelated.txt') -Value 'keep'
  @{ version = 1; files = @('masamong.exe', '..\outside.txt') } | ConvertTo-Json -Compress | Set-Content -LiteralPath (Join-Path $managed $script:ManifestName)
  $removed = Remove-ManagedFiles $managed
  Check (-not $removed -and -not (Test-Path (Join-Path $managed 'masamong.exe'))) 'uninstall removes the listed Masamong file'
  Check ((Test-Path (Join-Path $managed 'masamongd.exe')) -and (Test-Path (Join-Path $managed 'unrelated.txt'))) 'manifest paths cannot escape and unlisted files stay'
  Remove-Item -LiteralPath (Join-Path $managed 'masamongd.exe') -Force
  $null = Remove-ManagedFiles $managed
  Check ((Test-Path (Join-Path $managed 'unrelated.txt')) -and (Test-Path $managed)) 'uninstall keeps a non-empty shared directory'
  $legacy = Join-Path $tmp 'legacy-install'
  New-Item -ItemType Directory -Path $legacy | Out-Null
  Set-Content -LiteralPath (Join-Path $legacy 'masamong.exe') -Value 'known'
  Set-Content -LiteralPath (Join-Path $legacy 'masamongd.exe') -Value 'known'
  Set-Content -LiteralPath (Join-Path $legacy 'LICENSE') -Value 'known notice'
  Set-Content -LiteralPath (Join-Path $legacy 'other.txt') -Value 'keep'
  $null = Remove-ManagedFiles $legacy
  Check ((Test-Path (Join-Path $legacy 'other.txt')) -and (Test-Path $legacy) -and -not (Test-Path (Join-Path $legacy 'LICENSE'))) 'manifest-free uninstall falls back to known Masamong names only'

  # --- FA-02: broad prefixes and accidental non-empty installs are rejected ---
  $broadRefused = $false
  try { Assert-SafePrefix ([IO.Path]::GetPathRoot($tmp)) | Out-Null } catch { $broadRefused = $true }
  Check $broadRefused 'a drive root is refused for install and uninstall'
  $profileRefused = $false
  try { Assert-SafePrefix $env:USERPROFILE | Out-Null } catch { $profileRefused = $true }
  Check $profileRefused 'the user profile root is refused for install and uninstall'
  foreach ($broadPath in @($env:ProgramFiles, [Environment]::GetEnvironmentVariable('ProgramFiles(x86)'), $env:SystemRoot)) {
    if ([string]::IsNullOrWhiteSpace($broadPath)) { continue }
    $broadPathRefused = $false
    try { Assert-SafePrefix $broadPath | Out-Null } catch { $broadPathRefused = $true }
    Check $broadPathRefused "a Program Files or Windows root is refused for install and uninstall: $broadPath"
  }
  $nonEmpty = Join-Path $tmp 'unmarked-install'
  New-Item -ItemType Directory -Path $nonEmpty | Out-Null
  Set-Content -LiteralPath (Join-Path $nonEmpty 'keep.txt') -Value 'keep'
  $forceRequired = $false
  try { Assert-InstallDirectory $nonEmpty } catch { $forceRequired = $_.Exception.Message -match '-Force' }
  Check $forceRequired 'a non-empty unmarked install folder asks for -Force'
  Assert-InstallDirectory $nonEmpty -AllowNonEmpty

  # --- FA-03: post-stop PATH and startup failures restore the pair and restart prior roots ---
  $oldStop = (Get-Item Function:\Stop-InstalledPrograms).ScriptBlock
  $oldAddPath = (Get-Item Function:\Add-UserPathEntry).ScriptBlock
  $oldInvoke = (Get-Item Function:\Invoke-Masamong).ScriptBlock
  $oldGetProcesses = (Get-Item Function:\Get-InstalledProcesses).ScriptBlock
  try {
    Set-Item Function:\Stop-InstalledPrograms -Value {
      param([string]$InstallDir, [System.Collections.Generic.List[string]]$RunningRoots, [switch]$RemoveAutostart)
      $script:installTestStopCalls++
    }
    Set-Item Function:\Invoke-Masamong -Value {
      param([string]$Exe, [string[]]$Arguments)
      $script:installTestCalls.Add(($Arguments -join ' '))
      if ($Arguments -contains 'status') { return '{"output":{"running":true}}' }
      return '{"ok":true}'
    }
    $pathFailDir = Join-Path $tmp 'transaction-path-failure'
    New-Item -ItemType Directory -Path $pathFailDir | Out-Null
    foreach ($pair in @(@('masamong.exe', 'old-cli', 'new-cli'), @('masamongd.exe', 'old-daemon', 'new-daemon'))) {
      Set-Content -LiteralPath (Join-Path $pathFailDir $pair[0]) -Value $pair[1]
      Set-Content -LiteralPath ((Join-Path $pathFailDir $pair[0]) + '.new') -Value $pair[2]
    }
    $pathFailRoots = New-Object 'System.Collections.Generic.List[string]'
    $pathFailRoots.Add('root-one')
    $beforePath = Get-UserPath
    $beforeProcessPath = $env:Path
    $script:installTestCalls = New-Object 'System.Collections.Generic.List[string]'
    $script:installTestStopCalls = 0
    Set-Item Function:\Add-UserPathEntry -Value { param([string]$Entry) throw 'simulated PATH write failure after install' }
    $pathFailed = $false
    try { Invoke-InstallTransaction $pathFailDir @('masamong.exe', 'masamongd.exe') '.new' @('masamong.exe', 'masamongd.exe') $pathFailRoots $beforePath $beforeProcessPath $false $null }
    catch { $pathFailed = $_.Exception.Message -match 'simulated PATH write failure' }
    Check $pathFailed 'a PATH write failure after the swap reports failure'
    Check ((Get-Content (Join-Path $pathFailDir 'masamong.exe')) -eq 'old-cli' -and (Get-Content (Join-Path $pathFailDir 'masamongd.exe')) -eq 'old-daemon') 'PATH failure restores both previous binaries'
    Check ($script:installTestCalls -contains '--output json --data-root root-one start') 'PATH failure restarts the exact previously running root'
    Check ((Get-UserPath).Value -eq $beforePath.Value -and (Get-UserPath).Kind -eq $beforePath.Kind -and $env:Path -eq $beforeProcessPath) 'PATH failure restores the exact user PATH value, type, and process value'
    Check (-not (Test-Path (Join-Path $pathFailDir $script:ManifestName))) 'a failed install leaves no new ownership manifest'

    $startFailDir = Join-Path $tmp 'transaction-start-failure'
    New-Item -ItemType Directory -Path $startFailDir | Out-Null
    foreach ($pair in @(@('masamong.exe', 'old-cli', 'new-cli'), @('masamongd.exe', 'old-daemon', 'new-daemon'))) {
      Set-Content -LiteralPath (Join-Path $startFailDir $pair[0]) -Value $pair[1]
      Set-Content -LiteralPath ((Join-Path $startFailDir $pair[0]) + '.new') -Value $pair[2]
    }
    $startFailRoots = New-Object 'System.Collections.Generic.List[string]'
    $startFailRoots.Add('root-two')
    $beforePath = Get-UserPath
    $beforeProcessPath = $env:Path
    $script:installTestCalls = New-Object 'System.Collections.Generic.List[string]'
    Set-Item Function:\Add-UserPathEntry -Value {
      param([string]$Entry)
      $current = Get-UserPath
      $newValue = if ([string]::IsNullOrEmpty($current.Value)) { $Entry } else { $current.Value + ';' + $Entry }
      Set-UserPath $newValue $current.Kind
      if (-not (Has-PathEntry $env:Path $Entry)) { $env:Path = $env:Path + ';' + $Entry }
    }
    Set-Item Function:\Invoke-Masamong -Value {
      param([string]$Exe, [string[]]$Arguments)
      $script:installTestCalls.Add(($Arguments -join ' '))
      if (($Arguments -contains 'start') -and ((Get-Content -LiteralPath $Exe -Raw).Trim() -eq 'new-cli')) { throw 'new daemon failed to start' }
      if ($Arguments -contains 'status') { return '{"output":{"running":true}}' }
      return '{"ok":true}'
    }
    $startFailed = $false
    try { Invoke-InstallTransaction $startFailDir @('masamong.exe', 'masamongd.exe') '.new' @('masamong.exe', 'masamongd.exe') $startFailRoots $beforePath $beforeProcessPath $false $null }
    catch { $startFailed = $_.Exception.Message -match 'new daemon failed to start' }
    Check $startFailed 'a new daemon start failure rolls the install back'
    Check ((Get-Content (Join-Path $startFailDir 'masamong.exe')) -eq 'old-cli' -and (Get-Content (Join-Path $startFailDir 'masamongd.exe')) -eq 'old-daemon') 'start failure restores both previous binaries'
    Check (($script:installTestCalls | Where-Object { $_ -eq '--output json --data-root root-two start' }).Count -eq 2) 'start failure restarts the prior root after restoring the old pair'
    Check ((Get-UserPath).Value -eq $beforePath.Value -and (Get-UserPath).Kind -eq $beforePath.Kind -and $env:Path -eq $beforeProcessPath) 'start failure restores the exact PATH snapshot'

    $successDir = Join-Path $tmp 'transaction-success'
    New-Item -ItemType Directory -Path $successDir | Out-Null
    foreach ($pair in @(@('masamong.exe', 'old-cli', 'new-cli'), @('masamongd.exe', 'old-daemon', 'new-daemon'))) {
      Set-Content -LiteralPath (Join-Path $successDir $pair[0]) -Value $pair[1]
      Set-Content -LiteralPath ((Join-Path $successDir $pair[0]) + '.new') -Value $pair[2]
    }
    $successRoots = New-Object 'System.Collections.Generic.List[string]'
    $beforePath = Get-UserPath
    $beforeProcessPath = $env:Path
    $null = Invoke-InstallTransaction $successDir @('masamong.exe', 'masamongd.exe') '.new' @('masamong.exe', 'masamongd.exe') $successRoots $beforePath $beforeProcessPath $false $null
    $writtenManifest = @(Read-InstallManifest $successDir)
    Check ((Get-Content (Join-Path $successDir 'masamong.exe')) -eq 'new-cli' -and $writtenManifest.Count -eq 2 -and $writtenManifest -contains 'masamongd.exe') 'a successful install commits the new pair and its ownership manifest'
    Check (-not (Test-Path (Join-Path $successDir 'masamong.exe.old'))) 'successful health checks allow old executable backups to be removed'
    Restore-UserPath $beforePath $beforeProcessPath

    # --- FA-04: two custom roots are both stopped and restarted with --data-root ---
    $multiDir = Join-Path $tmp 'transaction-two-roots'
    New-Item -ItemType Directory -Path $multiDir | Out-Null
    Set-Content -LiteralPath (Join-Path $multiDir 'masamong.exe') -Value 'cli'
    Set-Content -LiteralPath (Join-Path $multiDir 'masamongd.exe') -Value 'daemon'
    $rootOne = Join-Path $tmp 'custom root one'
    $rootTwo = Join-Path $tmp 'custom root two'
    New-Item -ItemType Directory -Path $rootOne,$rootTwo | Out-Null
    $env:MASAMONG_DATA_ROOT = Join-Path $tmp 'missing-default-root'
    $script:installTestProcesses = @(
      [pscustomobject]@{ ExecutablePath = Join-Path $multiDir 'masamongd.exe'; CommandLine = "masamongd.exe --data-root `"$rootOne`"" },
      [pscustomobject]@{ ExecutablePath = Join-Path $multiDir 'masamongd.exe'; CommandLine = "masamongd.exe --data-root `"$rootTwo`"" })
    Set-Item Function:\Get-InstalledProcesses -Value {
      param([string]$CliPath, [string]$DaemonPath)
      return @($script:installTestProcesses)
    }
    Set-Item Function:\Invoke-Masamong -Value {
      param([string]$Exe, [string[]]$Arguments)
      $script:installTestCalls.Add(($Arguments -join ' '))
      if ($Arguments -contains 'status') { return '{"output":{"running":true}}' }
      if ($Arguments -contains 'stop') {
        $rootIndex = [Array]::IndexOf($Arguments, '--data-root') + 1
        $stoppedRoot = $Arguments[$rootIndex]
        $script:installTestProcesses = @($script:installTestProcesses | Where-Object { $_.CommandLine -notlike "*$stoppedRoot*" })
      }
      return '{"ok":true}'
    }
    $multiRoots = New-Object 'System.Collections.Generic.List[string]'
    $script:installTestCalls = New-Object 'System.Collections.Generic.List[string]'
    Set-Item Function:\Stop-InstalledPrograms -Value $oldStop
    Stop-InstalledPrograms $multiDir $multiRoots
    Check ($multiRoots.Count -eq 2 -and $multiRoots.Contains($rootOne) -and $multiRoots.Contains($rootTwo)) "the stop snapshot contains both running custom roots (got: $($multiRoots -join ', '))"
    Check (($script:installTestCalls | Where-Object { $_ -match '--data-root .* stop$' }).Count -eq 2) "each custom root receives its own graceful stop (calls: $($script:installTestCalls -join ' | '))"
    $script:installTestCalls.Clear()
    Start-HealthyInstances (Join-Path $multiDir 'masamong.exe') @($multiRoots)
    $starts = @($script:installTestCalls | Where-Object { $_ -match '--data-root .* start$' })
    Check ($starts.Count -eq 2 -and ($starts -join ' ') -match [regex]::Escape($rootOne) -and ($starts -join ' ') -match [regex]::Escape($rootTwo)) "each custom root is started and health-checked with --data-root (calls: $($starts -join ' | '))"

    $partialDir = Join-Path $tmp 'transaction-partial-stop'
    New-Item -ItemType Directory -Path $partialDir | Out-Null
    Set-Content -LiteralPath (Join-Path $partialDir 'masamong.exe') -Value 'old-cli'
    Set-Content -LiteralPath (Join-Path $partialDir 'masamongd.exe') -Value 'old-daemon'
    Set-Content -LiteralPath ((Join-Path $partialDir 'masamong.exe') + '.new') -Value 'new-cli'
    Set-Content -LiteralPath ((Join-Path $partialDir 'masamongd.exe') + '.new') -Value 'new-daemon'
    $script:installTestProcesses = @(
      [pscustomobject]@{ ExecutablePath = Join-Path $partialDir 'masamongd.exe'; CommandLine = "masamongd.exe --data-root `"$rootOne`"" },
      [pscustomobject]@{ ExecutablePath = Join-Path $partialDir 'masamongd.exe'; CommandLine = "masamongd.exe --data-root `"$rootTwo`"" })
    $script:installTestCalls = New-Object 'System.Collections.Generic.List[string]'
    $script:failStopRoot = $rootTwo
    Set-Item Function:\Invoke-Masamong -Value {
      param([string]$Exe, [string[]]$Arguments)
      $script:installTestCalls.Add(($Arguments -join ' '))
      if ($Arguments -contains 'status') { return '{"output":{"running":true}}' }
      if ($Arguments -contains 'stop') {
        $rootIndex = [Array]::IndexOf($Arguments, '--data-root') + 1
        $stoppedRoot = $Arguments[$rootIndex]
        if ($stoppedRoot -eq $script:failStopRoot) { throw 'simulated second stop failure' }
        $script:installTestProcesses = @($script:installTestProcesses | Where-Object { $_.CommandLine -notlike "*$stoppedRoot*" })
      }
      return '{"ok":true}'
    }
    $partialRoots = New-Object 'System.Collections.Generic.List[string]'
    $beforePath = Get-UserPath
    $beforeProcessPath = $env:Path
    $partialFailed = $false
    try { Invoke-InstallTransaction $partialDir @('masamong.exe', 'masamongd.exe') '.new' @('masamong.exe', 'masamongd.exe') $partialRoots $beforePath $beforeProcessPath $false $null }
    catch { $partialFailed = $_.Exception.Message -match 'simulated second stop failure' }
    $partialStarts = @($script:installTestCalls | Where-Object { $_ -match '--data-root .* start$' })
    Check $partialFailed 'a failure while stopping the second instance is reported'
    Check ($partialRoots.Count -eq 2 -and (Get-Content (Join-Path $partialDir 'masamong.exe')) -eq 'old-cli') 'a partial stop keeps the full original root snapshot and old binary pair'
    Check ($partialStarts.Count -eq 2 -and ($partialStarts -join ' ') -match [regex]::Escape($rootOne) -and ($partialStarts -join ' ') -match [regex]::Escape($rootTwo)) 'a partial stop restarts exactly both roots with --data-root'
    Remove-Item -LiteralPath ((Join-Path $partialDir 'masamong.exe') + '.new'),((Join-Path $partialDir 'masamongd.exe') + '.new') -Force
    Remove-Variable failStopRoot -Scope Script -ErrorAction SilentlyContinue
  } finally {
    Set-Item Function:\Stop-InstalledPrograms -Value $oldStop
    Set-Item Function:\Add-UserPathEntry -Value $oldAddPath
    Set-Item Function:\Invoke-Masamong -Value $oldInvoke
    Set-Item Function:\Get-InstalledProcesses -Value $oldGetProcesses
    Remove-Item Env:MASAMONG_DATA_ROOT -ErrorAction SilentlyContinue
  }

  # --- WIN-9: stderr output does not fail a command that succeeded ---
  $out = @(Invoke-Masamong 'cmd.exe' @('/c', 'echo to-stdout& echo to-stderr 1>&2'))
  Check ($out.Count -eq 1 -and "$($out[0])".Trim() -eq 'to-stdout') 'stderr is kept out of the returned output and does not throw'
  $threw = ''
  try { $null = Invoke-Masamong 'cmd.exe' @('/c', 'echo broken 1>&2& exit 3') } catch { $threw = $_.Exception.Message }
  Check ($threw -match 'exit code 3' -and $threw -match 'broken') 'a failing command reports its exit code and stderr'

  # --- WIN-10: instances are found by the --data-root of their processes ---
  $found = Get-InstanceRoots @(
    [pscustomobject]@{ CommandLine = '"C:\p\masamongd.exe" --supervise --data-root "D:\bots\one two" --web' },
    [pscustomobject]@{ CommandLine = 'C:\p\masamong.exe --data-root=E:\bots\three tray' },
    [pscustomobject]@{ CommandLine = 'C:\p\masamongd.exe --web' })
  Check ($found -contains 'D:\bots\one two') 'a quoted --data-root is read'
  Check (($found | Where-Object { $_ -like 'E:\bots\three*' }).Count -eq 1) 'a --data-root=<path> is read'
  Check ($found.Count -le 3) 'the default root is listed once with the others'

  # --- WIN-9: the script parses when it is piped into iex ---
  $env:MASAMONG_INSTALL_LIBRARY = '1'
  $piped = $true
  try { Get-Content -LiteralPath (Join-Path $root 'install.ps1') -Raw | Invoke-Expression } catch { $piped = $false; Write-Host $_.Exception.Message }
  Remove-Item Env:MASAMONG_INSTALL_LIBRARY
  Check $piped 'install.ps1 loads through iex (no script path needed)'
  # --- WIN-11: start-test.ps1 -Reset deletes only a folder the script made ---
  $docs = Join-Path $tmp 'Documents'
  New-Item -ItemType Directory -Path $docs | Out-Null
  Set-Content -LiteralPath (Join-Path $docs 'thesis.txt') -Value 'precious'
  $env:MASAMONG_TRY_ROOT = $docs
  $ErrorActionPreference = 'Continue'
  $resetOut = & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'start-test.ps1') -Reset 2>&1 | Out-String
  $ErrorActionPreference = 'Stop'
  Check ((Test-Path (Join-Path $docs 'thesis.txt')) -and $resetOut -match 'not created by this script') '-Reset refuses a folder without the marker and leaves it alone'
} finally {
  $env:Path = $processPathBefore
  Remove-Item Env:MASAMONG_TRY_ROOT -ErrorAction SilentlyContinue
  Remove-Item Env:MASAMONG_INSTALL_ENV_KEY -ErrorAction SilentlyContinue
  [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($scratchKey, $false)
  Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
if ($realEnvKey) { $realEnvKey.Close() }
$verifyReal = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $false)
$realUserPathAfter = $null
$realUserPathKindAfter = [Microsoft.Win32.RegistryValueKind]::ExpandString
if ($null -ne $verifyReal) {
  try {
    $realUserPathAfter = $verifyReal.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if ($null -ne $realUserPathAfter) { $realUserPathKindAfter = $verifyReal.GetValueKind('Path') }
  } finally { $verifyReal.Close() }
}
Check ($realUserPathAfter -ceq $realUserPathBefore -and $realUserPathKindAfter -eq $realUserPathKindBefore) 'the real user PATH value and registry type are restored exactly'
if ($failures -gt 0) { Write-Host "$failures check(s) failed"; exit 1 }
Write-Host 'all install checks passed'
