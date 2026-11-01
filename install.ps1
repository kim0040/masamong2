# Install or update Masamong for the current Windows user.
#   powershell -ExecutionPolicy Bypass -File install.ps1
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Prefix C:\Apps\Masamong
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall -RemoveKeys
#   irm https://raw.githubusercontent.com/kim0040/masamong2/main/install.ps1 | iex
# -RemoveKeys also deletes the API keys Masamong saved in Windows Credential Manager (masamong/...).
param(
  [switch]$Uninstall,
  [switch]$RemoveKeys,
  [switch]$Force,
  [string]$Prefix
)

$ErrorActionPreference = 'Stop'
$repo = 'kim0040/masamong2'
$script:ManifestName = '.masamong-manifest.json'
$script:KnownInstallFiles = @('masamong.exe', 'masamongd.exe', 'LICENSE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_LICENSES.txt')
# Windows PowerShell 5.1 offers TLS 1.0/1.1 by default; GitHub needs 1.2.
try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }
# Where the user PATH lives; tests point it at a scratch key.
$script:UserEnvironmentKey = 'Environment'
if ($env:MASAMONG_INSTALL_ENV_KEY) { $script:UserEnvironmentKey = $env:MASAMONG_INSTALL_ENV_KEY }

function Normalize-PathEntry([string]$Value) {
  if ([string]::IsNullOrWhiteSpace($Value)) { return '' }
  $expanded = [Environment]::ExpandEnvironmentVariables($Value.Trim())
  try { $full = [IO.Path]::GetFullPath($expanded) } catch { $full = $expanded }
  $root = [IO.Path]::GetPathRoot($full)
  if ($full.Length -gt $root.Length) { $full = $full.TrimEnd([char[]]@('\', '/')) }
  return $full
}

function Assert-SafePrefix([string]$Path) {
  $full = Normalize-PathEntry $Path
  if ([string]::IsNullOrWhiteSpace($full)) { throw 'Install prefix is empty.' }
  $broad = New-Object 'System.Collections.Generic.List[string]'
  $root = [IO.Path]::GetPathRoot($full)
  if ($root) { $broad.Add((Normalize-PathEntry $root)) }
  foreach ($candidate in @(
    [Environment]::GetFolderPath('UserProfile'),
    $env:USERPROFILE,
    $env:ProgramFiles,
    [Environment]::GetEnvironmentVariable('ProgramFiles(x86)'),
    $env:ProgramW6432,
    $env:SystemRoot,
    $env:WINDIR
  )) {
    if (-not [string]::IsNullOrWhiteSpace($candidate)) { $broad.Add((Normalize-PathEntry $candidate)) }
  }
  foreach ($path in $broad) {
    if ([string]::Equals($full, $path, [StringComparison]::OrdinalIgnoreCase)) {
      throw "Refusing broad install prefix '$full'. Choose a dedicated Masamong directory."
    }
  }
  return $full
}

function Read-InstallManifest([string]$Dir) {
  $path = Join-Path $Dir $script:ManifestName
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { return $null }
  try {
    $manifest = Get-Content -LiteralPath $path -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
    if ($manifest.version -ne 1 -or $null -eq $manifest.files) { return $null }
    $files = New-Object 'System.Collections.Generic.List[string]'
    foreach ($name in @($manifest.files)) {
      $leaf = [string]$name
      if ($script:KnownInstallFiles -contains $leaf -and -not $files.Contains($leaf)) { $files.Add($leaf) }
    }
    if ($files.Count -eq 0) { return $null }
    return @($files)
  } catch { return $null }
}

function Get-ManagedFiles([string]$Dir) {
  $files = Read-InstallManifest $Dir
  if ($null -eq $files) { $files = @($script:KnownInstallFiles) }
  $all = New-Object 'System.Collections.Generic.List[string]'
  foreach ($name in $files) { if (-not $all.Contains([string]$name)) { $all.Add([string]$name) } }
  $all.Add($script:ManifestName)
  return @($all)
}

function Remove-ManagedFiles([string]$Dir) {
  if (-not (Test-Path -LiteralPath $Dir -PathType Container)) { return $true }
  foreach ($name in Get-ManagedFiles $Dir) {
    $path = Join-Path $Dir $name
    if (Test-Path -LiteralPath $path -PathType Leaf) { Remove-Item -LiteralPath $path -Force -ErrorAction Stop }
  }
  if (@(Get-ChildItem -LiteralPath $Dir -Force -ErrorAction Stop).Count -eq 0) {
    Remove-Item -LiteralPath $Dir -Force -ErrorAction Stop
  }
  return -not (Test-Path -LiteralPath $Dir -PathType Container)
}

function Assert-InstallDirectory([string]$Dir, [switch]$AllowNonEmpty) {
  if (-not (Test-Path -LiteralPath $Dir -PathType Container)) { return }
  if ($null -ne (Read-InstallManifest $Dir)) { return }
  $items = @(Get-ChildItem -LiteralPath $Dir -Force -ErrorAction Stop)
  if ($items.Count -gt 0 -and -not $AllowNonEmpty) {
    throw "Install directory '$Dir' is not empty and has no Masamong manifest. Review its contents and rerun with -Force to use it."
  }
}

function Has-PathEntry([string]$PathValue, [string]$Entry) {
  if ($null -eq $PathValue) { return $false }
  $want = Normalize-PathEntry $Entry
  foreach ($part in $PathValue.Split([char]';', [StringSplitOptions]::None)) {
    if (-not [string]::IsNullOrWhiteSpace($part) -and
        [string]::Equals((Normalize-PathEntry $part), $want, [StringComparison]::OrdinalIgnoreCase)) { return $true }
  }
  return $false
}

# The user PATH is read and written through the registry value itself. [Environment]::GetEnvironmentVariable
# expands %VARS% and SetEnvironmentVariable stores a plain REG_SZ, which would turn
# %USERPROFILE%\... entries into fixed paths and change the value type.
function Get-UserPath {
  $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($script:UserEnvironmentKey, $false)
  if ($null -eq $key) { return [pscustomobject]@{ Value = $null; Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString } }
  try {
    $value = $key.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
    if ($null -ne $value) { $kind = $key.GetValueKind('Path') }
    return [pscustomobject]@{ Value = $value; Kind = $kind }
  } finally { $key.Close() }
}

function Set-UserPath([string]$Value, [Microsoft.Win32.RegistryValueKind]$Kind) {
  $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($script:UserEnvironmentKey)
  try { $key.SetValue('Path', $Value, $Kind) } finally { $key.Close() }
  if ($script:UserEnvironmentKey -eq 'Environment') { Send-EnvironmentChange }
}

# Tell running programs (Explorer, so new terminals) that the environment changed.
function Send-EnvironmentChange {
  try {
    if (-not ('Masamong.NativeMethods' -as [type])) {
      Add-Type -Namespace Masamong -Name NativeMethods -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("user32.dll", SetLastError = true, CharSet = System.Runtime.InteropServices.CharSet.Auto)]
public static extern System.IntPtr SendMessageTimeout(System.IntPtr hWnd, uint Msg, System.UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out System.UIntPtr lpdwResult);
'@
    }
    $result = [UIntPtr]::Zero
    $null = [Masamong.NativeMethods]::SendMessageTimeout([IntPtr]0xffff, 0x001A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$result)
  } catch {
    Write-Warning "Could not tell running programs about the PATH change; open a new terminal after signing out and in again if 'masamong' is not found. ($($_.Exception.Message))"
  }
}

function Add-UserPathEntry([string]$Entry) {
  $current = Get-UserPath
  if (-not (Has-PathEntry $current.Value $Entry)) {
    $newPath = if ([string]::IsNullOrEmpty($current.Value)) { $Entry } else { $current.Value.TrimEnd(';') + ';' + $Entry }
    Set-UserPath $newPath $current.Kind
    $after = Get-UserPath
    if (-not [string]::Equals($after.Value, $newPath, [StringComparison]::Ordinal)) { throw 'Could not update the current user PATH.' }
  }
  if (-not (Has-PathEntry $env:Path $Entry)) {
    $env:Path = if ([string]::IsNullOrEmpty($env:Path)) { $Entry } else { $env:Path + ';' + $Entry }
  }
}

function Remove-PathEntry([string]$PathValue, [string]$Entry) {
  if ($null -eq $PathValue) { return $null }
  $want = Normalize-PathEntry $Entry
  $kept = New-Object 'System.Collections.Generic.List[string]'
  foreach ($part in $PathValue.Split([char]';', [StringSplitOptions]::None)) {
    if ([string]::IsNullOrWhiteSpace($part) -or
        -not [string]::Equals((Normalize-PathEntry $part), $want, [StringComparison]::OrdinalIgnoreCase)) {
      $kept.Add($part)
    }
  }
  return [string]::Join(';', $kept)
}

function Remove-UserPathEntry([string]$Entry) {
  $current = Get-UserPath
  $newPath = Remove-PathEntry $current.Value $Entry
  if ($null -ne $current.Value -and -not [string]::Equals($current.Value, $newPath, [StringComparison]::Ordinal)) {
    Set-UserPath $newPath $current.Kind
  }
}

# Runs a program and returns what it printed on standard output. Standard error is kept apart: under
# $ErrorActionPreference = 'Stop', Windows PowerShell 5.1 turns a single stderr line into a terminating
# error, and masamong logs progress there.
function Invoke-Masamong([string]$Exe, [string[]]$Arguments) {
  $previous = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $all = @(& $Exe @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
  } finally { $ErrorActionPreference = $previous }
  $out = @($all | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] })
  if ($exitCode -ne 0) {
    $text = ($all | ForEach-Object { "$_" }) -join ' '
    throw "'$Exe $($Arguments -join ' ')' failed with exit code $exitCode. $text"
  }
  return $out
}

