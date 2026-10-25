# Run from Windows PowerShell as Administrator:
#   powershell -ExecutionPolicy Bypass -File .\scripts\tests\install-c3.tests.ps1
# Pester-free. Dot-sources uninstall.ps1 with -SkipUninstall (no real uninstall)
# and drives Invoke-SdwanUninstall against throwaway temp dirs with process /
# adapter / task / native seams that record calls and can inject failures. It
# never touches C:\ProgramData\sdwan, never stops a real process, never removes a
# real network adapter or scheduled task.

$ErrorActionPreference = "Stop"
$scriptPath = Join-Path $PSScriptRoot "..\uninstall.ps1"
. $scriptPath -SkipUninstall
$ErrorActionPreference = "Stop"

function Assert-True {
    param($Condition, [string]$Message)
    if (-not $Condition) { throw "ASSERT FAILED: $Message" }
}

function Assert-Throws {
    param([scriptblock]$Action, [string]$Message)
    try { & $Action } catch { return }
    throw "ASSERT FAILED (no throw): $Message"
}

# ---------------------------------------------------------------------------
# Seam overrides. Originals captured for the parts under test.
# ---------------------------------------------------------------------------

$script:realInvokeNative = (Get-Command Invoke-Native).ScriptBlock
$script:realStopByPath = (Get-Command Stop-ProcessByPath).ScriptBlock
$script:realRemoveDevice = (Get-Command Remove-SdwanAdapterDevice).ScriptBlock
$script:realTestAlive = (Get-Command Test-ProcessAlive).ScriptBlock
$script:realGetTask = (Get-Command Get-SdwanScheduledTask).ScriptBlock

function Get-ProcessTable { return @($script:fakeProcesses) }

# Real Get-Process is only probed by the observability test; every other test
# overrides Test-ProcessAlive directly.
function Get-Process {
    [CmdletBinding()]
    param([int]$Id)
    if ($script:getProcessThrows) { throw "injected process query failure." }
    if ($script:getProcessWindowless) {
        $close = { throw "CloseMainWindow failed." }
        return [PSCustomObject]@{ Id = $Id; CloseMainWindow = $close }
    }
    Microsoft.PowerShell.Management\Get-Process -Id $Id -ErrorAction Stop
}

function Test-ProcessAlive {
    param([int]$ProcessId)
    if ($script:forcePidAlive) { return $true }
    return [bool](@($script:fakeProcesses | Where-Object { [int]$_.ProcessId -eq $ProcessId }).Count -gt 0)
}

function Stop-ProcessForced {
    param([int]$ProcessId)
    $script:forcedStops += [int]$ProcessId
    if (-not $script:stopLeavesProcesses) {
        $script:fakeProcesses = @($script:fakeProcesses | Where-Object { [int]$_.ProcessId -ne $ProcessId })
    }
}

function Stop-ProcessByPath {
    param([int]$ProcessId, [string]$ExpectedPath, [int]$GracefulWaitSeconds = 10, [switch]$WaitForWindow)
    $script:stopCalls += [PSCustomObject]@{ ProcessId = $ProcessId; ExpectedPath = $ExpectedPath }
    if ($script:stopByPathThrows) { throw "injected stop failure for PID $ProcessId." }
    $script:forcedStops += [int]$ProcessId
    if (-not $script:stopLeavesProcesses) {
        $script:fakeProcesses = @($script:fakeProcesses | Where-Object { [int]$_.ProcessId -ne $ProcessId })
    }
}

function Get-NetAdapterTable {
    if ($script:adapterTableQueue.Count -gt 0) {
        $next = $script:adapterTableQueue[0]
        $script:adapterTableQueue = @($script:adapterTableQueue | Select-Object -Skip 1)
        if ($next -is [string] -and $next -eq "THROW") { throw "injected adapter query failure." }
        return @($next)
    }
    return @($script:fakeAdapters)
}

