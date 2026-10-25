#Requires -RunAsAdministrator

# ────────────────────────────────────────────────────────────
# sdwan Windows uninstaller (C3: exact, verified, fail-closed)
#
# One-liner (admin PowerShell, via GitHub proxy):
#   iwr -useb https://ghproxy.com/https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.ps1 | iex
#
# Behaviour:
#   * Only the two exact managed executables under the install dir are ever
#     stopped, matched by Win32_Process.ExecutablePath (OrdinalIgnoreCase).
#   * The iwan1 Wintun adapter is deleted only after process release is proven
#     and the adapter is proven to be a single sdwan-owned Wintun adapter.
#   * Config and token are kept by default; pass -RemoveConfig to delete them.
#   * The install directory is pruned only when it is empty; it is never
#     recursively wiped over unknown files.
# ────────────────────────────────────────────────────────────

param(
    [string]$InstallDir = "C:\ProgramData\sdwan",
    [string]$ShortcutPath = (Join-Path $env:ProgramData "Microsoft\Windows\Start Menu\Programs\SDWAN Panel.lnk"),
    [string]$ControlAddress = "127.0.0.1:17890",
    [string]$AdapterName = "iwan1",
    [switch]$RemoveConfig,
    [switch]$RemoveLogs,
    [switch]$SkipUninstall
)

# Every step is checked; nothing is silently swallowed.
$ErrorActionPreference = "Stop"

$TASK_NAME = "SDWAN Panel"
$MANAGED_ARTIFACTS = @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")
$BINARY_FILE_NAMES = @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll", "tray.ico")
$PROTECTED_FILE_NAMES = @("iwan.conf", "config.json", "control.token")
$LOG_FILE_NAMES = @("sdwan.log", "sdwan.log.old", "panel.log")
$PROCESS_ENUM_CANDIDATE_NAMES = @("panel.exe", "sdwan-windows-amd64.exe")
$STOP_GRACEFUL_WAIT_SECONDS = 10

$script:Failures = @()

function Add-Failure {
    param([Parameter(Mandatory = $true)][string]$Message)
    $script:Failures += $Message
    Write-Host "  ERROR: $Message" -ForegroundColor Red
}

# ---- native helpers (exit codes are always checked by the caller) ----

function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [string[]]$Arguments = @()
    )
    & $FilePath @Arguments | Out-Null
    return $LASTEXITCODE
}

function Invoke-NativeChecked {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [string[]]$Arguments = @(),
        [string]$What = "native command"
    )
    $code = Invoke-Native -FilePath $FilePath -Arguments $Arguments
    if ($code -ne 0) {
        throw "$What failed ($FilePath), exit code $code."
    }
}

# ---- exact-path process discovery / stop (mirrors install.ps1 C2) ----

function Get-ProcessTable {
    $rows = Get-CimInstance -ClassName Win32_Process -ErrorAction Stop
    $out = @()
    foreach ($row in $rows) {
        $path = $null
        if ($null -ne $row.ExecutablePath) { $path = [string]$row.ExecutablePath }
        $out += [PSCustomObject]@{
            ProcessId      = [int]$row.ProcessId
            Name           = [string]$row.Name
            ExecutablePath = $path
        }
    }
    return @($out)
}

function ConvertTo-NormalizedPath {
    param([Parameter(Mandatory = $true)][string]$Path)
    return [System.IO.Path]::GetFullPath($Path)
}

function Test-ProcessAlive {
    param([Parameter(Mandatory = $true)][int]$ProcessId)
    try {
        [void](Get-Process -Id $ProcessId -ErrorAction Stop)
        return $true
    } catch {
        # Only "process not found" means not alive; any other query error is an
        # inability to prove liveness and must not be silently treated as dead.
        if ($_.FullyQualifiedErrorId -like 'NoProcessFoundForGivenId*') { return $false }
        throw "Cannot determine whether PID $ProcessId is alive: $($_.Exception.Message)"
    }
}