function Get-InstalledProcesses([string]$CliPath, [string]$DaemonPath) {
  $cliFull = Normalize-PathEntry $CliPath
  $daemonFull = Normalize-PathEntry $DaemonPath
  Get-CimInstance -ClassName Win32_Process -ErrorAction Stop | Where-Object {
    $exe = Normalize-PathEntry ([string]$_.ExecutablePath)
    ($exe -and [string]::Equals($exe, $daemonFull, [StringComparison]::OrdinalIgnoreCase)) -or
    ($exe -and [string]::Equals($exe, $cliFull, [StringComparison]::OrdinalIgnoreCase) -and
      ([string]$_.CommandLine) -match '(?i)(?:^|\s)tray(?:\s|$)')
  }
}

# The data folders of every instance that is (or may be) running: the default one, plus each folder named
# by --data-root on the command line of a running daemon or tray.
function Get-InstanceRoots($Processes) {
  $roots = New-Object 'System.Collections.Generic.List[string]'
  $default = $env:MASAMONG_DATA_ROOT
  if ([string]::IsNullOrWhiteSpace($default) -and -not [string]::IsNullOrWhiteSpace($env:APPDATA)) { $default = Join-Path $env:APPDATA 'Masamong' }
  if (-not [string]::IsNullOrWhiteSpace($default)) { $roots.Add((Normalize-PathEntry $default)) }
  foreach ($process in $Processes) {
    $m = [regex]::Match([string]$process.CommandLine, '--data-root(?:=|\s+)(?:"([^"]+)"|(\S+))')
    if ($m.Success) {
      $root = if ($m.Groups[1].Success) { $m.Groups[1].Value } else { $m.Groups[2].Value }
      $full = Normalize-PathEntry $root
      if ($full -and -not ($roots | Where-Object { [string]::Equals($_, $full, [StringComparison]::OrdinalIgnoreCase) })) { $roots.Add($full) }
    }
  }
  return @($roots)
}