function Remove-SdwanAdapterDevice {
    param([string]$InstanceId)
    $script:adapterDeleteCalls += $InstanceId
    if ($script:adapterDeleteThrows) { throw "injected device removal failure." }
    switch ($script:adapterDeleteBehavior) {
        "remove" {
            $script:fakeAdapters = @($script:fakeAdapters | Where-Object {
                -not [string]::Equals([string]$_.Name, "iwan1", [System.StringComparison]::OrdinalIgnoreCase)
            })
        }
        "noop" { }
        "rename" {
            $script:fakeAdapters = @($script:fakeAdapters | ForEach-Object {
                if ([string]::Equals([string]$_.Name, "iwan1", [System.StringComparison]::OrdinalIgnoreCase)) {
                    [PSCustomObject]@{
                        Name                 = "iwan1-old"
                        InterfaceGuid        = $_.InterfaceGuid
                        PnPDeviceID          = $_.PnPDeviceID
                        InterfaceDescription = $_.InterfaceDescription
                        DriverDescription    = $_.DriverDescription
                    }
                } else { $_ }
            })
        }
        "rename-guid" {
            # Renamed, GUID readable (matches), PnP blanked.
            $script:fakeAdapters = @($script:fakeAdapters | ForEach-Object {
                if ([string]::Equals([string]$_.Name, "iwan1", [System.StringComparison]::OrdinalIgnoreCase)) {
                    [PSCustomObject]@{
                        Name                 = "iwan1-old"
                        InterfaceGuid        = $_.InterfaceGuid
                        PnPDeviceID          = ""
                        InterfaceDescription = $_.InterfaceDescription
                        DriverDescription    = $_.DriverDescription
                    }
                } else { $_ }
            })
        }
        "rename-pnp" {
            # Renamed, GUID blanked, PnP readable (matches).
            $script:fakeAdapters = @($script:fakeAdapters | ForEach-Object {
                if ([string]::Equals([string]$_.Name, "iwan1", [System.StringComparison]::OrdinalIgnoreCase)) {
                    [PSCustomObject]@{
                        Name                 = "iwan1-old"
                        InterfaceGuid        = ""
                        PnPDeviceID          = $_.PnPDeviceID
                        InterfaceDescription = $_.InterfaceDescription
                        DriverDescription    = $_.DriverDescription
                    }
                } else { $_ }
            })
        }
    }
}

# Real Get-ScheduledTask is only probed by the task-classification test.
function Get-ScheduledTask {
    [CmdletBinding()]
    param([string]$TaskName)
    if ($script:taskRealMode -eq "notfound-id") {
        $ex = New-Object System.Exception "No MSFT_ScheduledTask objects found"
        throw (New-Object System.Management.Automation.ErrorRecord($ex, "CmdletizationQuery_NotFound_TaskName,Get-ScheduledTask", [System.Management.Automation.ErrorCategory]::ObjectNotFound, $TaskName))
    }
    if ($script:taskRealMode -eq "notfound-category") {
        $ex = New-Object System.Exception "not found by category"
        throw (New-Object System.Management.Automation.ErrorRecord($ex, "SomeOtherId,Get-ScheduledTask", [System.Management.Automation.ErrorCategory]::ObjectNotFound, $TaskName))
    }
    if ($script:taskRealMode -eq "error") {
        $ex = New-Object System.Exception "access denied"
        throw (New-Object System.Management.Automation.ErrorRecord($ex, "AccessDenied,Get-ScheduledTask", [System.Management.Automation.ErrorCategory]::PermissionDenied, $TaskName))
    }
    return [PSCustomObject]@{ TaskName = $TaskName }
}

function Test-FileExclusiveAccess {
    param([string]$Path)
    foreach ($locked in $script:lockedPaths) {
        if ([string]::Equals($Path, $locked, [System.StringComparison]::OrdinalIgnoreCase)) { return $false }
    }
    return $true
}

function Get-SdwanScheduledTask {
    param([string]$TaskName)
    if ($script:taskQueryThrows) { throw "injected task query failure." }
    if ($script:taskExists) { return [PSCustomObject]@{ TaskName = $TaskName } }
    return $null
}

function Remove-SdwanScheduledTask {
    param([string]$TaskName)
    $script:taskRemoveCalls++
    if ($script:taskRemoveThrows) { throw "injected task remove failure." }
    if (-not $script:taskRemoveLeaves) { $script:taskExists = $false }
}

function Remove-Directory {
    param([string]$Path)
    if ($script:directoryRemoveNoOp) { return }
    Microsoft.PowerShell.Management\Remove-Item -LiteralPath $Path -Force -ErrorAction Stop
}

