# Run from Windows PowerShell as Administrator:
#   powershell -ExecutionPolicy Bypass -File .\scripts\tests\install-c2.tests.ps1
# Pester-free. Dot-sources install.ps1 with -SkipStaging (no downloads) and
# exercises Invoke-StagedCommit / Invoke-InstallFlow / Wait-ForManagedRuntime
# against throwaway temp install/staging dirs with process/file/ACL/API seams
# that record calls and can inject failures. It never touches C:\ProgramData\sdwan
# and never performs a real install.

$ErrorActionPreference = "Stop"
$scriptPath = Join-Path $PSScriptRoot "..\install.ps1"
. $scriptPath -SkipStaging
# install.ps1 sets Continue; restore the test's strict preference.
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

function Get-FileSha {
    param([string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
}

# ---------------------------------------------------------------------------
# Seam overrides. Originals are captured so real behavior can be asserted.
# ---------------------------------------------------------------------------

$script:realInstallArtifact = (Get-Command Install-ArtifactFile).ScriptBlock
$script:realRestoreArtifact = (Get-Command Restore-ArtifactFile).ScriptBlock
$script:realStageArtifacts = (Get-Command Stage-ReleaseArtifacts).ScriptBlock
$script:realStopByPath = (Get-Command Stop-ProcessByPath).ScriptBlock
$script:realStopManaged = (Get-Command Stop-ManagedProcesses).ScriptBlock
$script:realBackupArtifact = (Get-Command Backup-TargetArtifact).ScriptBlock
$script:realWriteReplace = (Get-Command Write-ArtifactFileReplacement).ScriptBlock

function Get-ProcessTable { return @($script:fakeProcesses) }

function Test-ProcessAlive {
    param([int]$ProcessId)
    if ($script:onTestAlive) { & $script:onTestAlive $ProcessId }
    if ($script:forcePidAlive) { return $true }
    return [bool](@($script:fakeProcesses | Where-Object { [int]$_.ProcessId -eq $ProcessId }).Count -gt 0)
}

function Stop-ProcessForced {
    param([int]$ProcessId)
    $script:forcedStops += [int]$ProcessId
    $script:fakeProcesses = @($script:fakeProcesses | Where-Object { [int]$_.ProcessId -ne $ProcessId })
}

# Fake stop: records and removes from the fake table without touching a real
# process. Tests that need the real path guard invoke $script:realStopByPath.
function Stop-ProcessByPath {
    param([int]$ProcessId, [string]$ExpectedPath, [int]$GracefulWaitSeconds = 10, [switch]$WaitForWindow)
    $script:stopCalls += [PSCustomObject]@{ ProcessId = $ProcessId; ExpectedPath = $ExpectedPath }
    if ($script:stopByPathThrows) { throw "injected stop failure for PID $ProcessId." }
    $script:forcedStops += [int]$ProcessId
    if (-not $script:stopLeavesProcesses) {
        $script:fakeProcesses = @($script:fakeProcesses | Where-Object { [int]$_.ProcessId -ne $ProcessId })
    }
}

# Counts invocations and can throw on a chosen call (to test fail-closed rollback).
function Stop-ManagedProcesses {
    param([string]$InstallDirectory)
    $script:stopManagedCalls++
    if ($script:stopManagedThrowsOnCall -gt 0 -and $script:stopManagedCalls -eq $script:stopManagedThrowsOnCall) {
        throw "injected stop-managed failure (call $($script:stopManagedCalls))."
    }
    & $script:realStopManaged -InstallDirectory $InstallDirectory
}

function Get-FileAclSddl {
    param([string]$Path)
    foreach ($blank in $script:blankAclPaths) {
        if ([string]::Equals($Path, $blank, [System.StringComparison]::OrdinalIgnoreCase)) { return "" }
    }
    if (-not $script:aclStore.ContainsKey($Path)) {
        $script:aclStore[$Path] = "D:ORIGINAL::$Path"
    }
    return $script:aclStore[$Path]
}

# Emulates apply-then-read-back (not a pure recorder): the caller can assert both
# the SDDL that was applied and that the read-back matches it.
function Set-FileAclFromSddl {
    param([string]$Path, [string]$Sddl)
    $script:aclRestoreCalls += [PSCustomObject]@{ Path = $Path; Sddl = $Sddl }
    if ($script:aclRestoreThrows) { throw "injected ACL restore failure for '$Path'." }
    $script:aclCurrent[$Path] = $Sddl
    $actual = $script:aclCurrent[$Path]
    if ($actual -ne $Sddl) { throw "ACL restore verification failed for '$Path'." }
}

function Backup-TargetArtifact {
    param($Transaction, [string]$TargetPath, [string]$Name)
    $entry = & $script:realBackupArtifact -Transaction $Transaction -TargetPath $TargetPath -Name $Name
    # Simulate a rollback-time missing ACL (the up-front capture guard in the real
    # backup already passed): blank the recorded ACL after capture.
    if ($script:blankArtifactAcl -contains $Name) {
        $Transaction.Entries[$Name].Acl = $null
    }
    return $entry
}

function Write-ArtifactFileReplacement {
    param([string]$SourcePath, [string]$TargetPath)
    $leaf = Split-Path -Leaf $TargetPath
    if ($script:replaceThrowsFor -and [string]::Equals($leaf, $script:replaceThrowsFor, [System.StringComparison]::OrdinalIgnoreCase)) {
        $message = $script:replaceThrowMessage
        $script:replaceThrowsFor = $null   # one-shot so rollback restore can proceed
        throw $message
    }
    & $script:realWriteReplace -SourcePath $SourcePath -TargetPath $TargetPath
    if ($script:corruptTargetOnReplace -and [string]::Equals($leaf, $script:corruptTargetOnReplace, [System.StringComparison]::OrdinalIgnoreCase)) {
        [System.IO.File]::WriteAllBytes($TargetPath, [byte[]](0, 0, 0, 0))
        $script:corruptTargetOnReplace = $null   # one-shot; do not corrupt the restore
    }
}

function Get-FileHash {
    param([string]$LiteralPath, [string]$Path, [string]$Algorithm = "SHA256")
    $target = $LiteralPath
    if ([string]::IsNullOrWhiteSpace($target)) { $target = $Path }
    if ($script:corruptBackupHashFor -and $target -like "*\backup\$($script:corruptBackupHashFor)") {
        return [PSCustomObject]@{ Algorithm = "SHA256"; Hash = ("0" * 64); Path = $target }
    }
    Microsoft.PowerShell.Utility\Get-FileHash -LiteralPath $target -Algorithm $Algorithm
}

function Set-AclPolicyStrict {
    param([string]$Path, [hashtable]$Policy)
    $script:aclApplyCalls += [PSCustomObject]@{ Path = $Path; Policy = $Policy }
    if ($script:aclThrows) { throw "injected ACL failure on '$Path'." }
}

function Get-SecretAclPolicy { return @{ "S-1-5-32-544" = "F" } }

function Test-FileExclusiveAccess {
    param([string]$Path)
    $script:exclusiveCalls++
    foreach ($locked in $script:lockedPaths) {
        if ([string]::Equals($Path, $locked, [System.StringComparison]::OrdinalIgnoreCase)) { return $false }
    }
    if ($script:exclusiveFailOnCall -gt 0 -and $script:exclusiveCalls -eq $script:exclusiveFailOnCall) { return $false }
    return $true
}

function Start-ManagedPanel {
    param([string]$InstallDirectory)
    if ($script:startThrows) { throw "injected start failure." }
    $script:startedPanelCount++
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 9001; Name = "panel.exe"; ExecutablePath = (Join-Path $InstallDirectory "panel.exe") }
    if (-not $script:startPanelOnly) {
        $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 9002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = (Join-Path $InstallDirectory "sdwan-windows-amd64.exe") }
    }
    return [PSCustomObject]@{ Id = 9001 }
}