# Stops every Masamong instance that runs from InstallDir. RunningRoots is filled from a complete status
# snapshot before the first stop, so a failure partway through still leaves an exact restart list.
function Stop-InstalledPrograms([string]$InstallDir, [System.Collections.Generic.List[string]]$RunningRoots, [switch]$RemoveAutostart) {
  $cli = Join-Path $InstallDir 'masamong.exe'
  $daemon = Join-Path $InstallDir 'masamongd.exe'
  $cliExists = Test-Path -LiteralPath $cli -PathType Leaf
  $roots = @()
  if ($cliExists) { $roots = Get-InstanceRoots @(Get-InstalledProcesses $cli $daemon) }

  $RunningRoots.Clear()
  foreach ($root in $roots) {
    if (-not (Test-Path -LiteralPath $root -PathType Container)) { continue }
    $statusText = (Invoke-Masamong $cli @('--output', 'json', '--data-root', $root, 'status') | Out-String).Trim()
    $status = $statusText | ConvertFrom-Json -ErrorAction Stop
    if ($status.output.running -eq $true) { $RunningRoots.Add($root) }
  }

  foreach ($root in $roots) {
    if (-not (Test-Path -LiteralPath $root -PathType Container)) { continue }
    if ($RemoveAutostart) { $null = Invoke-Masamong $cli @('--output', 'json', '--data-root', $root, 'autostart', 'off') }
    if ($RunningRoots.Contains($root)) { $null = Invoke-Masamong $cli @('--output', 'json', '--data-root', $root, 'stop') }
  }

  # `stop` also closes the tray; wait for a clean exit before ending anything.
  $graceful = [DateTime]::UtcNow.AddSeconds(20)
  do {
    $remaining = @(Get-InstalledProcesses $cli $daemon)
    if ($remaining.Count -eq 0) { return }
    Start-Sleep -Milliseconds 250
  } while ([DateTime]::UtcNow -lt $graceful)
  foreach ($process in $remaining) {
    if ([int]$process.ProcessId -eq $PID) { continue }
    Write-Warning "Process $($process.ProcessId) did not exit after 'masamong stop'; ending it."
    Stop-Process -Id ([int]$process.ProcessId) -Force -ErrorAction SilentlyContinue
  }
  $deadline = [DateTime]::UtcNow.AddSeconds(25)
  do {
    $remaining = @(Get-InstalledProcesses $cli $daemon)
    if ($remaining.Count -eq 0) { return }
    Start-Sleep -Milliseconds 250
  } while ([DateTime]::UtcNow -lt $deadline)
  $ids = ($remaining | ForEach-Object { $_.ProcessId }) -join ', '
  throw "Masamong tray or daemon processes did not exit (process IDs: $ids). Close them and retry."
}