function Invoke-Native {
    param([string]$FilePath, [string[]]$Arguments = @())
    $key = "$FilePath " + (@($Arguments) -join " ")
    $script:nativeCalls += $key
    if ($script:nativeResults.ContainsKey($key)) { return [int]$script:nativeResults[$key] }
    return 0
}

function Invoke-ControlApi {
    param([string]$BaseAddress, [string]$Path, [string]$Method = "GET", [string]$Token, [int]$TimeoutSeconds = 3)
    $script:apiCalls++
    return [PSCustomObject]@{ status = "ok" }
}

# ---------------------------------------------------------------------------
# Fixtures and state
# ---------------------------------------------------------------------------

function Reset-C3State {
    $script:fakeProcesses = @()
    $script:forcedStops = @()
    $script:stopCalls = @()
    $script:stopLeavesProcesses = $false
    $script:stopByPathThrows = $false
    $script:forcePidAlive = $false
    $script:getProcessThrows = $false
    $script:getProcessWindowless = $false
    $script:fakeAdapters = @()
    $script:adapterTableQueue = @()
    $script:adapterDeleteCalls = @()
    $script:adapterDeleteThrows = $false
    $script:adapterDeleteBehavior = "remove"
    $script:lockedPaths = @()
    $script:taskExists = $false
    $script:taskQueryThrows = $false
    $script:taskRealMode = "present"
    $script:taskRemoveCalls = 0
    $script:taskRemoveThrows = $false
    $script:taskRemoveLeaves = $false
    $script:directoryRemoveNoOp = $false
    $script:nativeCalls = @()
    $script:nativeResults = @{}
    $script:apiCalls = 0
    # Keep test stops fast (script scope: uninstall.ps1 reads this dynamically).
    $script:STOP_GRACEFUL_WAIT_SECONDS = 0
}

function New-Adapter {
    param([string]$Name, [string]$Description, [string]$Driver, [string]$Guid, [string]$Pnp)
    return [PSCustomObject]@{
        Name                 = $Name
        InterfaceGuid        = $Guid
        PnPDeviceID          = $Pnp
        InterfaceDescription = $Description
        DriverDescription    = $Driver
    }
}

function New-WintunAdapter {
    param([string]$Name = "iwan1", [string]$Guid = "11111111-1111-1111-1111-111111111111", [string]$Pnp = "ROOT\NET\0000")
    return New-Adapter -Name $Name -Description "Wintun Userspace Tunnel" -Driver "Wintun Userspace Tunnel" -Guid $Guid -Pnp $Pnp
}

function New-C3Fixture {
    $root = Join-Path ([System.IO.Path]::GetTempPath()) ("sdwan-c3-" + [Guid]::NewGuid().ToString("N"))
    $install = Join-Path $root "install"
    $shortcut = Join-Path $root "SDWAN Panel.lnk"
    New-Item -ItemType Directory -Path $install -Force | Out-Null
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        [System.IO.File]::WriteAllBytes((Join-Path $install $name), [byte[]](1, 2, 3, 4))
    }
    Set-Content -LiteralPath (Join-Path $install "iwan.conf") -Value "server=example"
    Set-Content -LiteralPath (Join-Path $install "config.json") -Value "{}"
    Set-Content -LiteralPath (Join-Path $install "control.token") -Value "tok"
    Set-Content -LiteralPath $shortcut -Value "shortcut"
    return [PSCustomObject]@{ Root = $root; Install = $install; Shortcut = $shortcut }
}

function Add-ManagedProcesses {
    param([string]$InstallDirectory)
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 1001; Name = "panel.exe"; ExecutablePath = (Join-Path $InstallDirectory "panel.exe") }
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 1002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = (Join-Path $InstallDirectory "sdwan-windows-amd64.exe") }
}

function New-TrackedFixture {
    $fx = New-C3Fixture
    $script:fixtures += $fx
    return $fx
}

$script:fixtures = @()
Reset-C3State