function Stop-ProcessForced {
    param([Parameter(Mandatory = $true)][int]$ProcessId)
    Stop-Process -Id $ProcessId -Force -ErrorAction Stop
}

function Get-ManagedProcessPaths {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)
    return @(
        (Join-Path $InstallDirectory "panel.exe"),
        (Join-Path $InstallDirectory "sdwan-windows-amd64.exe")
    )
}

function Get-ManagedProcesses {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)

    $normalizedAllowed = @(Get-ManagedProcessPaths -InstallDirectory $InstallDirectory |
        ForEach-Object { ConvertTo-NormalizedPath -Path $_ })
    $managed = @()
    foreach ($proc in Get-ProcessTable) {
        $rawPath = $proc.ExecutablePath
        if ([string]::IsNullOrWhiteSpace($rawPath)) {
            # Fail closed: cannot prove it is not one of our processes.
            foreach ($candidate in $PROCESS_ENUM_CANDIDATE_NAMES) {
                if ($proc.Name -and [string]::Equals($proc.Name, $candidate, [System.StringComparison]::OrdinalIgnoreCase)) {
                    throw "Cannot read ExecutablePath for PID $($proc.ProcessId) ('$($proc.Name)'); refusing to continue."
                }
            }
            continue
        }
        $normalized = ConvertTo-NormalizedPath -Path $rawPath
        foreach ($allowed in $normalizedAllowed) {
            if ([string]::Equals($normalized, $allowed, [System.StringComparison]::OrdinalIgnoreCase)) {
                $managed += [PSCustomObject]@{
                    ProcessId      = [int]$proc.ProcessId
                    Name           = $proc.Name
                    ExecutablePath = $normalized
                }
                break
            }
        }
    }
    return @($managed)
}

function Get-ProcessPathById {
    param([Parameter(Mandatory = $true)][int]$ProcessId)
    foreach ($proc in Get-ProcessTable) {
        if ([int]$proc.ProcessId -eq $ProcessId) {
            if ([string]::IsNullOrWhiteSpace($proc.ExecutablePath)) { return $null }
            try { return ConvertTo-NormalizedPath -Path $proc.ExecutablePath } catch { return $null }
        }
    }
    return $null
}