function Assert-Copy([string]$Source, [string]$Destination) {
  Copy-Item -LiteralPath $Source -Destination $Destination -Force -ErrorAction Stop
  if (-not (Test-Path -LiteralPath $Destination -PathType Leaf)) { throw "Copy did not create $Destination" }
  $sourceHash = (Get-FileHash -LiteralPath $Source -Algorithm SHA256).Hash
  $destHash = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash
  if ($sourceHash -ne $destHash) { throw "Copied file did not verify: $Destination" }
}

# Proves the install folder can be written (creating it when it is missing) before anything is stopped.
function Assert-Writable([string]$Dir) {
  $probe = Join-Path $Dir ('.masamong-write-test-' + [guid]::NewGuid().ToString('N'))
  try {
    New-Item -ItemType Directory -Force -Path $Dir -ErrorAction Stop | Out-Null
    Set-Content -LiteralPath $probe -Value 'x' -ErrorAction Stop
    Remove-Item -LiteralPath $probe -Force -ErrorAction Stop
  } catch {
    throw "Cannot write to $Dir ($($_.Exception.Message)). Nothing was stopped or changed. Run PowerShell as administrator for this folder, or choose another one with -Prefix <folder>."
  }
}

# Replaces the installed programs with the staged ones. The old files are renamed aside first and put
# back if anything fails, so a failed update leaves the previous version in place.
function Install-Staged([string]$Dir, [string[]]$Names, [string]$StageSuffix, [switch]$KeepBackup) {
  $moved = New-Object 'System.Collections.Generic.List[string]'
  $hadOriginal = @{}
  try {
    foreach ($name in $Names) {
      $target = Join-Path $Dir $name
      if (Test-Path -LiteralPath $target -PathType Leaf) {
        $hadOriginal[$name] = $true
        Move-Item -LiteralPath $target -Destination ($target + '.old') -Force -ErrorAction Stop
        $moved.Add($name)
      }
      Move-Item -LiteralPath ($target + $StageSuffix) -Destination $target -Force -ErrorAction Stop
    }
  } catch {
    foreach ($name in $Names) {
      $target = Join-Path $Dir $name
      if ($hadOriginal.ContainsKey($name) -and (Test-Path -LiteralPath ($target + '.old') -PathType Leaf)) {
        Remove-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
        Move-Item -LiteralPath ($target + '.old') -Destination $target -Force -ErrorAction SilentlyContinue
      } elseif (-not $hadOriginal.ContainsKey($name)) {
        Remove-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
      }
      Remove-Item -LiteralPath ($target + $StageSuffix) -Force -ErrorAction SilentlyContinue
    }
    throw
  }
  if (-not $KeepBackup) {
    foreach ($name in $moved) { Remove-Item -LiteralPath ((Join-Path $Dir $name) + '.old') -Force -ErrorAction SilentlyContinue }
  }
}