try {
    # -----------------------------------------------------------------------
    # 1. Exact-path stop only; decoys at other paths survive.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 2001; Name = "panel.exe"; ExecutablePath = "C:\Other\panel.exe" }
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 2002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = "C:\Other\sdwan-windows-amd64.exe" }
    $script:fakeAdapters = @(New-WintunAdapter)

    $managed = @(Get-ManagedProcesses -InstallDirectory $fx.Install)
    Assert-True ($managed.Count -eq 2) "Expected exactly the two exact-path managed processes, got $($managed.Count)."
    Assert-True (@($managed | Where-Object { $_.ProcessId -eq 2001 -or $_.ProcessId -eq 2002 }).Count -eq 0) "Path-mismatched processes must not be managed."

    Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut
    Assert-True ($script:forcedStops -contains 1001 -and $script:forcedStops -contains 1002) "Exact-path processes must be stopped."
    Assert-True (-not ($script:forcedStops -contains 2001) -and -not ($script:forcedStops -contains 2002)) "Path-mismatched processes must not be killed."
    Assert-True (@($script:fakeProcesses | Where-Object { $_.ProcessId -eq 2001 -or $_.ProcessId -eq 2002 }).Count -eq 2) "Path-mismatched processes must remain running."
    Write-Host "  [1] exact-path stop only: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 2. Real Stop-ProcessByPath refuses a PID whose path no longer matches.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $panel = Join-Path $fx.Install "panel.exe"
    $script:fakeProcesses = @([PSCustomObject]@{ ProcessId = 3001; Name = "panel.exe"; ExecutablePath = "C:\Other\panel.exe" })
    Assert-Throws { & $script:realStopByPath -ProcessId 3001 -ExpectedPath $panel -GracefulWaitSeconds 0 } "Stop-ProcessByPath must refuse a PID/path mismatch."
    Assert-True ($script:forcedStops.Count -eq 0) "A path-mismatched process must never be force-stopped."
    Write-Host "  [2] path mismatch not killed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 3. Process release cannot be proven -> adapter and files untouched.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:stopLeavesProcesses = $true
    $script:fakeAdapters = @(New-WintunAdapter)
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "Unproven process release must abort the uninstall."
    Assert-True ($script:adapterDeleteCalls.Count -eq 0) "The adapter must NOT be deleted when process release is unproven."
    Assert-True (Test-Path -LiteralPath (Join-Path $fx.Install "panel.exe")) "Files must be kept when process release is unproven."
    Write-Host "  [3] no-release keeps adapter and files: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 4. Adapter verification failure -> adapter not deleted.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-Adapter -Name "iwan1" -Description "Intel(R) Ethernet" -Driver "Intel" -Guid "g-intel" -Pnp "PCI\X")
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A non-Wintun 'iwan1' must be refused."
    Assert-True ($script:adapterDeleteCalls.Count -eq 0) "A non-Wintun adapter must NOT be deleted."
    Assert-True (@($script:fakeAdapters | Where-Object { $_.Name -eq "iwan1" }).Count -eq 1) "The refused adapter must remain."
    Write-Host "  [4] adapter verification failure leaves adapter: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 5. Non-unique candidate -> adapter not deleted.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(
        (New-WintunAdapter -Guid "guid-a" -Pnp "ROOT\NET\0000"),
        (New-WintunAdapter -Guid "guid-b" -Pnp "ROOT\NET\0001")
    )
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "Multiple 'iwan1' candidates must be refused."
    Assert-True ($script:adapterDeleteCalls.Count -eq 0) "A non-unique candidate set must NOT be deleted."
    Write-Host "  [5] non-unique candidate refuses deletion: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 6. Config/token kept by default, removed only with -DeleteConfig.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut
    foreach ($name in @("iwan.conf", "config.json", "control.token")) {
        Assert-True (Test-Path -LiteralPath (Join-Path $fx.Install $name)) "'$name' must be kept by default."
    }

    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut -DeleteConfig
    foreach ($name in @("iwan.conf", "config.json", "control.token")) {
        Assert-True (-not (Test-Path -LiteralPath (Join-Path $fx.Install $name))) "'$name' must be removed with -DeleteConfig."
    }
    Write-Host "  [6] config preserved by default / removed with -DeleteConfig: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 7. Only 'iwan1' may be targeted; other adapters are never touched.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut -NetworkAdapterName "another-wintun" } "A caller must not target another adapter."
    Assert-True ($script:adapterDeleteCalls.Count -eq 0) "Only 'iwan1' may ever be deleted."

    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(
        (New-WintunAdapter),
        (New-Adapter -Name "Ethernet" -Description "Realtek" -Driver "Realtek" -Guid "g-eth" -Pnp "PCI\ETH"),
        (New-Adapter -Name "utun3" -Description "Other tunnel" -Driver "Other" -Guid "g-utun" -Pnp "SW\UTUN")
    )
    Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut
    Assert-True ($script:adapterDeleteCalls.Count -eq 1 -and $script:adapterDeleteCalls[0] -eq "ROOT\NET\0000") "The delete must receive the exact captured PnP instance id."
    Assert-True (@($script:fakeAdapters | Where-Object { $_.Name -eq "Ethernet" }).Count -eq 1) "Other adapters must be untouched."
    Assert-True (@($script:fakeAdapters | Where-Object { $_.Name -eq "utun3" }).Count -eq 1) "Other adapters must be untouched."
    Write-Host "  [7] iwan1-only boundary + other adapters untouched: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 8. Missing GUID or PnP instance -> zero delete calls.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-Adapter -Name "iwan1" -Description "Wintun Userspace Tunnel" -Driver "Wintun Userspace Tunnel" -Guid "" -Pnp "ROOT\NET\0000")
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A missing InterfaceGuid must be refused."
    Assert-True ($script:adapterDeleteCalls.Count -eq 0) "No delete without a complete identity."

    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-Adapter -Name "iwan1" -Description "Wintun Userspace Tunnel" -Driver "Wintun Userspace Tunnel" -Guid "g-1" -Pnp "")
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A missing PnPDeviceID must be refused."
    Assert-True ($script:adapterDeleteCalls.Count -eq 0) "No delete without a complete identity."
    Write-Host "  [8] missing GUID/PnP refused: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 9. Identity changing between resolutions -> zero delete calls.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:adapterTableQueue = @(
        @((New-WintunAdapter -Guid "guid-a" -Pnp "ROOT\NET\0000")),
        @((New-WintunAdapter -Guid "guid-b" -Pnp "ROOT\NET\0001"))
    )
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "An identity change between resolutions must be refused."
    Assert-True ($script:adapterDeleteCalls.Count -eq 0) "No delete when the identity changed."
    Write-Host "  [9] identity change refused: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 10. Delete reports success but the device is still present -> failure.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:adapterDeleteBehavior = "noop"
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A still-present adapter after a successful delete must fail."
    Assert-True (@($script:fakeAdapters | Where-Object { $_.Name -eq "iwan1" }).Count -eq 1) "The adapter is still present."

    # A failed post-delete enumeration must NOT be treated as "gone".
    Reset-C3State
    $fx = New-TrackedFixture
    $script:adapterTableQueue = @(
        @((New-WintunAdapter -Guid "guid-a" -Pnp "ROOT\NET\0000")),
        @((New-WintunAdapter -Guid "guid-a" -Pnp "ROOT\NET\0000")),
        "THROW"
    )
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A failed post-delete adapter query must fail, not be treated as gone."
    Write-Host "  [10] delete-success-but-present / failed query fails: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 11. Renamed adapter with the same identity is not treated as deleted.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:adapterDeleteBehavior = "rename"
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A renamed adapter with the same identity must not count as deleted."
    Assert-True (@($script:fakeAdapters | Where-Object { $_.Name -eq "iwan1-old" }).Count -eq 1) "The renamed adapter remains."

    # Renamed with PnP unreadable but GUID still matching -> still present.
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:adapterDeleteBehavior = "rename-guid"
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A renamed adapter whose readable GUID matches must not count as deleted."
    Assert-True (@($script:fakeAdapters | Where-Object { $_.Name -eq "iwan1-old" }).Count -eq 1) "The renamed adapter remains (GUID match)."

    # Renamed with GUID unreadable but PnP still matching -> still present.
    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:adapterDeleteBehavior = "rename-pnp"
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A renamed adapter whose readable PnP matches must not count as deleted."
    Assert-True (@($script:fakeAdapters | Where-Object { $_.Name -eq "iwan1-old" }).Count -eq 1) "The renamed adapter remains (PnP match)."
    Write-Host "  [11] renamed adapter (full / GUID-only / PnP-only) not treated as deleted: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 12. Real delete wrapper: exact instance id + checked exit code.
    # -----------------------------------------------------------------------
    Reset-C3State
    & $script:realRemoveDevice -InstanceId "ROOT\NET\0007"
    Assert-True (@($script:nativeCalls).Count -eq 1) "The delete wrapper must invoke exactly one native command."
    Assert-True ($script:nativeCalls[0] -like "*pnputil* /remove-device ROOT\NET\0007") "The wrapper must pass the exact instance id to pnputil /remove-device (got '$($script:nativeCalls[0])')."

    Reset-C3State
    $script:nativeResults["pnputil.exe /remove-device ROOT\NET\0007"] = 1
    Assert-Throws { & $script:realRemoveDevice -InstanceId "ROOT\NET\0007" } "A nonzero pnputil exit code must throw."
    Write-Host "  [12] real delete wrapper command + exit code: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 13. Scheduled task: query error != absent; failed delete != removed.
    # -----------------------------------------------------------------------
    # Real wrapper classification against crafted ErrorRecords.
    Reset-C3State
    $script:taskRealMode = "notfound-id"
    $notFound = & $script:realGetTask -TaskName "SDWAN Panel"
    Assert-True ($null -eq $notFound) "CmdletizationQuery_NotFound_TaskName must map to null (absent)."
    $script:taskRealMode = "notfound-category"
    $notFound2 = & $script:realGetTask -TaskName "SDWAN Panel"
    Assert-True ($null -eq $notFound2) "ObjectNotFound category must map to null (absent)."
    $script:taskRealMode = "error"
    Assert-Throws { & $script:realGetTask -TaskName "SDWAN Panel" } "Any other task query error must rethrow, not be treated as absent."
    $script:taskRealMode = "present"
    $present = & $script:realGetTask -TaskName "SDWAN Panel"
    Assert-True ($null -ne $present) "A present task must be returned."

    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:taskQueryThrows = $true
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A task query error must be reported, not treated as absent."
    Assert-True ($script:taskRemoveCalls -eq 0) "No delete when the query failed."

    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:taskExists = $true
    $script:taskRemoveLeaves = $true
    Assert-Throws { Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut } "A successful delete call that leaves the task must fail."
    Assert-True ($script:taskRemoveCalls -eq 1) "The delete must have been attempted."

    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:taskExists = $true
    Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut
    Assert-True ($script:taskRemoveCalls -eq 1 -and -not $script:taskExists) "A present task must be removed and verified gone."

    Reset-C3State
    $fx = New-TrackedFixture
    $script:fakeAdapters = @(New-WintunAdapter)
    $script:taskExists = $false
    Invoke-SdwanUninstall -InstallDirectory $fx.Install -ShortcutFilePath $fx.Shortcut
    Assert-True ($script:taskRemoveCalls -eq 0) "An absent task must not be deleted."
    Write-Host "  [13] scheduled task query/delete semantics: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 14. Directory prune read-back verification.
    # -----------------------------------------------------------------------
    Reset-C3State
    $fx = New-TrackedFixture
    $empty = Join-Path $fx.Root "empty"
    New-Item -ItemType Directory -Path $empty -Force | Out-Null
    $script:directoryRemoveNoOp = $true
    Assert-Throws { Remove-EmptyInstallDirectory -InstallDirectory $empty } "Prune must read-back verify the directory is gone."
    Assert-True (Test-Path -LiteralPath $empty) "The directory is still present after a silently failed removal."
    $script:directoryRemoveNoOp = $false
    Remove-EmptyInstallDirectory -InstallDirectory $empty
    Assert-True (-not (Test-Path -LiteralPath $empty)) "A real prune must remove the empty directory."
    Write-Host "  [14] prune read-back: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 15. Observability: liveness query errors rethrow; graceful close warns.
    # -----------------------------------------------------------------------
    Reset-C3State
    $dead = & $script:realTestAlive -ProcessId 2147483647
    Assert-True (-not $dead) "A clearly absent PID must report not-alive."
    $script:getProcessThrows = $true
    Assert-Throws { & $script:realTestAlive -ProcessId 4 } "A liveness query error must not be swallowed as not-alive."

    Reset-C3State
    $fx = New-TrackedFixture
    $panel = Join-Path $fx.Install "panel.exe"
    $script:fakeProcesses = @([PSCustomObject]@{ ProcessId = 6001; Name = "panel.exe"; ExecutablePath = $panel })
    $script:getProcessWindowless = $true
    & $script:realStopByPath -ProcessId 6001 -ExpectedPath $panel -GracefulWaitSeconds 0 -WaitForWindow
    Assert-True ($script:forcedStops -contains 6001) "A failed graceful close must fall through to the checked stop."
    Write-Host "  [15] observability paths: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 16. Static source assertions.
    # -----------------------------------------------------------------------
    $source = Get-Content -LiteralPath $scriptPath -Raw

    Assert-True ($source -notmatch 'Get-Process\s+-Name') "Get-Process -Name is forbidden."
    Assert-True ($source -notmatch 'taskkill') "taskkill is forbidden."
    Assert-True ($source -notmatch 'Stop-Process[^\r\n]*-Name') "Stop-Process -Name is forbidden."
    Assert-True ($source -notmatch 'SilentlyContinue') "Silent error suppression is forbidden."
    Assert-True ($source -notmatch 'Remove-Item[^\r\n]*-Recurse') "Blanket recursive delete is forbidden."
    Assert-True ($source -notmatch 'Get-NetAdapter\s+-Name') "Wildcard adapter-name lookup is forbidden."
    Assert-True ($source -notmatch 'Remove-NetAdapter') "Remove-NetAdapter does not exist in the NetAdapter module."
    Assert-True ($source -match 'pnputil\.exe') "The real device-removal mechanism is missing."
    Assert-True ($source -match '"/remove-device"') "pnputil /remove-device is missing."
    Assert-True ($source -match 'Get-NetAdapter -IncludeHidden') "Hidden adapters must be counted."
    Assert-True ($source -match 'function\s+Assert-AdapterIdentityComplete') "Identity completeness check is missing."
    Assert-True ($source -match 'function\s+Test-AdapterIdentityEqual') "Dual GUID/PnP comparison is missing."
    Assert-True ($source -match 'function\s+Assert-AdapterNameAllowed') "iwan1-only boundary is missing."
    Assert-True ($source -match '\$ADAPTER_NAME = "iwan1"') "The fixed adapter name is missing."
    Assert-True ($source -match 'Remove-SdwanAdapterDevice -InstanceId \$again\.PnPDeviceID') "The delete must receive the captured PnP instance id."
    Assert-True ($source -match 'function\s+Assert-SdwanAdapterGone') "Post-delete identity verification is missing."
    Assert-True ($source -match 'function\s+Get-SdwanScheduledTask') "Task lookup with not-found distinction is missing."
    Assert-True ($source -match 'CmdletizationQuery_NotFound_TaskName') "The real task not-found error id must be detected."
    Assert-True ($source -match 'ObjectNotFound') "Task not-found must be distinguished from query errors."
    Assert-True ($source -match 'GUID and PnP are unreadable') "Post-delete identity exclusion must fail closed when identity is unreadable."
    Assert-True ($source -match 'guidReadable = -not') "Each identity field must be compared independently."
    Assert-True ($source -match '\$LASTEXITCODE') "Native exit codes must be inspected."
    Assert-True ($source -match 'function\s+Invoke-NativeChecked') "Exit-code-checked native wrapper is missing."
    Assert-True ($source -match 'if \(\$DeleteConfig\)') "Config deletion must be gated behind -DeleteConfig."
    Assert-True ($source -match 'function\s+Get-ManagedProcesses') "Exact-path process enumeration is missing."
    Assert-True ($source -match 'Get-ProcessPathById -ProcessId \$ProcessId') "PID->path recheck is missing."
    Assert-True ($source -match 'function\s+Assert-ArtifactsUnlocked') "Exclusive-open release proof is missing."
    Assert-True ($source -match 'function\s+Remove-Directory') "Directory removal seam is missing."
    Assert-True ($source -match 'still present after removal') "Removal read-back verification is missing."
    Assert-True ($source -match 'NoProcessFoundForGivenId') "Liveness must distinguish not-found from query errors."
    Assert-True ($source -match 'Graceful close failed') "Graceful-close failure must be surfaced."
    Write-Host "  [16] static source assertions: PASS" -ForegroundColor Green

    Write-Host "C3 uninstall tests passed." -ForegroundColor Green
} finally {
    foreach ($fx in $script:fixtures) {
        if (Test-Path -LiteralPath $fx.Root) {
            Remove-Item -LiteralPath $fx.Root -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