function Invoke-ControlApi {
    param([string]$BaseAddress, [string]$Path, [string]$Method = "GET", [string]$JsonBody = $null, [string]$Token, [int]$TimeoutSeconds = 5)
    $script:apiCalls++
    if ($script:expectedToken -and $Token -ne $script:expectedToken) {
        throw "control token mismatch: got '$Token'."
    }
    $script:apiTokenMatches = $true
    if ($script:apiThrows) { throw "injected control API failure." }
    return [PSCustomObject]@{ state = "connected" }
}

function Install-ArtifactFile {
    param($Transaction, [string]$SourcePath, [string]$TargetPath, [string]$Name)
    $script:installCalls++
    if ($script:failInstallAt -gt 0 -and $script:installCalls -eq $script:failInstallAt) {
        throw "injected failure replacing '$Name'."
    }
    & $script:realInstallArtifact -Transaction $Transaction -SourcePath $SourcePath -TargetPath $TargetPath -Name $Name
}

function Restore-ArtifactFile {
    param($Transaction, [string]$TargetPath, [string]$Name)
    $script:restoreCalls++
    if ($script:restoreThrows) { throw "injected restore failure for '$Name'." }
    & $script:realRestoreArtifact -Transaction $Transaction -TargetPath $TargetPath -Name $Name
}