function Restore-Staged([string]$Dir, [string[]]$Names, [string]$StageSuffix, [string[]]$PreviouslyPresent) {
  $had = @{}
  foreach ($name in $PreviouslyPresent) { $had[$name] = $true }
  foreach ($name in $Names) {
    $target = Join-Path $Dir $name
    $backup = $target + '.old'
    if ($had.ContainsKey($name)) {
      if (Test-Path -LiteralPath $backup -PathType Leaf) {
        Remove-Item -LiteralPath $target -Force -ErrorAction Stop
        Move-Item -LiteralPath $backup -Destination $target -Force -ErrorAction Stop
      } elseif (-not (Test-Path -LiteralPath $target -PathType Leaf)) {
        throw "The original install file could not be restored: $target"
      }
    } else {
      Remove-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath ($target + $StageSuffix) -Force -ErrorAction SilentlyContinue
  }
}

function Complete-Staged([string]$Dir, [string[]]$Names, [string]$StageSuffix) {
  foreach ($name in $Names) {
    Remove-Item -LiteralPath ((Join-Path $Dir $name) + '.old') -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath ((Join-Path $Dir $name) + $StageSuffix) -Force -ErrorAction SilentlyContinue
  }
}

function Write-InstallManifest([string]$Dir, [string[]]$Files) {
  $path = Join-Path $Dir $script:ManifestName
  $backup = $path + '.old'
  $temp = $path + '.new'
  if (Test-Path -LiteralPath $backup) { throw "An unfinished manifest recovery file exists: $backup" }
  $safeFiles = New-Object 'System.Collections.Generic.List[string]'
  foreach ($name in $Files) {
    if ($script:KnownInstallFiles -contains [string]$name -and -not $safeFiles.Contains([string]$name)) { $safeFiles.Add([string]$name) }
  }
  if (Test-Path -LiteralPath $path -PathType Leaf) { Move-Item -LiteralPath $path -Destination $backup -Force -ErrorAction Stop }
  try {
    $json = @{ version = 1; files = @($safeFiles) } | ConvertTo-Json -Depth 4 -Compress
    [IO.File]::WriteAllText($temp, $json + "`n", (New-Object System.Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $temp -Destination $path -Force -ErrorAction Stop
  } catch {
    Remove-Item -LiteralPath $temp -Force -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $backup -PathType Leaf) {
      Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
      Move-Item -LiteralPath $backup -Destination $path -Force -ErrorAction SilentlyContinue
    }
    throw
  }
}

function Restore-InstallManifest([string]$Dir, [bool]$PreviouslyPresent) {
  $path = Join-Path $Dir $script:ManifestName
  $backup = $path + '.old'
  Remove-Item -LiteralPath ($path + '.new') -Force -ErrorAction SilentlyContinue
  if ($PreviouslyPresent) {
    if (Test-Path -LiteralPath $backup -PathType Leaf) {
      Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
      Move-Item -LiteralPath $backup -Destination $path -Force -ErrorAction Stop
    } elseif (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
      throw "The original install manifest could not be restored: $path"
    }
  } else {
    Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
  }
}

function Complete-InstallManifest([string]$Dir) {
  $path = Join-Path $Dir $script:ManifestName
  Remove-Item -LiteralPath ($path + '.old') -Force -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath ($path + '.new') -Force -ErrorAction SilentlyContinue
}

function Restore-UserPath($Before, [string]$ProcessPathBefore) {
  $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($script:UserEnvironmentKey)
  try {
    if ($null -eq $Before.Value) { $key.DeleteValue('Path', $false) }
    else { $key.SetValue('Path', [string]$Before.Value, [Microsoft.Win32.RegistryValueKind]$Before.Kind) }
  } finally { $key.Close() }
  if ($script:UserEnvironmentKey -eq 'Environment') { Send-EnvironmentChange }
  $env:Path = $ProcessPathBefore
}

function Start-HealthyInstances([string]$Cli, [string[]]$Roots) {
  foreach ($root in $Roots) {
    $null = Invoke-Masamong $Cli @('--output', 'json', '--data-root', $root, 'start')
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    $healthy = $false
    $last = ''
    do {
      try {
        $statusText = (Invoke-Masamong $Cli @('--output', 'json', '--data-root', $root, 'status') | Out-String).Trim()
        $status = $statusText | ConvertFrom-Json -ErrorAction Stop
        if ($status.output.running -eq $true) { $healthy = $true; break }
        $last = 'the daemon did not report running'
      } catch { $last = $_.Exception.Message }
      Start-Sleep -Milliseconds 250
    } while ([DateTime]::UtcNow -lt $deadline)
    if (-not $healthy) { throw "Masamong did not become healthy for data root '$root': $last" }
  }
}

function Assert-NoInstallRecoveryFiles([string]$Dir, [string[]]$Names, [switch]$Staged) {
  foreach ($name in $Names) {
    $suffixes = if ($Staged) { @('.old') } else { @('.old', '.new') }
    foreach ($suffix in $suffixes) {
      $path = (Join-Path $Dir $name) + $suffix
      if (Test-Path -LiteralPath $path) { throw "An unfinished install recovery file exists: $path" }
    }
  }
  $manifest = Join-Path $Dir $script:ManifestName
  foreach ($suffix in @('.old', '.new')) {
    if (Test-Path -LiteralPath ($manifest + $suffix)) { throw "An unfinished install recovery file exists: $($manifest + $suffix)" }
  }
}

# One recovery boundary covers the stop, file swap, PATH update, and health checks. The caller stages
# and verifies every input before entering this function, so all recoverable work starts only after that.
function Invoke-InstallTransaction([string]$Dir, [string[]]$Names, [string]$StageSuffix, [string[]]$ManagedFiles,
    [System.Collections.Generic.List[string]]$RunningRoots, $UserPathBefore, [string]$ProcessPathBefore,
    [bool]$ManifestPreviouslyPresent, [string[]]$OldManagedFiles) {
  Assert-NoInstallRecoveryFiles $Dir $Names -Staged
  $previouslyPresent = @($Names | Where-Object { Test-Path -LiteralPath (Join-Path $Dir $_) -PathType Leaf })
  $staged = $false
  $manifestWritten = $false
  $pathAttempted = $false
  $startAttempted = $false
  $cli = Join-Path $Dir 'masamong.exe'
  try {
    Stop-InstalledPrograms $Dir $RunningRoots
    Install-Staged $Dir $Names $StageSuffix -KeepBackup
    $staged = $true
    $files = New-Object 'System.Collections.Generic.List[string]'
    foreach ($name in @($OldManagedFiles) + @($ManagedFiles)) {
      if ($script:KnownInstallFiles -contains [string]$name -and -not $files.Contains([string]$name)) { $files.Add([string]$name) }
    }
    Write-InstallManifest $Dir @($files)
    $manifestWritten = $true
    $pathAttempted = $true
    Add-UserPathEntry $Dir
    if (-not (Has-PathEntry (Get-UserPath).Value $Dir) -or -not (Has-PathEntry $env:Path $Dir)) {
      throw "PATH verification failed for $Dir."
    }
    if ($RunningRoots.Count -gt 0) {
      $startAttempted = $true
      Start-HealthyInstances $cli @($RunningRoots)
    }
    Complete-Staged $Dir $Names $StageSuffix
    Complete-InstallManifest $Dir
  } catch {
    $failure = $_
    $recoveryErrors = New-Object 'System.Collections.Generic.List[string]'
    if ($startAttempted) {
      try {
        $newRoots = New-Object 'System.Collections.Generic.List[string]'
        Stop-InstalledPrograms $Dir $newRoots
      } catch { $recoveryErrors.Add("stopping the new service: $($_.Exception.Message)") }
    }
    if ($staged) {
      try { Restore-Staged $Dir $Names $StageSuffix $previouslyPresent }
      catch { $recoveryErrors.Add("restoring the previous executables: $($_.Exception.Message)") }
    }
    if ($manifestWritten -or (Test-Path -LiteralPath ((Join-Path $Dir $script:ManifestName) + '.old'))) {
      try { Restore-InstallManifest $Dir $ManifestPreviouslyPresent }
      catch { $recoveryErrors.Add("restoring the previous manifest: $($_.Exception.Message)") }
    }
    if ($pathAttempted) {
      try { Restore-UserPath $UserPathBefore $ProcessPathBefore }
      catch { $recoveryErrors.Add("restoring the user PATH: $($_.Exception.Message)") }
    }
    if ($RunningRoots.Count -gt 0) {
      try { Start-HealthyInstances $cli @($RunningRoots) }
      catch { $recoveryErrors.Add("restarting the previous data roots: $($_.Exception.Message)") }
    }
    if ($recoveryErrors.Count -gt 0) {
      throw [InvalidOperationException]::new("Install failed: $($failure.Exception.Message) Recovery also failed: $($recoveryErrors -join '; ')", $failure.Exception)
    }
    throw $failure
  }
}

function Get-GoVersion([string]$VersionText) {
  $value = $VersionText -replace '^go', ''
  if ($value -notmatch '^\d+\.\d+(?:\.\d+)?') { throw "Could not read Go version '$VersionText'." }
  return [version]$Matches[0]
}

# The tag of the newest release, from the redirect of github.com/<repo>/releases/latest (no API quota).
function Resolve-LatestTag {
  try {
    $request = [Net.HttpWebRequest]::Create("https://github.com/$repo/releases/latest")
    $request.AllowAutoRedirect = $false
    $request.UserAgent = 'Masamong-Installer'
    $response = $request.GetResponse()
    try {
      $location = [string]$response.Headers['Location']
    } finally { $response.Close() }
    if ($location -match '/releases/tag/([^/?#]+)$') { return [Uri]::UnescapeDataString($Matches[1]) }
  } catch { }
  try {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ 'User-Agent' = 'Masamong-Installer' }
    return [string]$release.tag_name
  } catch { }
  return ''
}

function Get-ReleaseBinaries([string]$OutputDir) {
  $archName = $env:PROCESSOR_ARCHITEW6432
  if ([string]::IsNullOrWhiteSpace($archName)) { $archName = $env:PROCESSOR_ARCHITECTURE }
  switch -Regex ($archName) {
    '^(ARM64)$' { $arch = 'arm64'; break }
    '^(AMD64|x64)$' { $arch = 'amd64'; break }
    default { throw "Unsupported Windows architecture '$archName' (supported: amd64, arm64)." }
  }
  $archiveName = "masamong_windows_$arch.zip"
  $requested = $env:MASAMONG_VERSION
  if ([string]::IsNullOrWhiteSpace($requested) -or $requested -eq 'latest') {
    $tag = Resolve-LatestTag
    if ([string]::IsNullOrWhiteSpace($tag)) { throw 'Could not find the latest Masamong release on GitHub. Check the network, or set MASAMONG_VERSION to a release tag.' }
  } else {
    $tag = $requested
  }
  # every file comes from the one tag resolved here, so the archive and its checksums cannot be from
  # two different releases
  $base = "https://github.com/$repo/releases/download/$([Uri]::EscapeDataString($tag))"

  $zipPath = Join-Path $OutputDir $archiveName
  $sumsPath = Join-Path $OutputDir 'SHA256SUMS'
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$archiveName" -OutFile $zipPath -Headers @{ 'User-Agent' = 'Masamong-Installer' }
  Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sumsPath -Headers @{ 'User-Agent' = 'Masamong-Installer' }
  $sumLine = Get-Content -LiteralPath $sumsPath | Where-Object { $_ -match ('^([0-9a-fA-F]{64})\s+\*?' + [regex]::Escape($archiveName) + '\s*$') } | Select-Object -First 1
  if (-not $sumLine) { throw "SHA256SUMS has no checksum for $archiveName." }
  $expectedHash = ([regex]::Match($sumLine, '^([0-9a-fA-F]{64})')).Groups[1].Value.ToUpperInvariant()
  $actualHash = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToUpperInvariant()
  if ($actualHash -ne $expectedHash) { throw "Checksum verification failed for $archiveName." }

  $extract = Join-Path $OutputDir 'release'
  Expand-Archive -LiteralPath $zipPath -DestinationPath $extract -Force
  $releaseDir = Join-Path $extract "masamong_windows_$arch"
  foreach ($name in @('masamong.exe', 'masamongd.exe')) {
    if (-not (Test-Path -LiteralPath (Join-Path $releaseDir $name) -PathType Leaf)) {
      throw "The verified release archive is missing $name."
    }
    Assert-Copy (Join-Path $releaseDir $name) (Join-Path $OutputDir $name)
  }
  return $releaseDir
}

# Credential Manager entries Masamong wrote: target "masamong/<instance>/<key id>".
function Get-MasamongCredentials {
  $targets = New-Object 'System.Collections.Generic.List[string]'
  $listing = @(& cmdkey.exe '/list:masamong/*' 2>$null)
  foreach ($line in $listing) {
    if ("$line" -match '(?i)Target:\s*(?:LegacyGeneric:target=)?(masamong/\S+)') { $targets.Add($Matches[1]) }
  }
  return @($targets)
}

# Used by tests: load the functions above and stop.
if ($env:MASAMONG_INSTALL_LIBRARY -eq '1') { return }

# $MyInvocation.MyCommand.Path is empty when the script is piped into iex.
$here = ''
if ($MyInvocation.MyCommand.Path) { $here = Split-Path -Parent $MyInvocation.MyCommand.Path }
if ([string]::IsNullOrWhiteSpace($Prefix)) {
  if ([string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) { throw 'LOCALAPPDATA is not set; pass -Prefix <install-directory>.' }
  $Prefix = Join-Path $env:LOCALAPPDATA 'Programs\Masamong'
}
$dest = Assert-SafePrefix $Prefix
if (-not $Uninstall) { Assert-InstallDirectory $dest -AllowNonEmpty:$Force }

if ($Uninstall) {
  $runningRoots = New-Object 'System.Collections.Generic.List[string]'
  Stop-InstalledPrograms $dest $runningRoots -RemoveAutostart
  $directoryRemoved = Remove-ManagedFiles $dest
  Remove-UserPathEntry $dest
  $env:Path = Remove-PathEntry $env:Path $dest
  if (Has-PathEntry (Get-UserPath).Value $dest) { throw "Uninstall did not remove $dest from the user PATH." }
  if (-not $directoryRemoved) { Write-Host "Removed Masamong-managed files from $dest; kept the directory because it still contains other files." }
  else { Write-Host "Removed Masamong from $dest. Your data folder (%APPDATA%\Masamong) was kept." }
  # API keys saved in Windows Credential Manager stay unless asked for: they belong to the data folder
  # that was kept, and removing them would leave its settings pointing at nothing.
  $keys = @(Get-MasamongCredentials)
  if ($keys.Count -gt 0 -and $RemoveKeys) {
    foreach ($target in $keys) { & cmdkey.exe "/delete:$target" | Out-Null }
    Write-Host "Removed $($keys.Count) saved key(s) from Windows Credential Manager."
  } elseif ($keys.Count -gt 0) {
    Write-Host "$($keys.Count) saved API key(s) remain in Windows Credential Manager (masamong/...)."
    Write-Host '  Remove them too: run this again with -Uninstall -RemoveKeys, or delete them in Control Panel > Credential Manager.'
  }
  return
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("masamong-install-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -ErrorAction Stop | Out-Null
$runningRoots = New-Object 'System.Collections.Generic.List[string]'
$installedCli = Join-Path $dest 'masamong.exe'
$exeNames = @('masamong.exe', 'masamongd.exe')
$installNames = @($exeNames)
$managedFiles = @($exeNames)
try {
  $cliSource = Join-Path $tmp 'masamong.exe'
  $daemonSource = Join-Path $tmp 'masamongd.exe'
  $noticesRoot = $here
  $localCli = if ($here) { Join-Path $here 'masamong.exe' } else { '' }
  $localDaemon = if ($here) { Join-Path $here 'masamongd.exe' } else { '' }

  if ($here -and (Test-Path -LiteralPath $localCli -PathType Leaf) -and (Test-Path -LiteralPath $localDaemon -PathType Leaf)) {
    Assert-Copy $localCli $cliSource
    Assert-Copy $localDaemon $daemonSource
  } elseif ($here -and (Test-Path -LiteralPath (Join-Path $here 'go.mod') -PathType Leaf) -and (Get-Command go -ErrorAction SilentlyContinue)) {
    $mod = Get-Content -LiteralPath (Join-Path $here 'go.mod') -Raw
    if ($mod -notmatch '(?m)^go\s+(\d+\.\d+(?:\.\d+)?)\s*$') { throw 'Could not read the required Go version from go.mod.' }
    $required = [version]$Matches[1]
    $installed = Get-GoVersion (& go env GOVERSION)
    if ($installed -lt $required) { throw "Go $required or newer is required (found $installed). Install an update from https://go.dev/dl/." }
    $version = 'dev'
    if (Get-Command git -ErrorAction SilentlyContinue) {
      $gitVersion = (& git -C $here describe --tags --always --dirty 2>$null)
      if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($gitVersion)) { $version = $gitVersion }
    }
    $ldflags = "-s -w -X masamong/internal/application.Version=$($version.Trim())"
    Push-Location $here
    try {
      & go build -trimpath -ldflags $ldflags -o $cliSource ./cmd/masamong
      if ($LASTEXITCODE -ne 0) { throw 'Building masamong.exe failed.' }
      & go build -trimpath -ldflags $ldflags -o $daemonSource ./cmd/masamongd
      if ($LASTEXITCODE -ne 0) { throw 'Building masamongd.exe failed.' }
    } finally { Pop-Location }
  } else {
    $noticesRoot = Get-ReleaseBinaries $tmp
  }

  foreach ($exe in @($cliSource, $daemonSource)) {
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { throw "Expected installer input is missing: $exe" }
  }
  $null = Invoke-Masamong $cliSource @('--output', 'json', 'version')

  # Everything that can fail without touching the running install happens before it is stopped: the
  # folder is proven writable and the new programs/notices are staged beside the old ones.
  Assert-Writable $dest
  $noticeNames = @('LICENSE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_LICENSES.txt')
  Assert-NoInstallRecoveryFiles $dest (@($exeNames) + @($noticeNames))
  foreach ($name in $exeNames) { Assert-Copy (Join-Path $tmp $name) ((Join-Path $dest $name) + '.new') }
  foreach ($notice in $noticeNames) {
    $source = if ($noticesRoot) { Join-Path $noticesRoot $notice } else { '' }
    if ($source -and (Test-Path -LiteralPath $source -PathType Leaf)) {
      Assert-Copy $source ((Join-Path $dest $notice) + '.new')
      $installNames += $notice
      $managedFiles += $notice
    }
  }
  foreach ($name in $exeNames) {
    $staged = (Join-Path $dest $name) + '.new'
    if (-not (Test-Path -LiteralPath $staged -PathType Leaf)) { throw "Install staging failed: $staged is missing." }
  }
  $pathBefore = Get-UserPath
  $processPathBefore = $env:Path
  $manifestPath = Join-Path $dest $script:ManifestName
  $manifestPreviouslyPresent = Test-Path -LiteralPath $manifestPath -PathType Leaf
  $oldManagedFiles = Read-InstallManifest $dest
  Invoke-InstallTransaction $dest $installNames '.new' $managedFiles $runningRoots $pathBefore $processPathBefore $manifestPreviouslyPresent $oldManagedFiles
} finally {
  foreach ($name in @($exeNames) + @($noticeNames)) { Remove-Item -LiteralPath ((Join-Path $dest $name) + '.new') -Force -ErrorAction SilentlyContinue }
  if (Test-Path -LiteralPath $tmp) {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction Stop
    if (Test-Path -LiteralPath $tmp) { throw "Installer temporary folder was not removed: $tmp" }
  }
}

Write-Host "Installed and verified Masamong in $dest."
if ($runningRoots.Count -gt 0) { Write-Host 'Restarted every previously running service with the new version.' }
$exePath = Join-Path $dest 'masamong.exe'
$quotedExe = "'" + $exePath.Replace("'", "''") + "'"
Write-Host ''
Write-Host 'Next:'
Write-Host "  Same window: & $quotedExe open"
Write-Host '  New terminal: masamong open'