function Stop-ProcessByPath {
    param(
        [Parameter(Mandatory = $true)][int]$ProcessId,
        [Parameter(Mandatory = $true)][string]$ExpectedPath,
        [int]$GracefulWaitSeconds = 10,
        [switch]$WaitForWindow
    )

    $expected = ConvertTo-NormalizedPath -Path $ExpectedPath
    $livePath = Get-ProcessPathById -ProcessId $ProcessId
    if ($null -eq $livePath) {
        if (-not (Test-ProcessAlive -ProcessId $ProcessId)) { return }
        throw "Cannot resolve ExecutablePath for running PID $ProcessId; refusing to stop it."
    }
    if (-not [string]::Equals($livePath, $expected, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to stop PID ${ProcessId}: ExecutablePath '$livePath' does not match '$expected'."
    }

    if ($WaitForWindow) {
        try {
            $proc = Get-Process -Id $ProcessId -ErrorAction Stop
            [void]$proc.CloseMainWindow()
        } catch {
            Write-Warning "Graceful close failed for PID ${ProcessId}: $($_.Exception.Message); falling back to the checked stop path."
        }
    }

    $deadline = (Get-Date).AddSeconds($GracefulWaitSeconds)
    while ((Get-Date) -lt $deadline) {
        if (-not (Test-ProcessAlive -ProcessId $ProcessId)) { return }
        Start-Sleep -Milliseconds 250
    }

    # Re-verify PID -> path immediately before the forced kill.
    $recheck = Get-ProcessPathById -ProcessId $ProcessId
    if ($null -eq $recheck) {
        if (-not (Test-ProcessAlive -ProcessId $ProcessId)) { return }
        throw "Cannot re-resolve ExecutablePath for PID $ProcessId before force stop."
    }
    if (-not [string]::Equals($recheck, $expected, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to force-stop PID ${ProcessId}: path changed to '$recheck'."
    }
    Stop-ProcessForced -ProcessId $ProcessId
}

function Get-ControlToken {
    param([Parameter(Mandatory = $true)][string]$TokenPath)
    return (Get-Content -LiteralPath $TokenPath -Raw -ErrorAction Stop).Trim()
}

function Invoke-ControlApi {
    param(
        [Parameter(Mandatory = $true)][string]$BaseAddress,
        [Parameter(Mandatory = $true)][string]$Path,
        [string]$Method = "GET",
        [Parameter(Mandatory = $true)][string]$Token,
        [int]$TimeoutSeconds = 3
    )
    return Invoke-RestMethod -Uri "$BaseAddress$Path" -Method $Method `
        -Headers @{ Authorization = "Bearer $Token" } -TimeoutSec $TimeoutSeconds -ErrorAction Stop
}

function Stop-ManagedProcesses {
    param(
        [Parameter(Mandatory = $true)][string]$InstallDirectory,
        [Parameter(Mandatory = $true)][string]$ControlAddress
    )

    $panelPath = ConvertTo-NormalizedPath -Path (Join-Path $InstallDirectory "panel.exe")
    $daemonPath = ConvertTo-NormalizedPath -Path (Join-Path $InstallDirectory "sdwan-windows-amd64.exe")
    $managed = Get-ManagedProcesses -InstallDirectory $InstallDirectory

    # Panel first: it has a window, so try a graceful close.
    $panelProcs = @($managed | Where-Object { [string]::Equals($_.ExecutablePath, $panelPath, [System.StringComparison]::OrdinalIgnoreCase) })
    foreach ($proc in $panelProcs) {
        Stop-ProcessByPath -ProcessId $proc.ProcessId -ExpectedPath $panelPath -GracefulWaitSeconds $STOP_GRACEFUL_WAIT_SECONDS -WaitForWindow
    }

    # Daemon: best-effort control-API shutdown, then bounded wait / force.
    $daemonProcs = @($managed | Where-Object { [string]::Equals($_.ExecutablePath, $daemonPath, [System.StringComparison]::OrdinalIgnoreCase) })
    if ($daemonProcs.Count -gt 0) {
        $tokenPath = Join-Path $InstallDirectory "control.token"
        if (Test-Path -LiteralPath $tokenPath -PathType Leaf) {
            try {
                $token = Get-ControlToken -TokenPath $tokenPath
                [void](Invoke-ControlApi -BaseAddress "http://$ControlAddress" -Path "/v1/shutdown" -Method "POST" -Token $token)
            } catch {
                Write-Warning "Daemon control API shutdown was not accepted: $($_.Exception.Message)"
            }
        }
        foreach ($proc in $daemonProcs) {
            Stop-ProcessByPath -ProcessId $proc.ProcessId -ExpectedPath $daemonPath -GracefulWaitSeconds $STOP_GRACEFUL_WAIT_SECONDS
        }
    }
}

function Assert-ProcessesReleased {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)
    $managed = Get-ManagedProcesses -InstallDirectory $InstallDirectory
    if ($managed.Count -gt 0) {
        $pids = (@($managed | ForEach-Object { $_.ProcessId }) -join ", ")
        throw "Managed processes are still running after stop: PID(s) $pids."
    }
}

function Test-FileExclusiveAccess {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $true }
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        $stream.Dispose()
        return $true
    } catch {
        return $false
    }
}

function Assert-ArtifactsUnlocked {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)
    foreach ($name in $MANAGED_ARTIFACTS) {
        $target = Join-Path $InstallDirectory $name
        if (Test-Path -LiteralPath $target -PathType Leaf) {
            if (-not (Test-FileExclusiveAccess -Path $target)) {
                throw "Target file is still locked: $target"
            }
        }
    }
}

# ---- exact adapter verification (iwan1 only) ----

$ADAPTER_NAME = "iwan1"

function Assert-AdapterNameAllowed {
    param([Parameter(Mandatory = $true)][string]$AdapterName)
    # Only the single managed adapter may ever be targeted; a caller must not be
    # able to point this at another (even Wintun) adapter.
    if (-not [string]::Equals($AdapterName, $ADAPTER_NAME, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to target adapter '$AdapterName': only '$ADAPTER_NAME' is managed."
    }
}

function Get-NetAdapterTable {
    # IncludeHidden so a disabled/hidden iwan1 is still counted.
    $rows = Get-NetAdapter -IncludeHidden -ErrorAction Stop
    $out = @()
    foreach ($row in $rows) {
        $out += [PSCustomObject]@{
            Name                 = [string]$row.Name
            InterfaceGuid        = [string]$row.InterfaceGuid
            PnPDeviceID          = [string]$row.PnPDeviceID
            InterfaceDescription = [string]$row.InterfaceDescription
            DriverDescription    = [string]$row.DriverDescription
        }
    }
    return @($out)
}

function Get-SdwanAdapterCandidates {
    param([Parameter(Mandatory = $true)][string]$AdapterName)
    Assert-AdapterNameAllowed -AdapterName $AdapterName
    # Exact name only; never any other adapter (no utun, no physical NICs).
    return @(Get-NetAdapterTable | Where-Object {
        [string]::Equals([string]$_.Name, $AdapterName, [System.StringComparison]::OrdinalIgnoreCase)
    })
}

function Test-SdwanAdapterOwned {
    param([Parameter(Mandatory = $true)]$Adapter)
    $description = "$($Adapter.InterfaceDescription) $($Adapter.DriverDescription)"
    return ($description -match '(?i)wintun')
}

function Assert-AdapterIdentityComplete {
    param([Parameter(Mandatory = $true)]$Adapter)
    # A missing GUID or PnP instance means we cannot pin deletion to a device.
    if ([string]::IsNullOrWhiteSpace([string]$Adapter.InterfaceGuid)) {
        throw "Adapter '$($Adapter.Name)' has no InterfaceGuid; refusing to delete."
    }
    if ([string]::IsNullOrWhiteSpace([string]$Adapter.PnPDeviceID)) {
        throw "Adapter '$($Adapter.Name)' has no PnPDeviceID; refusing to delete."
    }
}

function Test-AdapterIdentityEqual {
    param(
        [Parameter(Mandatory = $true)]$A,
        [Parameter(Mandatory = $true)]$B
    )
    # Both GUID and PnP instance must be present and equal; empty == empty is not equal.
    if ([string]::IsNullOrWhiteSpace([string]$A.InterfaceGuid) -or [string]::IsNullOrWhiteSpace([string]$B.InterfaceGuid)) { return $false }
    if ([string]::IsNullOrWhiteSpace([string]$A.PnPDeviceID) -or [string]::IsNullOrWhiteSpace([string]$B.PnPDeviceID)) { return $false }
    $guidEqual = [string]::Equals([string]$A.InterfaceGuid, [string]$B.InterfaceGuid, [System.StringComparison]::OrdinalIgnoreCase)
    $pnpEqual = [string]::Equals([string]$A.PnPDeviceID, [string]$B.PnPDeviceID, [System.StringComparison]::OrdinalIgnoreCase)
    return ($guidEqual -and $pnpEqual)
}

function Resolve-SdwanAdapterForRemoval {
    param([Parameter(Mandatory = $true)][string]$AdapterName)
    Assert-AdapterNameAllowed -AdapterName $AdapterName
    $candidates = @(Get-SdwanAdapterCandidates -AdapterName $AdapterName)
    if ($candidates.Count -eq 0) {
        throw "No network adapter named '$AdapterName' was found."
    }
    if ($candidates.Count -ne 1) {
        throw "Expected exactly one adapter named '$AdapterName', found $($candidates.Count); refusing to delete."
    }
    $adapter = $candidates[0]
    if (-not (Test-SdwanAdapterOwned -Adapter $adapter)) {
        throw "Adapter '$AdapterName' is not a Wintun adapter (InterfaceDescription='$($adapter.InterfaceDescription)', DriverDescription='$($adapter.DriverDescription)'); refusing to delete."
    }
    Assert-AdapterIdentityComplete -Adapter $adapter
    return $adapter
}

function Remove-SdwanAdapterDevice {
    param(
        [Parameter(Mandatory = $true)][string]$InstanceId
    )
    # pnputil removes the exact device instance only; never a class, wildcard,
    # or driver-package removal.
    Invoke-NativeChecked -FilePath "pnputil.exe" -Arguments @("/remove-device", $InstanceId) -What "pnputil /remove-device $InstanceId"
}

function Assert-SdwanAdapterGone {
    param(
        [Parameter(Mandatory = $true)][string]$AdapterName,
        [Parameter(Mandatory = $true)][string]$InterfaceGuid,
        [Parameter(Mandatory = $true)][string]$PnpDeviceId
    )
    # A failed enumeration throws (see Get-NetAdapterTable) and must never be
    # treated as "gone".
    $candidates = @(Get-SdwanAdapterCandidates -AdapterName $AdapterName)
    if ($candidates.Count -ne 0) {
        throw "Adapter '$AdapterName' is still present after removal (exact-name count $($candidates.Count))."
    }
    foreach ($adapter in @(Get-NetAdapterTable)) {
        if ([string]::Equals([string]$adapter.Name, $AdapterName, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "Adapter '$AdapterName' is still present by name after removal."
        }
        # Compare each readable identity field independently. A readable field
        # that still matches the captured device means it still exists, even
        # under a new name or with the other field unreadable. If NEITHER field
        # is readable we cannot exclude this candidate from the captured device,
        # so we fail closed rather than declaring success.
        $guidReadable = -not [string]::IsNullOrWhiteSpace([string]$adapter.InterfaceGuid)
        $pnpReadable = -not [string]::IsNullOrWhiteSpace([string]$adapter.PnPDeviceID)
        if (-not $guidReadable -and -not $pnpReadable) {
            throw "Cannot exclude adapter '$($adapter.Name)' from the captured identity (GUID and PnP are unreadable); refusing to report the device as removed."
        }
        $sameGuid = $guidReadable -and [string]::Equals([string]$adapter.InterfaceGuid, $InterfaceGuid, [System.StringComparison]::OrdinalIgnoreCase)
        $samePnp = $pnpReadable -and [string]::Equals([string]$adapter.PnPDeviceID, $PnpDeviceId, [System.StringComparison]::OrdinalIgnoreCase)
        if ($sameGuid -or $samePnp) {
            throw "Adapter identity (guid=$InterfaceGuid pnp=$PnpDeviceId) is still present as '$($adapter.Name)' after removal."
        }
    }
}

function Invoke-SdwanAdapterRemoval {
    param([Parameter(Mandatory = $true)][string]$AdapterName)

    Assert-AdapterNameAllowed -AdapterName $AdapterName

    $adapter = Resolve-SdwanAdapterForRemoval -AdapterName $AdapterName
    Write-Host "  Adapter record: name=$AdapterName guid=$($adapter.InterfaceGuid) pnp=$($adapter.PnPDeviceID)"
    Write-Host "  Adapter description: '$($adapter.InterfaceDescription)' / driver '$($adapter.DriverDescription)'"

    # Re-resolve immediately before deletion; both GUID and PnP must match.
    $again = Resolve-SdwanAdapterForRemoval -AdapterName $AdapterName
    if (-not (Test-AdapterIdentityEqual -A $adapter -B $again)) {
        throw "Adapter identity changed before deletion; refusing to delete."
    }

    Remove-SdwanAdapterDevice -InstanceId $again.PnPDeviceID

    Assert-SdwanAdapterGone -AdapterName $AdapterName -InterfaceGuid $again.InterfaceGuid -PnpDeviceId $again.PnPDeviceID
}

# ---- scheduled task (exact name, not-found distinguished from query errors) ----

function Get-SdwanScheduledTask {
    param([Parameter(Mandatory = $true)][string]$TaskName)
    try {
        return Get-ScheduledTask -TaskName $TaskName -ErrorAction Stop
    } catch {
        # The real not-found error is
        #   FullyQualifiedErrorId = CmdletizationQuery_NotFound_TaskName,Get-ScheduledTask
        # with CategoryInfo.Category = ObjectNotFound (Reason is CimJobException).
        $errorId = [string]$_.FullyQualifiedErrorId
        $category = $null
        if ($null -ne $_.CategoryInfo) { $category = [string]$_.CategoryInfo.Category }
        if ($errorId -like 'CmdletizationQuery_NotFound_TaskName*' -or $category -eq 'ObjectNotFound') {
            return $null
        }
        throw "Failed to query scheduled task '$TaskName' (id='$errorId', category='$category'): $($_.Exception.Message)"
    }
}

function Remove-SdwanScheduledTask {
    param([Parameter(Mandatory = $true)][string]$TaskName)
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop
}

# ---- file removal ----

function Remove-ManagedFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [string]$What = $Path
    )
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    Remove-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (Test-Path -LiteralPath $Path -PathType Leaf) {
        throw "Failed to remove ${What}: $Path"
    }
    return $true
}

function Remove-Directory {
    param([Parameter(Mandatory = $true)][string]$Path)
    Remove-Item -LiteralPath $Path -Force -ErrorAction Stop
}

function Remove-EmptyInstallDirectory {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)
    if (-not (Test-Path -LiteralPath $InstallDirectory -PathType Container)) { return }
    # Never recurse over unknown files: enumerate, remove only when empty.
    $remaining = @(Get-ChildItem -LiteralPath $InstallDirectory -Force -ErrorAction Stop)
    if ($remaining.Count -eq 0) {
        Remove-Directory -Path $InstallDirectory
        # Read-back: do not claim removal without proving it.
        if (Test-Path -LiteralPath $InstallDirectory) {
            throw "Directory '$InstallDirectory' is still present after removal."
        }
        Write-Host "  $InstallDirectory removed." -ForegroundColor Green
    } else {
        $names = (@($remaining | ForEach-Object { $_.Name }) -join ", ")
        Write-Host "  Directory retained (not empty): $names" -ForegroundColor Yellow
    }
}

# ---- orchestration ----

function Complete-Uninstall {
    Write-Host ""
    if ($script:Failures.Count -gt 0) {
        Write-Host "SDWAN uninstall completed with $($script:Failures.Count) error(s):" -ForegroundColor Red
        foreach ($failure in $script:Failures) {
            Write-Host "  - $failure" -ForegroundColor Red
        }
        throw "SDWAN uninstall did not fully succeed."
    }
    Write-Host "Uninstall complete" -ForegroundColor Green
}

function Invoke-SdwanUninstall {
    param(
        [Parameter(Mandatory = $true)][string]$InstallDirectory,
        [Parameter(Mandatory = $true)][string]$ShortcutFilePath,
        [string]$ControlListenAddress = "127.0.0.1:17890",
        [string]$NetworkAdapterName = "iwan1",
        [switch]$DeleteConfig,
        [switch]$DeleteLogs
    )

    $script:Failures = @()
    Write-Host "Removing SDWAN from '$InstallDirectory'..." -ForegroundColor Yellow

    # 1. Discover managed processes (fail closed).
    try {
        $managed = @(Get-ManagedProcesses -InstallDirectory $InstallDirectory)
    } catch {
        Add-Failure "process discovery: $($_.Exception.Message)"
        Write-Host "Refusing to continue: processes cannot be enumerated safely." -ForegroundColor Red
        Complete-Uninstall
        return
    }

    # 2. Stop exact-path processes and prove release before touching anything.
    if ($managed.Count -gt 0) {
        try {
            Stop-ManagedProcesses -InstallDirectory $InstallDirectory -ControlAddress $ControlListenAddress
        } catch {
            Add-Failure "process stop: $($_.Exception.Message)"
        }
    }
    try {
        Assert-ProcessesReleased -InstallDirectory $InstallDirectory
        Assert-ArtifactsUnlocked -InstallDirectory $InstallDirectory
    } catch {
        Add-Failure "process release: $($_.Exception.Message)"
        Write-Host "Refusing to delete files or the adapter: managed processes are not confirmed gone." -ForegroundColor Red
        Complete-Uninstall
        return
    }

    # 3. iwan1 Wintun adapter, only when proven to be the sdwan-owned one.
    try {
        Invoke-SdwanAdapterRemoval -AdapterName $NetworkAdapterName
        Write-Host "  Adapter '$NetworkAdapterName' removed." -ForegroundColor Green
    } catch {
        Add-Failure "adapter removal: $($_.Exception.Message)"
        Write-Host "  Adapter '$NetworkAdapterName' was NOT removed (verification failed); leaving it in place." -ForegroundColor Red
    }

    # 4. Binaries / asset.
    foreach ($name in $BINARY_FILE_NAMES) {
        try {
            if (Remove-ManagedFile -Path (Join-Path $InstallDirectory $name) -What $name) {
                Write-Host "  Removed $name"
            }
        } catch {
            Add-Failure "remove ${name}: $($_.Exception.Message)"
        }
    }

    # 5. Start Menu shortcut (exact path).
    try {
        if (Test-Path -LiteralPath $ShortcutFilePath -PathType Leaf) {
            Remove-Item -LiteralPath $ShortcutFilePath -Force -ErrorAction Stop
            if (Test-Path -LiteralPath $ShortcutFilePath -PathType Leaf) {
                throw "shortcut is still present."
            }
            Write-Host "  Start Menu shortcut removed."
        }
    } catch {
        Add-Failure "shortcut: $($_.Exception.Message)"
    }

    # 6. Scheduled task (exact name): not-found is distinguished from a query
    #    error, and removal is verified by re-query (never by an exit code).
    try {
        $task = Get-SdwanScheduledTask -TaskName $TASK_NAME
        if ($null -ne $task) {
            Remove-SdwanScheduledTask -TaskName $TASK_NAME
            $after = Get-SdwanScheduledTask -TaskName $TASK_NAME
            if ($null -ne $after) {
                throw "scheduled task '$TASK_NAME' is still present after delete."
            }
            Write-Host "  Auto-start task removed."
        }
    } catch {
        Add-Failure "scheduled task: $($_.Exception.Message)"
    }

    # 7. Config/token only when explicitly requested.
    if ($DeleteConfig) {
        foreach ($name in $PROTECTED_FILE_NAMES) {
            try {
                if (Remove-ManagedFile -Path (Join-Path $InstallDirectory $name) -What $name) {
                    Write-Host "  Removed $name"
                }
            } catch {
                Add-Failure "remove ${name}: $($_.Exception.Message)"
            }
        }
    } else {
        $kept = @($PROTECTED_FILE_NAMES | Where-Object { Test-Path -LiteralPath (Join-Path $InstallDirectory $_) -PathType Leaf })
        if ($kept.Count -gt 0) {
            Write-Host "  Keeping config/token: $($kept -join ', ') (pass -RemoveConfig to delete)." -ForegroundColor Yellow
        }
    }

    # 8. Logs only when explicitly requested.
    if ($DeleteLogs) {
        foreach ($name in $LOG_FILE_NAMES) {
            try {
                if (Remove-ManagedFile -Path (Join-Path $InstallDirectory $name) -What $name) {
                    Write-Host "  Removed $name"
                }
            } catch {
                Add-Failure "remove ${name}: $($_.Exception.Message)"
            }
        }
    }

    # 9. Prune the directory only when it is empty.
    try {
        Remove-EmptyInstallDirectory -InstallDirectory $InstallDirectory
    } catch {
        Add-Failure "directory prune: $($_.Exception.Message)"
    }

    Complete-Uninstall
}

if (-not $SkipUninstall) {
    Invoke-SdwanUninstall -InstallDirectory $InstallDir -ShortcutFilePath $ShortcutPath `
        -ControlListenAddress $ControlAddress -NetworkAdapterName $AdapterName `
        -DeleteConfig:$RemoveConfig -DeleteLogs:$RemoveLogs
}