function Stage-ReleaseArtifacts {
    param([string]$ReleaseUrl)
    if ($script:stageReturns) { return $script:stageReturns }
    & $script:realStageArtifacts -ReleaseUrl $ReleaseUrl
}

# ---------------------------------------------------------------------------
# Fixtures and state
# ---------------------------------------------------------------------------

function Reset-C2State {
    $script:fakeProcesses = @()
    $script:forcedStops = @()
    $script:stopCalls = @()
    $script:stopManagedCalls = 0
    $script:stopManagedThrowsOnCall = 0
    $script:stopByPathThrows = $false
    $script:lockedPaths = @()
    $script:exclusiveCalls = 0
    $script:exclusiveFailOnCall = 0
    $script:stopLeavesProcesses = $false
    $script:aclStore = @{}
    $script:aclCurrent = @{}
    $script:blankAclPaths = @()
    $script:blankArtifactAcl = @()
    $script:aclApplyCalls = @()
    $script:aclRestoreCalls = @()
    $script:aclThrows = $false
    $script:aclRestoreThrows = $false
    $script:startThrows = $false
    $script:startPanelOnly = $false
    $script:restoreThrows = $false
    $script:apiThrows = $false
    $script:apiCalls = 0
    $script:apiTokenMatches = $false
    $script:expectedToken = "tok"
    $script:failInstallAt = 0
    $script:installCalls = 0
    $script:restoreCalls = 0
    $script:startedPanelCount = 0
    $script:stageReturns = $null
    $script:forcePidAlive = $false
    $script:onTestAlive = $null
    $script:reuseDone = $false
    $script:corruptTargetOnReplace = $null
    $script:replaceThrowsFor = $null
    $script:replaceThrowMessage = "injected replacement failure"
    $script:corruptBackupHashFor = $null
    # Keep test stops fast (script scope: install.ps1 reads this dynamically).
    $script:STOP_GRACEFUL_WAIT_SECONDS = 0
}

function New-C2Fixture {
    $root = Join-Path ([System.IO.Path]::GetTempPath()) ("sdwan-c2-" + [Guid]::NewGuid().ToString("N"))
    $staging = Join-Path $root "staging"
    $install = Join-Path $root "install"
    $txn = Join-Path $root "txn"
    New-Item -ItemType Directory -Path $staging, $install, $txn -Force | Out-Null

    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        [System.IO.File]::WriteAllBytes((Join-Path $staging $name), [byte[]](1, 2, 3, 4))
        [System.IO.File]::WriteAllBytes((Join-Path $install $name), [byte[]](9, 9, 9, 9))
    }
    $lines = foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        $h = (Get-FileHash -LiteralPath (Join-Path $staging $name) -Algorithm SHA256).Hash.ToLowerInvariant()
        "$h  $name"
    }
    Set-Content -LiteralPath (Join-Path $staging "SHA256SUMS") -Value $lines

    Set-Content -LiteralPath (Join-Path $install "iwan.conf") -Value "server=example"
    Set-Content -LiteralPath (Join-Path $install "config.json") -Value "{}"
    Set-Content -LiteralPath (Join-Path $install "control.token") -Value "tok"

    return [PSCustomObject]@{ Root = $root; Staging = $staging; Install = $install; Txn = $txn }
}

function Add-ManagedProcesses {
    param([string]$InstallDirectory)
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 1001; Name = "panel.exe"; ExecutablePath = (Join-Path $InstallDirectory "panel.exe") }
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 1002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = (Join-Path $InstallDirectory "sdwan-windows-amd64.exe") }
}

function Add-PanelProcessOnly {
    param([string]$InstallDirectory)
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 1001; Name = "panel.exe"; ExecutablePath = (Join-Path $InstallDirectory "panel.exe") }
}

function Get-AclRestoreLeaves {
    return @($script:aclRestoreCalls | ForEach-Object { Split-Path -Leaf $_.Path })
}

function New-TrackedFixture {
    $fx = New-C2Fixture
    $script:fixtures += $fx
    return $fx
}

$script:fixtures = @()
Reset-C2State

try {
    # -----------------------------------------------------------------------
    # 1. Exactly the two normalized full paths are ever stopped.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 2001; Name = "panel.exe"; ExecutablePath = "C:\Other\panel.exe" }
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 2002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = "C:\Other\sdwan-windows-amd64.exe" }

    $managed = @(Get-ManagedProcesses -InstallDirectory $fx.Install)
    Assert-True ($managed.Count -eq 2) "Expected exactly the two exact-path managed processes, got $($managed.Count)."
    Assert-True (@($managed | Where-Object { $_.ProcessId -eq 2001 -or $_.ProcessId -eq 2002 }).Count -eq 0) "Path-mismatched processes must not be classified as managed."

    Stop-ManagedProcesses -InstallDirectory $fx.Install
    Assert-True ($script:forcedStops -contains 1001 -and $script:forcedStops -contains 1002) "Exact-path processes must be stopped."
    Assert-True (-not ($script:forcedStops -contains 2001) -and -not ($script:forcedStops -contains 2002)) "Path-mismatched processes must not be killed."
    Assert-True (@($script:fakeProcesses | Where-Object { $_.ProcessId -eq 2001 -or $_.ProcessId -eq 2002 }).Count -eq 2) "Path-mismatched processes must remain running."
    Write-Host "  [1] exact-path stop only: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 2. Real Stop-ProcessByPath refuses a PID whose path no longer matches.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    $script:fakeProcesses = @([PSCustomObject]@{ ProcessId = 3001; Name = "panel.exe"; ExecutablePath = "C:\Other\panel.exe" })
    Assert-Throws { & $script:realStopByPath -ProcessId 3001 -ExpectedPath (Join-Path $fx.Install "panel.exe") -GracefulWaitSeconds 0 } "Stop-ProcessByPath must refuse a PID/path mismatch."
    Assert-True ($script:forcedStops.Count -eq 0) "A path-mismatched process must never be force-stopped."
    Write-Host "  [2] path mismatch not killed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 3. A locked target aborts before any replacement.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:lockedPaths = @(Join-Path $fx.Install "sdwan-windows-amd64.exe")
    $before = @{}
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $before[$name] = Get-FileSha (Join-Path $fx.Install $name) }
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A locked target must abort the commit."
    Assert-True ($script:installCalls -eq 0) "No artifact may be replaced while a target is locked."
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $before[$name]) "Target '$name' must be unchanged after a lock abort."
    }
    Write-Host "  [3] lock blocks commit: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 4. Successful upgrade (happy path, direct commit).
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $protectedBefore = @{
        "iwan.conf"     = Get-FileSha (Join-Path $fx.Install "iwan.conf")
        "config.json"   = Get-FileSha (Join-Path $fx.Install "config.json")
        "control.token" = Get-FileSha (Join-Path $fx.Install "control.token")
    }
    Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq (Get-FileSha (Join-Path $fx.Staging $name))) "Artifact '$name' must be upgraded to the staged content."
    }
    Assert-True ($script:startedPanelCount -eq 1) "Panel must be restarted after a successful commit."
    Assert-True ($script:apiTokenMatches) "The restart must present the existing control.token (matching value)."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 0) "A successful commit must remove the transaction directory."
    foreach ($name in $protectedBefore.Keys) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $protectedBefore[$name]) "Protected '$name' must be unchanged."
    }
    Write-Host "  [4] successful upgrade: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 5. Failure at the 1st/2nd/3rd artifact rolls back.
    # -----------------------------------------------------------------------
    foreach ($failAt in 1, 2, 3) {
        Reset-C2State
        $fx = New-TrackedFixture
        Add-ManagedProcesses $fx.Install
        $old = @{}
        foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $old[$name] = Get-FileSha (Join-Path $fx.Install $name) }
        $script:failInstallAt = $failAt
        Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "Failure at artifact #$failAt must surface as an error."
        foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
            Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $old[$name]) "Artifact '$name' must be restored after failure at #$failAt."
        }
        Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 0) "A verified rollback must remove the transaction directory (failure #$failAt)."
    }
    Write-Host "  [5] rollback on 1st/2nd/3rd artifact failure: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 6. ACL apply failure rolls back.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $old = @{}
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $old[$name] = Get-FileSha (Join-Path $fx.Install $name) }
    $script:aclThrows = $true
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "ACL failure must fail the commit."
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $old[$name]) "Artifact '$name' must be restored after ACL failure."
    }
    Write-Host "  [6] ACL failure rollback: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 7. Commit exception (restart failure) rolls back.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $old = @{}
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $old[$name] = Get-FileSha (Join-Path $fx.Install $name) }
    $script:startThrows = $true
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A restart failure must fail the commit."
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $old[$name]) "Artifact '$name' must be restored after a commit exception."
    }
    Write-Host "  [7] commit exception rollback: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 8. ACL rollback restores the directory and protected files, in order, and
    #    never changes config/token contents.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $protectedBefore = @{}
    foreach ($name in @("iwan.conf", "config.json", "control.token")) { $protectedBefore[$name] = Get-FileSha (Join-Path $fx.Install $name) }
    $script:startThrows = $true
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "Restart failure must surface."

    $leaves = @(Get-AclRestoreLeaves)
    $expected = @(
        (Split-Path -Leaf $fx.Install),
        "panel.exe",
        "sdwan-windows-amd64.exe",
        "wintun.dll",
        "iwan.conf",
        "config.json",
        "control.token"
    )
    Assert-True ($leaves.Count -eq $expected.Count) "Expected $($expected.Count) ACL restores, got $($leaves.Count): $($leaves -join ', ')."
    for ($i = 0; $i -lt $expected.Count; $i++) {
        Assert-True ($leaves[$i] -eq $expected[$i]) "ACL restore order mismatch at index $i: expected '$($expected[$i])', got '$($leaves[$i])'."
    }
    # Each restore must receive the exact captured original SDDL, and the
    # apply-then-read-back seam must hold.
    foreach ($call in $script:aclRestoreCalls) {
        $original = "D:ORIGINAL::$($call.Path)"
        Assert-True ($call.Sddl -eq $original) "Set-FileAclFromSddl must receive the captured original SDDL for '$($call.Path)'."
        Assert-True ($script:aclCurrent[$call.Path] -eq $original) "Apply-then-read-back must hold for '$($call.Path)'."
    }
    Assert-True ($script:aclCurrent.Count -eq $expected.Count) "Every restored path must have been applied."
    foreach ($name in $protectedBefore.Keys) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $protectedBefore[$name]) "Protected '$name' content must be unchanged by ACL rollback."
    }
    Write-Host "  [8] ACL rollback (dir + protected, ordered, exact SDDL): PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 9. Fail-closed rollback: if stopping/release proof fails, no file restore.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:failInstallAt = 2
    $script:stopManagedThrowsOnCall = 2   # commit stop = call 1; rollback stop = call 2
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A failed rollback stop must surface as an error."
    Assert-True ($script:restoreCalls -eq 0) "No file restore may be attempted when rollback stop fails."
    Assert-True ($script:aclRestoreCalls.Count -eq 0) "No ACL restore may be attempted when rollback stop fails."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 1) "A fail-closed rollback must keep the transaction directory."
    Write-Host "  [9] fail-closed rollback stop: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 10. Unprovable rollback (restore throws) keeps the txn dir; staging is
    #     still cleaned by Invoke-InstallFlow's finally.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:stageReturns = $fx.Staging
    $script:failInstallAt = 2
    $script:restoreThrows = $true
    Assert-Throws { Invoke-InstallFlow -ReleaseUrl "https://example.invalid" -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "An unprovable rollback must surface as an error."
    Assert-True (-not (Test-Path -LiteralPath $fx.Staging)) "Staging must be cleaned by the outer finally even when rollback fails."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 1) "An unprovable rollback must keep the transaction directory."
    Write-Host "  [10] rollback failure keeps txn, cleans staging: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 11. Success-path staging cleanup via Invoke-InstallFlow.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:stageReturns = $fx.Staging
    $stagedHashes = @{}
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $stagedHashes[$name] = Get-FileSha (Join-Path $fx.Staging $name) }
    Invoke-InstallFlow -ReleaseUrl "https://example.invalid" -InstallDirectory $fx.Install -TransactionRoot $fx.Txn
    Assert-True (-not (Test-Path -LiteralPath $fx.Staging)) "Staging must be cleaned after a successful flow."
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $stagedHashes[$name]) "Artifact '$name' must be upgraded after the successful flow."
    }
    Assert-True ($script:startedPanelCount -eq 1) "Panel must be restarted after a successful flow."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 0) "A successful flow must remove the transaction directory."
    Write-Host "  [11] success-path staging cleanup (flow): PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 12. Restart failures: panel-up/daemon-down, /v1/status failure, token missing.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-PanelProcessOnly $fx.Install
    Assert-Throws { Wait-ForManagedRuntime -InstallDirectory $fx.Install -ControlAddress "127.0.0.1:17890" -TimeoutSeconds 1 } "panel-up/daemon-down must be a restart failure."

    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:apiThrows = $true
    Assert-Throws { Wait-ForManagedRuntime -InstallDirectory $fx.Install -ControlAddress "127.0.0.1:17890" -TimeoutSeconds 1 } "A failing /v1/status must be a restart failure."

    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    Remove-Item -LiteralPath (Join-Path $fx.Install "control.token") -Force
    Assert-Throws { Wait-ForManagedRuntime -InstallDirectory $fx.Install -ControlAddress "127.0.0.1:17890" -TimeoutSeconds 1 } "A missing control.token must be a restart failure."
    Write-Host "  [12] restart failure modes: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 13. Daemon path mismatch is not killed; ExecutablePath unreadable fails closed.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 4002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = "C:\Other\sdwan-windows-amd64.exe" }
    $managed = @(Get-ManagedProcesses -InstallDirectory $fx.Install)
    Assert-True (@($managed | Where-Object { $_.ProcessId -eq 4002 }).Count -eq 0) "A daemon at a mismatched path must not be managed."
    Assert-Throws { & $script:realStopByPath -ProcessId 4002 -ExpectedPath (Join-Path $fx.Install "sdwan-windows-amd64.exe") -GracefulWaitSeconds 0 } "Real stop must refuse a mismatched daemon path."
    Assert-True ($script:forcedStops.Count -eq 0) "No force stop for a mismatched daemon."

    Reset-C2State
    $fx = New-TrackedFixture
    $script:fakeProcesses = @([PSCustomObject]@{ ProcessId = 7001; Name = "panel.exe"; ExecutablePath = $null })
    Assert-Throws { Get-ManagedProcesses -InstallDirectory $fx.Install } "Unreadable panel.exe ExecutablePath must fail closed."
    $script:fakeProcesses = @([PSCustomObject]@{ ProcessId = 7002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = $null })
    Assert-Throws { Get-ManagedProcesses -InstallDirectory $fx.Install } "Unreadable daemon ExecutablePath must fail closed."
    Write-Host "  [13] daemon path mismatch + unreadable path fail-closed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 14. PID reuse during the graceful wait, and unreadable path at -Force recheck.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    $panel = Join-Path $fx.Install "panel.exe"
    $script:fakeProcesses = @([PSCustomObject]@{ ProcessId = 6001; Name = "panel.exe"; ExecutablePath = $panel })
    $script:forcePidAlive = $true
    $script:onTestAlive = {
        param($ProcessId)
        if (-not $script:reuseDone) {
            $script:reuseDone = $true
            $script:fakeProcesses = @($script:fakeProcesses | ForEach-Object {
                if ([int]$_.ProcessId -eq [int]$ProcessId) { [PSCustomObject]@{ ProcessId = $_.ProcessId; Name = $_.Name; ExecutablePath = "C:\Other\panel.exe" } } else { $_ }
            })
        }
    }
    Assert-Throws { & $script:realStopByPath -ProcessId 6001 -ExpectedPath $panel -GracefulWaitSeconds 1 } "PID reuse during wait must refuse the force kill."
    Assert-True ($script:forcedStops.Count -eq 0) "No force kill after PID reuse."

    Reset-C2State
    $fx = New-TrackedFixture
    $panel = Join-Path $fx.Install "panel.exe"
    $script:fakeProcesses = @([PSCustomObject]@{ ProcessId = 6002; Name = "panel.exe"; ExecutablePath = $panel })
    $script:forcePidAlive = $true
    $script:onTestAlive = {
        param($ProcessId)
        if (-not $script:reuseDone) {
            $script:reuseDone = $true
            $script:fakeProcesses = @($script:fakeProcesses | ForEach-Object {
                if ([int]$_.ProcessId -eq [int]$ProcessId) { [PSCustomObject]@{ ProcessId = $_.ProcessId; Name = $_.Name; ExecutablePath = $null } } else { $_ }
            })
        }
    }
    Assert-Throws { & $script:realStopByPath -ProcessId 6002 -ExpectedPath $panel -GracefulWaitSeconds 1 } "Unreadable path at the -Force recheck must abort the kill."
    Assert-True ($script:forcedStops.Count -eq 0) "No force kill when the path is unreadable at recheck."
    Write-Host "  [14] PID reuse + recheck path mismatch: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 15. Static source assertions.
    # -----------------------------------------------------------------------
    $source = Get-Content -LiteralPath $scriptPath -Raw

    Assert-True ($source -match '\$MANAGED_ARTIFACTS = @\("panel\.exe", "sdwan-windows-amd64\.exe", "wintun\.dll"\)') "Three-artifact whitelist is missing."
    Assert-True ($source -notmatch 'taskkill') "taskkill must not be used."
    Assert-True ($source -notmatch 'Get-Process\s+-Name') "Get-Process -Name is forbidden."
    Assert-True ($source -notmatch 'Stop-Process[^\r\n]*-Name') "Stop-Process -Name is forbidden."
    Assert-True ($source -notmatch 'Remove-NetAdapter|Disable-NetAdapter|Enable-NetAdapter|Reset-NetAdapter') "The iwan1 adapter must never be removed/disabled/recreated."
    Assert-True ($source -notmatch 'netsh[^\r\n]*delete') "Adapter deletion via netsh is forbidden."
    Assert-True ($source -notmatch 'Copy-Item[^\r\n]*(iwan\.conf|config\.json|control\.token)') "Config/token must never be copied."
    Assert-True ($source -notmatch 'Remove-Item[^\r\n]*(iwan\.conf|config\.json|control\.token)') "Config/token must never be deleted."
    Assert-True ($source -match 'function\s+Assert-StagingReady') "Staging re-verify helper is missing."
    Assert-True ($source -match 'Test-StagedArtifactHash -ManifestPath \$manifest -ArtifactPath \$artifactPath') "Staging must be re-hashed before commit."
    Assert-True ($source -match 'Get-ProcessPathById -ProcessId \$ProcessId') "PID->path re-verification is missing."
    Assert-True ($source -match 'OrdinalIgnoreCase') "Ordinal path comparison is missing."
    Assert-True ($source -match 'function\s+Invoke-CommitRollback') "Rollback helper is missing."
    Assert-True ($source -match 'Rollback could not be proven safe') "Rollback must refuse to claim success."
    Assert-True ($source -match 'function\s+Invoke-InstallFlow') "Install-flow helper is missing."
    Assert-True ($source -match 'Remove-StagingDirectory -StagingDirectory \$stagedRelease') "Outer finally staging cleanup is missing."
    Assert-True ($source -notmatch 'Test-PostInstallStatus') "Legacy live-install block must be removed."
    Assert-True ($source -notmatch 'Auto-probing MTU') "Legacy live-install block must be removed."
    # Blocker 3 markers.
    Assert-True ($source -match 'function\s+Write-ArtifactFileReplacement') "Non-truncating replacement helper is missing."
    Assert-True ($source -match '\[System\.IO\.File\]::Replace') "Single-file Replace is missing."
    Assert-True ($source -match 'NON-ATOMIC per-file replace') "Non-atomic per-file replace must be labelled."
    Assert-True ($source -match 'Post-replace hash mismatch') "Placed artifact must be re-hashed."
    Assert-True ($source -match 'Backup verification failed') "Backup copy must be verified."
    # Blocker 1 markers.
    Assert-True ($source -match 'InstallDirAcl') "Install-directory ACL must be snapshotted."
    Assert-True ($source -match '\$transaction\.InstallDirAcl = Get-FileAclSddl') "Directory ACL snapshot call is missing."
    Assert-True ($source -match 'ACL restore verification failed') "ACL restore must apply-then-verify."
    Assert-True ($source -match 'ProtectedRecords') "Protected records must be threaded into rollback."
    # Blocker 2 markers.
    Assert-True ($source -match 'ROLLBACK STOP FAILED') "Fail-closed rollback stop is missing."
    # Missing-ACL fail-closed markers.
    Assert-True ($source -match 'Could not capture the original ACL') "Backup must fail closed when the original ACL cannot be captured."
    Assert-True ($source -match 'Original ACL for') "Restore must fail closed when an artifact ACL was not captured."
    Write-Host "  [15] static source assertions: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 16. Missing artifact ACL at rollback: fail closed, keep the transaction.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:blankArtifactAcl = @("wintun.dll")
    $script:failInstallAt = 2
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A missing artifact ACL must fail the rollback."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 1) "A missing artifact ACL must keep the transaction directory."
    Assert-True (@($script:aclRestoreCalls | Where-Object { (Split-Path -Leaf $_.Path) -eq "wintun.dll" }).Count -eq 0) "No ACL restore may be recorded for an artifact with a missing ACL."
    Write-Host "  [16] missing artifact ACL fails closed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 17. Missing install-directory ACL: fail closed, keep the transaction.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:blankAclPaths = @($fx.Install)
    $script:startThrows = $true
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A missing install-directory ACL must fail closed."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 1) "A missing install-directory ACL must keep the transaction directory."
    Write-Host "  [17] missing install-dir ACL fails closed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 18. Missing protected-file ACL: fail closed, keep the transaction.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:blankAclPaths = @((Join-Path $fx.Install "iwan.conf"))
    $script:startThrows = $true
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A missing protected-file ACL must fail closed."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 1) "A missing protected-file ACL must keep the transaction directory."
    Write-Host "  [18] missing protected-file ACL fails closed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 19. Directory did not exist: commit rejects; rollback removes the dir.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    $missing = Join-Path $fx.Root "absent-install"
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $missing -TransactionRoot $fx.Txn } "Commit must reject a missing install directory."

    Reset-C2State
    $fx = New-TrackedFixture
    $txn = New-TransactionDirectory -TransactionRoot $fx.Txn
    $txn.InstallDirExisted = $false
    Assert-Throws { Invoke-CommitRollback -Transaction $txn -InstallDirectory $fx.Install -ProtectedRecords @() } "Rollback with no prior install directory must terminate."
    Assert-True (-not (Test-Path -LiteralPath $fx.Install)) "Rollback must remove the directory that did not exist before."
    Write-Host "  [19] directory-did-not-exist handling: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 20. Release-proof failure (processes not released) -> fail closed.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:stopLeavesProcesses = $true
    $script:failInstallAt = 1
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A release-proof failure must fail closed."
    Assert-True ($script:restoreCalls -eq 0) "No file restore when process release cannot be proven."
    Assert-True ($script:aclRestoreCalls.Count -eq 0) "No ACL restore when process release cannot be proven."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 1) "Release-proof failure must keep the transaction directory."
    Write-Host "  [20] release-proof failure fails closed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 21. Exclusive-open failure during rollback -> fail closed.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $script:startThrows = $true
    $script:exclusiveFailOnCall = 4   # 3 commit checks pass; first rollback check fails
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "An exclusive-open failure during rollback must fail closed."
    Assert-True ($script:restoreCalls -eq 0) "No file restore when a rollback target cannot be proven unlocked."
    Assert-True ($script:aclRestoreCalls.Count -eq 0) "No ACL restore when a rollback target cannot be proven unlocked."
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 1) "Exclusive-open failure must keep the transaction directory."
    Write-Host "  [21] exclusive-open failure fails closed: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 22. Post-replace hash mismatch triggers rollback.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $old = @{}
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $old[$name] = Get-FileSha (Join-Path $fx.Install $name) }
    $script:corruptTargetOnReplace = "wintun.dll"
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A post-replace hash mismatch must fail the commit."
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $old[$name]) "Artifact '$name' must be restored after a post-replace hash mismatch."
    }
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 0) "A verified rollback must remove the transaction directory."
    Write-Host "  [22] post-replace hash mismatch rolls back: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 23. Replacement-helper failure (temp-copy / File.Replace) triggers rollback.
    # -----------------------------------------------------------------------
    foreach ($message in @("injected temp-copy failure", "injected File.Replace failure")) {
        Reset-C2State
        $fx = New-TrackedFixture
        Add-ManagedProcesses $fx.Install
        $old = @{}
        foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $old[$name] = Get-FileSha (Join-Path $fx.Install $name) }
        $script:replaceThrowsFor = "wintun.dll"
        $script:replaceThrowMessage = $message
        Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "Replacement failure '$message' must fail the commit."
        foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
            Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $old[$name]) "Artifact '$name' must be restored after '$message'."
        }
        Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 0) "A verified rollback must remove the transaction directory ('$message')."
    }
    Write-Host "  [23] replacement-helper failures roll back: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 24. Backup hash mismatch is caught up front; files stay unchanged.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-ManagedProcesses $fx.Install
    $old = @{}
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) { $old[$name] = Get-FileSha (Join-Path $fx.Install $name) }
    $script:corruptBackupHashFor = "wintun.dll"
    Assert-Throws { Invoke-StagedCommit -StagingDirectory $fx.Staging -InstallDirectory $fx.Install -TransactionRoot $fx.Txn } "A backup hash mismatch must fail the commit."
    foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
        Assert-True ((Get-FileSha (Join-Path $fx.Install $name)) -eq $old[$name]) "Artifact '$name' must be unchanged after a backup hash mismatch."
    }
    Assert-True (@(Get-ChildItem -LiteralPath $fx.Txn -Directory).Count -eq 0) "An up-front backup failure must roll back cleanly."
    Write-Host "  [24] backup hash mismatch fails up front: PASS" -ForegroundColor Green

    # -----------------------------------------------------------------------
    # 25. Daemon path mismatch in the restart success judgment.
    # -----------------------------------------------------------------------
    Reset-C2State
    $fx = New-TrackedFixture
    Add-PanelProcessOnly $fx.Install
    $script:fakeProcesses += [PSCustomObject]@{ ProcessId = 8002; Name = "sdwan-windows-amd64.exe"; ExecutablePath = "C:\Other\sdwan-windows-amd64.exe" }
    Assert-Throws { Wait-ForManagedRuntime -InstallDirectory $fx.Install -ControlAddress "127.0.0.1:17890" -TimeoutSeconds 1 } "A wrong-path daemon must not satisfy daemon-up."
    Write-Host "  [25] daemon path mismatch in restart judgment: PASS" -ForegroundColor Green

    Write-Host "C2 installer tests passed." -ForegroundColor Green
} finally {
    foreach ($fx in $script:fixtures) {
        if (Test-Path -LiteralPath $fx.Root) {
            Remove-Item -LiteralPath $fx.Root -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
