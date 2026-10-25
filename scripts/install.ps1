#Requires -RunAsAdministrator

param(
    [string]$Version = "latest",
    [string]$TestHost = "hfs.minieye.tech",
    [string]$LocalDir = "",
    [switch]$SkipStaging
)

# Usage:
#   Saved script: .\install.ps1 1.0.29
#   Saved script: .\install.ps1 v1.0.29
#   Local prebuilt artifacts (no download): .\install.ps1 -LocalDir .\dist
#   Remote scriptblock: & ([scriptblock]::Create((irm https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.ps1))) -Version v1.0.29

$ErrorActionPreference = "Continue"
$REPO_OWNER = "wantianle"
$REPO_NAME  = "sdwan-go"
$REPO_BRANCH = "master"
$INSTALL_DIR = "C:\ProgramData\sdwan"
$START_MENU_DIR = Join-Path $env:ProgramData "Microsoft\Windows\Start Menu\Programs"
$SHORTCUT_PATH = Join-Path $START_MENU_DIR "SDWAN Panel.lnk"
$GH_PROXIES = @("https://gh.ddlc.top/", "https://gh-proxy.com/", "https://gh.idayer.com/")  # GitHub mirrors (verified working 2025-06-29)
$DOWNLOAD_CONNECT_TIMEOUT_MS = 15000
$DOWNLOAD_READ_TIMEOUT_MS = 15000
$DOWNLOAD_OVERALL_TIMEOUT_SEC = 90
$DOWNLOAD_BUFFER_SIZE = 65536

# C2 managed-upgrade constants.
$CONTROL_ADDR = "127.0.0.1:17890"                                    # daemon control API
$MANAGED_ARTIFACTS = @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")  # staging whitelist
$REPLACE_ORDER = @("wintun.dll", "sdwan-windows-amd64.exe", "panel.exe")      # replace order
$PROTECTED_FILE_NAMES = @("iwan.conf", "config.json", "control.token")        # never copied/deleted
$TRANSACTION_ROOT_NAME = "sdwan-install-transaction"
$PROCESS_ENUM_CANDIDATE_NAMES = @("panel.exe", "sdwan-windows-amd64.exe")
$STOP_GRACEFUL_WAIT_SECONDS = 10

if ($Version.ToLower() -eq "latest") {
    $Version = "latest"
} else {
    $Version = "v" + $Version.TrimStart("v", "V")
}

Write-Host ""
Write-Host "===========================================" -ForegroundColor Cyan
Write-Host "  SD-WAN Windows Installer" -ForegroundColor Cyan
Write-Host "===========================================" -ForegroundColor Cyan
Write-Host "  Version: $Version" -ForegroundColor Cyan
Write-Host ""

# ────────────────────────────────────────────────────────────
# C1. Download and verify a release in an isolated staging directory.
# This phase deliberately does not create, replace, or otherwise modify the
# live installation. The later C2 commit phase will consume $stagedRelease.
# ────────────────────────────────────────────────────────────
function Download-File {
    param($Urls, $Dest)

    function Format-Bytes {
        param([Int64]$Bytes)
        if ($Bytes -lt 1KB) { return "$Bytes B" }
        if ($Bytes -lt 1MB) { return "{0:N1} KB" -f ($Bytes / 1KB) }
        if ($Bytes -lt 1GB) { return "{0:N1} MB" -f ($Bytes / 1MB) }
        return "{0:N1} GB" -f ($Bytes / 1GB)
    }

    function Download-WithProgress {
        param(
            [string]$Uri,
            [string]$OutFile,
            [string]$Activity,
            [string]$Status,
            [int]$ProgressId
        )

        $request = [System.Net.HttpWebRequest]::Create($Uri)
        $request.Method = "GET"
        $request.AllowAutoRedirect = $true
        $request.AutomaticDecompression = [System.Net.DecompressionMethods]::GZip -bor [System.Net.DecompressionMethods]::Deflate
        $request.Timeout = $DOWNLOAD_CONNECT_TIMEOUT_MS
        $request.ReadWriteTimeout = $DOWNLOAD_READ_TIMEOUT_MS
        $request.UserAgent = "sdwan-go-installer"

        $response = $null
        $responseStream = $null
        $fileStream = $null

        try {
            $response = $request.GetResponse()
            $responseStream = $response.GetResponseStream()
            $fileStream = [System.IO.File]::Open($OutFile, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)

            $buffer = New-Object byte[] $DOWNLOAD_BUFFER_SIZE
            $totalBytes = if ($response.ContentLength -ge 0) { [Int64]$response.ContentLength } else { -1 }
            $downloadedBytes = [Int64]0
            $startTime = Get-Date
            $lastProgressAt = Get-Date "2000-01-01"

            while ($true) {
                if (((Get-Date) - $startTime).TotalSeconds -ge $DOWNLOAD_OVERALL_TIMEOUT_SEC) {
                    throw "overall timeout after ${DOWNLOAD_OVERALL_TIMEOUT_SEC}s"
                }

                $read = $responseStream.Read($buffer, 0, $buffer.Length)
                if ($read -le 0) { break }

                $fileStream.Write($buffer, 0, $read)
                $downloadedBytes += $read

                $now = Get-Date
                if (($now - $lastProgressAt).TotalMilliseconds -ge 200) {
                    $elapsed = ($now - $startTime).TotalSeconds
                    $speed = if ($elapsed -gt 0) { [Int64]($downloadedBytes / $elapsed) } else { 0 }

                    if ($totalBytes -gt 0) {
                        $percent = [Math]::Min(100, [int](($downloadedBytes * 100) / $totalBytes))
                        $progressStatus = "$(Format-Bytes $downloadedBytes) / $(Format-Bytes $totalBytes) at $(Format-Bytes $speed)/s"
                        Write-Progress -Id $ProgressId -Activity $Activity -Status $Status -CurrentOperation $progressStatus -PercentComplete $percent
                    } else {
                        $progressStatus = "$(Format-Bytes $downloadedBytes) at $(Format-Bytes $speed)/s"
                        Write-Progress -Id $ProgressId -Activity $Activity -Status $Status -CurrentOperation $progressStatus -PercentComplete -1
                    }
                    $lastProgressAt = $now
                }
            }

            $elapsedSec = ((Get-Date) - $startTime).TotalSeconds
            Write-Progress -Id $ProgressId -Activity $Activity -Completed
            return @{
                Bytes = $downloadedBytes
                Seconds = $elapsedSec
            }
        } finally {
            if ($fileStream) { $fileStream.Dispose() }
            if ($responseStream) { $responseStream.Dispose() }
            if ($response) { $response.Dispose() }
        }
    }

    $name = Split-Path $Urls[0] -Leaf
    Write-Host "  Download: $name"
    $allTries = @()
    foreach ($proxy in $GH_PROXIES) { $allTries += "${proxy}$($Urls[0])" }
    $allTries += $Urls[0]

    for ($i = 0; $i -lt $allTries.Count; $i++) {
        $try = $allTries[$i]
        $label = if ($i -lt $GH_PROXIES.Count) {
            $GH_PROXIES[$i].Replace('https://','').TrimEnd('/')
        } else {
            'direct'
        }
        Write-Host ("    [{0}/{1}] {2}" -f ($i + 1), $allTries.Count, $label)
        try {
            if (Test-Path $Dest) {
                Remove-Item $Dest -Force -ErrorAction SilentlyContinue
            }
            $result = Download-WithProgress -Uri $try -OutFile $Dest -Activity "Downloading $name" -Status $label -ProgressId 1
            Write-Host ("      OK ({0} in {1:N1}s)" -f (Format-Bytes $result.Bytes), $result.Seconds) -ForegroundColor Green
            return
        } catch {
            Write-Progress -Id 1 -Activity "Downloading $name" -Completed
            if (Test-Path $Dest) {
                Remove-Item $Dest -Force -ErrorAction SilentlyContinue
            }
            $msg = $_.Exception.Message
            if ($msg.Length -gt 100) { $msg = $msg.Substring(0, 100) + "..." }
            Write-Host "      FAILED" -ForegroundColor Yellow
            Write-Host "      -> $msg" -ForegroundColor DarkYellow
        }
    }

    Write-Host "  FAILED" -ForegroundColor Red
    throw "Download failed: $name"
}

function New-IsolatedStagingDirectory {
    $root = Join-Path ([System.IO.Path]::GetTempPath()) "sdwan-install-staging"
    $path = Join-Path $root ([Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $path -Force -ErrorAction Stop | Out-Null
    return $path
}

function Remove-StagingDirectory {
    param([Parameter(Mandatory = $true)][string]$StagingDirectory)

    # Cleanup must be observable: a silent failure here can strand a partial
    # release on disk. Warn (do not throw) so installer teardown is not aborted.
    try {
        Remove-Item -LiteralPath $StagingDirectory -Recurse -Force -ErrorAction Stop
    } catch {
        Write-Warning "Could not remove staging directory '$StagingDirectory': $($_.Exception.Message) Remove it manually to reclaim disk space."
        return
    }
    if (Test-Path -LiteralPath $StagingDirectory) {
        Write-Warning "Staging directory '$StagingDirectory' still exists after cleanup. Remove it manually to reclaim disk space."
    }
}

function Get-ManifestSha256 {
    param(
        [Parameter(Mandatory = $true)][string]$ManifestPath,
        [Parameter(Mandatory = $true)][string]$ArtifactName
    )

    # SHA256SUMS validates downloaded content integrity; it is not a code-signing check.
    # Accept only GNU sha256sum records: 64 hex characters, one separator space,
    # then either a literal space or '*' mode marker, followed by the exact name.
    $canonicalRecord = '^([A-Fa-f0-9]{64}) ([ *])(.+)$'
    # Do NOT name this accumulator $matches/$Matches: PowerShell variable names are
    # case-insensitive, so it would be clobbered by the automatic match variable
    # (-match) on the next iteration, silently breaking multi-artifact manifests
    # and duplicate detection.
    $records = @()
    foreach ($line in Get-Content -LiteralPath $ManifestPath -ErrorAction Stop) {
        if ($line -match $canonicalRecord) {
            # Capture the regex result before using it so the group values survive.
            $record = $Matches
            # Ordinal / case-sensitive name equality: PANEL.EXE must not satisfy a
            # required panel.exe record.
            if ($record[3] -ceq $ArtifactName) {
                $records += [PSCustomObject]@{ Hash = $record[1].ToLowerInvariant(); Name = $record[3] }
            }
        } elseif ($line.EndsWith($ArtifactName, [System.StringComparison]::Ordinal)) {
            throw "SHA256SUMS contains a malformed record for required artifact '$ArtifactName'."
        }
    }
    if ($records.Count -ne 1) {
        throw "SHA256SUMS must contain exactly one valid SHA-256 entry for '$ArtifactName'."
    }
    return $records[0].Hash
}

function Test-StagedArtifactHash {
    param(
        [Parameter(Mandatory = $true)][string]$ManifestPath,
        [Parameter(Mandatory = $true)][string]$ArtifactPath,
        [Parameter(Mandatory = $true)][string]$ArtifactName
    )

    $expected = Get-ManifestSha256 -ManifestPath $ManifestPath -ArtifactName $ArtifactName
    $actual = (Get-FileHash -LiteralPath $ArtifactPath -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        throw "SHA-256 mismatch for '$ArtifactName' (expected $expected, got $actual)."
    }
    Write-Host "  Verified SHA-256: $ArtifactName" -ForegroundColor Green
}

function Redact-ConfigLine {
    param([Parameter(Mandatory = $true)][string]$Line)

    if ($Line -match '^(\s*password\s*=).*') {
        return "$($Matches[1])REDACTED"
    }
    return $Line
}

function Invoke-NativeChecked {
    param([Parameter(Mandatory = $true)][string]$FilePath, [string[]]$Arguments = @())

    & $FilePath @Arguments | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "Native command failed ($FilePath), exit code $LASTEXITCODE."
    }
}

function Get-ExecutableAclPolicy {
    # Executables/directories: administrators and SYSTEM manage them; users execute only.
    # Never grant the current user Modify on the executable directory.
    return @{ "S-1-5-32-544" = "F"; "S-1-5-18" = "F"; "S-1-5-32-545" = "RX" }
}

function Get-CurrentUserIdentity {
    return [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
}

function Get-SecretAclPolicy {
    # Config/token files may be writable by their designated interactive user.
    return @{ "S-1-5-32-544" = "F"; "S-1-5-18" = "F"; (Get-CurrentUserIdentity) = "M" }
}

function ConvertFrom-IcaclsRights {
    param([Parameter(Mandatory = $true)][string]$Rights)

    switch ($Rights) {
        "F"  { return [System.Security.AccessControl.FileSystemRights]::FullControl }
        "M"  { return [System.Security.AccessControl.FileSystemRights]::Modify }
        "RX" { return [System.Security.AccessControl.FileSystemRights]::ReadAndExecute }
        default { throw "Unsupported icacls rights abbreviation '$Rights'." }
    }
}

function Test-AclPolicyApplied {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][hashtable]$ExpectedPolicy
    )

    # This verifies only required allow rules. It does not prove inherited or other
    # rules deny extra privileges; C2 must evaluate that separately. No ACL is applied by C1.
    $acl = Get-Acl -LiteralPath $Path -ErrorAction Stop
    foreach ($identity in $ExpectedPolicy.Keys) {
        $required = ConvertFrom-IcaclsRights -Rights $ExpectedPolicy[$identity]
        $matchingRule = @($acl.Access | Where-Object {
            $_.AccessControlType -eq [System.Security.AccessControl.AccessControlType]::Allow -and
            $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value -eq $identity -and
            ($_.FileSystemRights -band $required) -eq $required
        })
        if ($matchingRule.Count -eq 0) {
            throw "ACL verification failed for '$identity' on '$Path'."
        }
    }
    return $true
}

function Set-AclPolicy {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][hashtable]$Policy
    )

    # C2 may call this on a specifically selected path, then call
    # Test-AclPolicyApplied to verify it. C1 must not apply it to live files.
    foreach ($identity in $Policy.Keys) {
        Invoke-NativeChecked -FilePath "icacls.exe" -Arguments @($Path, "/grant:r", "*$identity`:$($Policy[$identity])")
    }
    [void](Test-AclPolicyApplied -Path $Path -ExpectedPolicy $Policy)
}

function Stage-ReleaseArtifacts {
    param([Parameter(Mandatory = $true)][string]$ReleaseUrl)

    $stagingDir = New-IsolatedStagingDirectory
    $artifacts = @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")
    Write-Host "[1/5] Staging release in: $stagingDir" -ForegroundColor Cyan
    try {
        Download-File -Urls @("$ReleaseUrl/SHA256SUMS") -Dest (Join-Path $stagingDir "SHA256SUMS")
        foreach ($artifact in $artifacts) {
            # Wintun must be the release DLL covered by this manifest; ZIP fallbacks are not accepted.
            Download-File -Urls @("$ReleaseUrl/$artifact") -Dest (Join-Path $stagingDir $artifact)
        }
        foreach ($artifact in $artifacts) {
            Test-StagedArtifactHash -ManifestPath (Join-Path $stagingDir "SHA256SUMS") -ArtifactPath (Join-Path $stagingDir $artifact) -ArtifactName $artifact
        }
    } catch {
        Write-Host "  Staging failed; no installed files were changed." -ForegroundColor Red
        Remove-StagingDirectory -StagingDirectory $stagingDir
        throw
    }

    Write-Host "  Staging integrity verification succeeded. SHA-256 verifies integrity, not publisher signing." -ForegroundColor Green
    return $stagingDir
}

function Stage-LocalArtifacts {
    param([Parameter(Mandatory = $true)][string]$LocalDir)

    # C1 contract for local builds: isolate, verify whatever the local directory
    # provides, then hand the staging directory to the unchanged C2 commit phase.
    # The live installation is never read or written here.
    if (-not (Test-Path -LiteralPath $LocalDir -PathType Container)) {
        throw "Local artifact directory not found: $LocalDir"
    }
    $localRoot = (Resolve-Path -LiteralPath $LocalDir -ErrorAction Stop).Path
    $artifacts = @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")

    $missing = @()
    foreach ($artifact in $artifacts) {
        if (-not (Test-Path -LiteralPath (Join-Path $localRoot $artifact) -PathType Leaf)) {
            $missing += $artifact
        }
    }
    if ($missing.Count -gt 0) {
        throw "Local artifact directory '$localRoot' is missing required file(s): $($missing -join ', ')."
    }

    $stagingDir = New-IsolatedStagingDirectory
    Write-Host "[1/5] Staging local artifacts from: $localRoot" -ForegroundColor Cyan
    try {
        foreach ($artifact in $artifacts) {
            Assert-SafeRegularFile -Path (Join-Path $localRoot $artifact) -Root $localRoot
            Copy-Item -LiteralPath (Join-Path $localRoot $artifact) -Destination (Join-Path $stagingDir $artifact) -Force -ErrorAction Stop
        }

        $stagingManifest = Join-Path $stagingDir "SHA256SUMS"
        $localManifest = Join-Path $localRoot "SHA256SUMS"
        if (Test-Path -LiteralPath $localManifest -PathType Leaf) {
            Assert-SafeRegularFile -Path $localManifest -Root $localRoot
            Copy-Item -LiteralPath $localManifest -Destination $stagingManifest -Force -ErrorAction Stop
            # The release pipeline runs `sha256sum dist/*`, so local records name the
            # artifact as "dist/<name>". Strip only that prefix; Get-ManifestSha256
            # stays the single strict GNU-format verifier.
            $distPrefixed = '^([A-Fa-f0-9]{64}) ([ *])dist/(.+)$'
            $normalizedRecords = @()
            $rewritten = $false
            foreach ($line in Get-Content -LiteralPath $stagingManifest -ErrorAction Stop) {
                if ($line -match $distPrefixed) {
                    $normalizedRecords += "$($Matches[1]) $($Matches[2])$($Matches[3])"
                    $rewritten = $true
                } else {
                    $normalizedRecords += $line
                }
            }
            if ($rewritten) {
                Set-Content -LiteralPath $stagingManifest -Value $normalizedRecords -Encoding ASCII
            }
            foreach ($artifact in $artifacts) {
                Test-StagedArtifactHash -ManifestPath $stagingManifest -ArtifactPath (Join-Path $stagingDir $artifact) -ArtifactName $artifact
            }
        } else {
            # Local builds are trusted; this is not a signing check. The commit phase
            # still requires a manifest, so record the staged bytes' own hashes.
            Write-Host "  [local] SHA256SUMS not found in '$localRoot'; local artifacts are installed without manifest verification." -ForegroundColor Yellow
            $records = @()
            foreach ($artifact in $artifacts) {
                $hash = (Get-FileHash -LiteralPath (Join-Path $stagingDir $artifact) -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
                $records += "$hash  $artifact"
            }
            Set-Content -LiteralPath $stagingManifest -Value $records -Encoding ASCII
        }
    } catch {
        Write-Host "  Staging failed; no installed files were changed." -ForegroundColor Red
        Remove-StagingDirectory -StagingDirectory $stagingDir
        throw
    }

    Write-Host "  Local artifacts staged: $stagingDir" -ForegroundColor Green
    return $stagingDir
}

# =============================================================================
# C2. Verified, rollback-capable managed upgrade.
#
# Replaces the three release artifacts in place only after proving the old
# processes are gone and the target files are unlocked. Config and token files
# are never copied, overwritten, or deleted. The iwan1 Wintun adapter is never
# deleted, disabled, reset, or recreated. Replacement is a non-atomic per-file
# replace, rollback-capable: each artifact is written to a temp file in the
# install directory and swapped in, so a failure never leaves a truncated
# target; per-file backups in a transaction directory make failures recoverable.
# =============================================================================

# ---- process table / exact-path matching (overridable seams) ----

function Get-ProcessTable {
    # Returns objects shaped as { ProcessId, Name, ExecutablePath }. Tests
    # override this instead of touching CIM.
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
    return [bool](Get-Process -Id $ProcessId -ErrorAction SilentlyContinue)
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
            # Fail closed: an unreadable path for one of our executable names means
            # we cannot prove it is not a managed process.
            foreach ($candidate in $PROCESS_ENUM_CANDIDATE_NAMES) {
                if ($proc.Name -and [string]::Equals($proc.Name, $candidate, [System.StringComparison]::OrdinalIgnoreCase)) {
                    throw "Cannot read ExecutablePath for PID $($proc.ProcessId) ('$($proc.Name)'); refusing unsafe upgrade."
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
        throw "Refusing to stop PID $ProcessId: ExecutablePath '$livePath' does not match '$expected'."
    }

    if ($WaitForWindow) {
        try {
            $proc = Get-Process -Id $ProcessId -ErrorAction Stop
            [void]$proc.CloseMainWindow()
        } catch { }
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
        throw "Refusing to force-stop PID $ProcessId: path changed to '$recheck'."
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
        [string]$JsonBody = $null,
        [Parameter(Mandatory = $true)][string]$Token,
        [int]$TimeoutSeconds = 5
    )
    $params = @{
        Uri         = "$BaseAddress$Path"
        Method      = $Method
        Headers     = @{ Authorization = "Bearer $Token" }
        TimeoutSec  = $TimeoutSeconds
        ErrorAction = "Stop"
    }
    if ($JsonBody) {
        $params["Body"] = $JsonBody
        $params["ContentType"] = "application/json"
    }
    return Invoke-RestMethod @params
}

function Stop-ManagedProcesses {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)

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
                [void](Invoke-ControlApi -BaseAddress "http://$CONTROL_ADDR" -Path "/v1/shutdown" -Method "POST" -Token $token -TimeoutSeconds 3)
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

# ---- file / ACL helpers (overridable seams) ----

function Get-FileAclSddl {
    param([Parameter(Mandatory = $true)][string]$Path)
    return (Get-Acl -LiteralPath $Path -ErrorAction Stop).Sddl
}

function Set-FileAclFromSddl {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Sddl
    )
    $acl = Get-Acl -LiteralPath $Path -ErrorAction Stop
    $acl.SetSecurityDescriptorSddlForm($Sddl)
    Set-Acl -LiteralPath $Path -AclObject $acl -ErrorAction Stop
    # Apply-then-verify: re-read and require the exact original SDDL.
    $actual = (Get-Acl -LiteralPath $Path -ErrorAction Stop).Sddl
    if ($actual -ne $Sddl) {
        throw "ACL restore verification failed for '$Path'."
    }
}

function Test-FileExclusiveAccess {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        $stream.Dispose()
        return $true
    } catch {
        return $false
    }
}

function Assert-SafeRegularFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Root
    )
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "Required staging file is missing: $Path" }
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Refusing reparse point (symlink/junction): $Path"
    }
    $fullPath = [System.IO.Path]::GetFullPath($item.FullName)
    $fullRoot = [System.IO.Path]::GetFullPath($Root).TrimEnd([char]'\') + [System.IO.Path]::DirectorySeparatorChar
    if (-not $fullPath.StartsWith($fullRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Path traversal detected outside staging root: $Path"
    }
}

function Assert-StagingReady {
    param([Parameter(Mandatory = $true)][string]$StagingDirectory)

    if (-not (Test-Path -LiteralPath $StagingDirectory -PathType Container)) {
        throw "Staging directory is missing: $StagingDirectory"
    }
    $root = [System.IO.Path]::GetFullPath($StagingDirectory)
    $manifest = Join-Path $root "SHA256SUMS"
    Assert-SafeRegularFile -Path $manifest -Root $root
    # Only the three whitelisted artifacts are ever consumed from staging.
    foreach ($artifact in $MANAGED_ARTIFACTS) {
        $artifactPath = Join-Path $root $artifact
        Assert-SafeRegularFile -Path $artifactPath -Root $root
        Test-StagedArtifactHash -ManifestPath $manifest -ArtifactPath $artifactPath -ArtifactName $artifact
    }
}

function Get-ProtectedFileRecords {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)
    $records = @()
    foreach ($name in $PROTECTED_FILE_NAMES) {
        $path = Join-Path $InstallDirectory $name
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            $records += [PSCustomObject]@{
                Name    = $name
                Existed = $true
                Path    = $path
                Sha256  = (Get-FileHash -LiteralPath $path -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
                Acl     = (Get-FileAclSddl -Path $path)
            }
        } else {
            $records += [PSCustomObject]@{ Name = $name; Existed = $false; Path = $path; Sha256 = $null; Acl = $null }
        }
    }
    return @($records)
}

function Assert-ProtectedFilesUnchanged {
    param([Parameter(Mandatory = $true)]$Records)
    foreach ($record in $Records) {
        if (-not $record.Existed) { continue }
        if (-not (Test-Path -LiteralPath $record.Path -PathType Leaf)) {
            throw "Protected file disappeared during upgrade: $($record.Name)"
        }
        $now = (Get-FileHash -LiteralPath $record.Path -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
        if ($now -ne $record.Sha256) {
            throw "Protected file content changed during upgrade: $($record.Name)"
        }
    }
}

function Test-AclPolicyStrict {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][hashtable]$ExpectedPolicy
    )

    $acl = Get-Acl -LiteralPath $Path -ErrorAction Stop
    $expectedIds = @($ExpectedPolicy.Keys)

    # Every expected identity must carry its required rights.
    foreach ($identity in $expectedIds) {
        $required = ConvertFrom-IcaclsRights -Rights $ExpectedPolicy[$identity]
        $matchingRule = @($acl.Access | Where-Object {
            $_.AccessControlType -eq [System.Security.AccessControl.AccessControlType]::Allow -and
            (-not $_.IsInherited) -and
            $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value -eq $identity -and
            ($_.FileSystemRights -band $required) -eq $required
        })
        if ($matchingRule.Count -eq 0) {
            throw "ACL verification failed for required identity '$identity' on '$Path'."
        }
    }

    # Reject unexpected allow identities, inherited rules, and excess rights.
    foreach ($rule in $acl.Access) {
        if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { continue }
        if ($rule.IsInherited) { throw "Unexpected inherited allow rule on '$Path'." }
        $sid = $rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
        if ($expectedIds -notcontains $sid) {
            throw "Unexpected allow identity '$sid' on '$Path'."
        }
        $required = ConvertFrom-IcaclsRights -Rights $ExpectedPolicy[$sid]
        if ((([int]$rule.FileSystemRights) -band (-bnot ([int]$required))) -ne 0) {
            throw "ACL grants excess rights to '$sid' on '$Path'."
        }
    }
    return $true
}

function Set-AclPolicyStrict {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][hashtable]$Policy
    )
    # Remove inheritance first so the read-back can require an exact explicit ACL.
    Invoke-NativeChecked -FilePath "icacls.exe" -Arguments @($Path, "/inheritance:r")
    foreach ($identity in $Policy.Keys) {
        Invoke-NativeChecked -FilePath "icacls.exe" -Arguments @($Path, "/grant:r", "*$identity`:$($Policy[$identity])")
    }
    [void](Test-AclPolicyStrict -Path $Path -ExpectedPolicy $Policy)
}

# ---- transaction / rollback ----

function New-TransactionDirectory {
    param([Parameter(Mandatory = $true)][string]$TransactionRoot)
    $root = Join-Path $TransactionRoot ([Guid]::NewGuid().ToString("N"))
    $backup = Join-Path $root "backup"
    New-Item -ItemType Directory -Path $backup -Force -ErrorAction Stop | Out-Null
    return [PSCustomObject]@{
        Root             = $root
        Backup           = $backup
        Entries          = @{}
        InstallDirAcl    = $null
        InstallDirExisted = $false
    }
}

function Write-ArtifactFileReplacement {
    param(
        [Parameter(Mandatory = $true)][string]$SourcePath,
        [Parameter(Mandatory = $true)][string]$TargetPath
    )
    # Non-atomic per-file replace: copy the source to a temp file *in the install
    # directory*, then swap it in. A failed copy only affects the temp file, so a
    # truncated target is never left behind. Same-volume swap keeps it atomic at
    # the individual-file level; the overall multi-file upgrade is not atomic.
    $targetDir = Split-Path -Parent $TargetPath
    $tempPath = Join-Path $targetDir ("." + [System.IO.Path]::GetFileName($TargetPath) + "." + [Guid]::NewGuid().ToString("N") + ".tmp")
    try {
        Copy-Item -LiteralPath $SourcePath -Destination $tempPath -Force -ErrorAction Stop
        if (Test-Path -LiteralPath $TargetPath -PathType Leaf) {
            [System.IO.File]::Replace($tempPath, $TargetPath, $null)
        } else {
            [System.IO.File]::Move($tempPath, $TargetPath)
        }
    } finally {
        if (Test-Path -LiteralPath $tempPath -PathType Leaf) {
            Remove-Item -LiteralPath $tempPath -Force -ErrorAction SilentlyContinue
        }
    }
}

function Backup-TargetArtifact {
    param(
        [Parameter(Mandatory = $true)]$Transaction,
        [Parameter(Mandatory = $true)][string]$TargetPath,
        [Parameter(Mandatory = $true)][string]$Name
    )
    if ($Transaction.Entries.ContainsKey($Name)) { return $Transaction.Entries[$Name] }
    $existed = Test-Path -LiteralPath $TargetPath -PathType Leaf
    $entry = [PSCustomObject]@{
        Name = $Name; Existed = $existed; BackupPath = $null; Acl = $null; Sha256 = $null; Replaced = $false
    }
    if ($existed) {
        $backupPath = Join-Path $Transaction.Backup $Name
        Copy-Item -LiteralPath $TargetPath -Destination $backupPath -Force -ErrorAction Stop
        $entry.BackupPath = $backupPath
        $entry.Sha256 = (Get-FileHash -LiteralPath $TargetPath -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
        $backupHash = (Get-FileHash -LiteralPath $backupPath -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
        if ($backupHash -ne $entry.Sha256) {
            throw "Backup verification failed for '$Name'."
        }
        $entry.Acl = Get-FileAclSddl -Path $TargetPath
        # Capture failure is a commit failure up front, not a rollback surprise.
        if ([string]::IsNullOrWhiteSpace($entry.Acl)) {
            throw "Could not capture the original ACL for '$Name'; refusing to replace it."
        }
    }
    $Transaction.Entries[$Name] = $entry
    return $entry
}

function Install-ArtifactFile {
    param(
        [Parameter(Mandatory = $true)]$Transaction,
        [Parameter(Mandatory = $true)][string]$SourcePath,
        [Parameter(Mandatory = $true)][string]$TargetPath,
        [Parameter(Mandatory = $true)][string]$Name
    )
    Backup-TargetArtifact -Transaction $Transaction -TargetPath $TargetPath -Name $Name | Out-Null
    # Mark dirty before touching the target so any later failure restores it.
    $Transaction.Entries[$Name].Replaced = $true
    Write-ArtifactFileReplacement -SourcePath $SourcePath -TargetPath $TargetPath
    # Re-hash the placed artifact; a mismatch aborts and triggers rollback.
    $expected = (Get-FileHash -LiteralPath $SourcePath -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
    $actual = (Get-FileHash -LiteralPath $TargetPath -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        throw "Post-replace hash mismatch for '$Name' (expected $expected, got $actual)."
    }
    Set-AclPolicyStrict -Path $TargetPath -Policy (Get-ExecutableAclPolicy)
}

function Restore-ArtifactFile {
    param(
        [Parameter(Mandatory = $true)]$Transaction,
        [Parameter(Mandatory = $true)][string]$TargetPath,
        [Parameter(Mandatory = $true)][string]$Name
    )
    if (-not $Transaction.Entries.ContainsKey($Name)) { return }
    $entry = $Transaction.Entries[$Name]
    if (-not $entry.Replaced) { return }
    if ($entry.Existed) {
        # Fail closed: a missing captured ACL must never be silently skipped.
        if ([string]::IsNullOrWhiteSpace($entry.Acl)) {
            throw "Original ACL for '$Name' was not captured; refusing to claim a safe restore."
        }
        Write-ArtifactFileReplacement -SourcePath $entry.BackupPath -TargetPath $TargetPath
        $restored = (Get-FileHash -LiteralPath $TargetPath -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
        if ($restored -ne $entry.Sha256) {
            throw "Restored hash mismatch for '$Name'."
        }
        Set-FileAclFromSddl -Path $TargetPath -Sddl $entry.Acl
    } else {
        # Artifact did not exist before: no ACL to restore, just remove the file.
        if (Test-Path -LiteralPath $TargetPath -PathType Leaf) {
            Remove-Item -LiteralPath $TargetPath -Force -ErrorAction Stop
        }
    }
    $entry.Replaced = $false
}

function Remove-TransactionDirectory {
    param([Parameter(Mandatory = $true)]$Transaction)
    try {
        Remove-Item -LiteralPath $Transaction.Root -Recurse -Force -ErrorAction Stop
    } catch {
        Write-Warning "Could not remove transaction directory '$($Transaction.Root)': $($_.Exception.Message)"
    }
}

function Invoke-CommitRollback {
    param(
        [Parameter(Mandatory = $true)]$Transaction,
        [Parameter(Mandatory = $true)][string]$InstallDirectory,
        $ProtectedRecords = @()
    )

    Write-Warning "Upgrade failed; attempting rollback of the three release artifacts."

    # Fail closed: prove the exact-path processes are gone and the targets are
    # unlocked BEFORE restoring anything. If that cannot be proven, keep the
    # transaction directory and stop without touching any file.
    try {
        Stop-ManagedProcesses -InstallDirectory $InstallDirectory
        Assert-ProcessesReleased -InstallDirectory $InstallDirectory
        foreach ($artifact in $MANAGED_ARTIFACTS) {
            $target = Join-Path $InstallDirectory $artifact
            if (Test-Path -LiteralPath $target -PathType Leaf) {
                if (-not (Test-FileExclusiveAccess -Path $target)) {
                    throw "Cannot prove '$target' is unlocked for rollback."
                }
            }
        }
    } catch {
        Write-Host "  ROLLBACK STOP FAILED; no files were restored. Transaction kept at: $($Transaction.Root)" -ForegroundColor Red
        throw "Rollback aborted before any file restore: $($_.Exception.Message). Transaction directory: $($Transaction.Root)"
    }

    $failures = @()

    # 1. Restore the install-directory ACL first (container policy).
    if ($Transaction.InstallDirExisted) {
        if (-not $Transaction.InstallDirAcl) {
            $failures += "install dir ACL: original SDDL was not captured."
        } else {
            try {
                Set-FileAclFromSddl -Path $InstallDirectory -Sddl $Transaction.InstallDirAcl
            } catch {
                $failures += "install dir ACL: $($_.Exception.Message)"
            }
        }
    } else {
        # The directory did not exist before; undo the upgrade shell explicitly.
        try {
            if (Test-Path -LiteralPath $InstallDirectory) {
                Remove-Item -LiteralPath $InstallDirectory -Recurse -Force -ErrorAction Stop
            }
        } catch {
            $failures += "install dir removal: $($_.Exception.Message)"
        }
    }

    # 2. Restore the three artifacts in reverse replace order (content + ACL).
    $reverse = @($REPLACE_ORDER)
    [array]::Reverse($reverse)
    foreach ($name in $reverse) {
        try {
            Restore-ArtifactFile -Transaction $Transaction -TargetPath (Join-Path $InstallDirectory $name) -Name $name
        } catch {
            $failures += "$name: $($_.Exception.Message)"
        }
    }

    # 3. Restore each protected file's original SDDL; content is never modified.
    foreach ($record in $ProtectedRecords) {
        if (-not $record.Existed) { continue }
        try {
            if (-not $record.Acl) { throw "original ACL was not captured." }
            Set-FileAclFromSddl -Path $record.Path -Sddl $record.Acl
            $now = (Get-FileHash -LiteralPath $record.Path -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
            if ($now -ne $record.Sha256) {
                throw "protected content changed while restoring ACL."
            }
        } catch {
            $failures += "protected '$($record.Name)': $($_.Exception.Message)"
        }
    }

    # 4. Re-verify restored artifact hashes for artifacts that existed before.
    if ($failures.Count -eq 0) {
        foreach ($name in $REPLACE_ORDER) {
            if (-not $Transaction.Entries.ContainsKey($name)) { continue }
            $entry = $Transaction.Entries[$name]
            if (-not $entry.Existed) { continue }
            try {
                $now = (Get-FileHash -LiteralPath (Join-Path $InstallDirectory $name) -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
                if ($now -ne $entry.Sha256) { $failures += "$name: restored hash mismatch." }
            } catch {
                $failures += "$name: hash verify failed: $($_.Exception.Message)"
            }
        }
    }

    if ($failures.Count -gt 0) {
        Write-Host "  ROLLBACK INCOMPLETE. Transaction kept at: $($Transaction.Root)" -ForegroundColor Red
        foreach ($failure in $failures) { Write-Host "    - $failure" -ForegroundColor Red }
        throw "Rollback could not be proven safe; manual recovery required. Transaction directory: $($Transaction.Root)"
    }

    Remove-TransactionDirectory -Transaction $Transaction
    # Config/token content is never touched; only restart if restore was verified.
    try {
        [void](Start-ManagedPanel -InstallDirectory $InstallDirectory)
        Write-Warning "Rolled back to the previous release and restarted the panel."
    } catch {
        Write-Warning "Rolled back, but restarting the panel failed: $($_.Exception.Message)"
    }
    throw "Upgrade rolled back after a failure; the previous release is restored."
}

function Start-ManagedPanel {
    param([Parameter(Mandatory = $true)][string]$InstallDirectory)
    return Start-Process -FilePath (Join-Path $InstallDirectory "panel.exe") -WorkingDirectory $InstallDirectory -PassThru
}

function Wait-ForManagedRuntime {
    param(
        [Parameter(Mandatory = $true)][string]$InstallDirectory,
        [Parameter(Mandatory = $true)][string]$ControlAddress,
        [int]$TimeoutSeconds = 60
    )
    $panelPath = ConvertTo-NormalizedPath -Path (Join-Path $InstallDirectory "panel.exe")
    $daemonPath = ConvertTo-NormalizedPath -Path (Join-Path $InstallDirectory "sdwan-windows-amd64.exe")
    $tokenPath = Join-Path $InstallDirectory "control.token"

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    $panelUp = $false
    $daemonUp = $false
    $apiUp = $false
    while ((Get-Date) -lt $deadline) {
        $managed = Get-ManagedProcesses -InstallDirectory $InstallDirectory
        $panelUp = @($managed | Where-Object { [string]::Equals($_.ExecutablePath, $panelPath, [System.StringComparison]::OrdinalIgnoreCase) }).Count -gt 0
        $daemonUp = @($managed | Where-Object { [string]::Equals($_.ExecutablePath, $daemonPath, [System.StringComparison]::OrdinalIgnoreCase) }).Count -gt 0
        if ($panelUp -and $daemonUp -and (-not $apiUp) -and (Test-Path -LiteralPath $tokenPath -PathType Leaf)) {
            try {
                $token = Get-ControlToken -TokenPath $tokenPath
                $status = Invoke-ControlApi -BaseAddress "http://$ControlAddress" -Path "/v1/status" -Method "GET" -Token $token -TimeoutSeconds 3
                if ($status) { $apiUp = $true }
            } catch {
                # Transient: the daemon may still be reconnecting.
            }
        }
        if ($panelUp -and $daemonUp -and $apiUp) { return $true }
        Start-Sleep -Milliseconds 500
    }
    if (-not $panelUp) { throw "Panel did not start from '$panelPath'." }
    if (-not $daemonUp) { throw "Daemon did not start from '$daemonPath' (panel-up/daemon-down is a restart failure)." }
    throw "Control API did not become ready on $ControlAddress."
}

# ---- commit orchestration ----

function Invoke-StagedCommit {
    param(
        [Parameter(Mandatory = $true)][string]$StagingDirectory,
        [string]$InstallDirectory = $INSTALL_DIR,
        [string]$ControlAddress = $CONTROL_ADDR,
        [string]$TransactionRoot = (Join-Path ([System.IO.Path]::GetTempPath()) $TRANSACTION_ROOT_NAME)
    )

    if (-not (Test-Path -LiteralPath $InstallDirectory -PathType Container)) {
        throw "Install directory not found: $InstallDirectory"
    }

    Write-Host "[2/5] Re-verifying staged release..." -ForegroundColor Cyan
    Assert-StagingReady -StagingDirectory $StagingDirectory

    Write-Host "[3/5] Recording protected files (never copied or deleted)..." -ForegroundColor Cyan
    $protectedRecords = Get-ProtectedFileRecords -InstallDirectory $InstallDirectory

    $transaction = New-TransactionDirectory -TransactionRoot $TransactionRoot
    # Snapshot the original install-directory ACL before any mutation.
    $transaction.InstallDirExisted = Test-Path -LiteralPath $InstallDirectory -PathType Container
    if ($transaction.InstallDirExisted) {
        $transaction.InstallDirAcl = Get-FileAclSddl -Path $InstallDirectory
    }
    try {
        Write-Host "[4/5] Stopping managed processes (exact paths only)..." -ForegroundColor Cyan
        Stop-ManagedProcesses -InstallDirectory $InstallDirectory

        # Prove release before replacing anything.
        Assert-ProcessesReleased -InstallDirectory $InstallDirectory
        foreach ($artifact in $MANAGED_ARTIFACTS) {
            $target = Join-Path $InstallDirectory $artifact
            if (-not (Test-FileExclusiveAccess -Path $target)) {
                throw "Target file is missing or locked by another process: $target"
            }
        }

        Write-Host "[5/5] Replacing release artifacts (rollback-capable)..." -ForegroundColor Cyan
        foreach ($artifact in $REPLACE_ORDER) {
            Write-Host "  Replace: $artifact"
            Install-ArtifactFile -Transaction $transaction -SourcePath (Join-Path $StagingDirectory $artifact) -TargetPath (Join-Path $InstallDirectory $artifact) -Name $artifact
        }

        # Executable policy for the directory; secret policy for config/token.
        Set-AclPolicyStrict -Path $InstallDirectory -Policy (Get-ExecutableAclPolicy)
        foreach ($record in $protectedRecords) {
            if ($record.Existed) {
                Set-AclPolicyStrict -Path $record.Path -Policy (Get-SecretAclPolicy)
            }
        }

        Write-Host "  Restarting managed panel..." -ForegroundColor Cyan
        [void](Start-ManagedPanel -InstallDirectory $InstallDirectory)
        [void](Wait-ForManagedRuntime -InstallDirectory $InstallDirectory -ControlAddress $ControlAddress)

        # Config/token bytes must be intact.
        Assert-ProtectedFilesUnchanged -Records $protectedRecords

        Remove-TransactionDirectory -Transaction $transaction
        Write-Host "  Managed upgrade verified. SHA-256 verifies integrity, not publisher signing." -ForegroundColor Green
        return
    } catch {
        Write-Host "  Commit failed: $($_.Exception.Message)" -ForegroundColor Red
    }

    # Invoke-CommitRollback always throws; this is the terminal failure path.
    Invoke-CommitRollback -Transaction $transaction -InstallDirectory $InstallDirectory -ProtectedRecords $protectedRecords
    throw "Upgrade failed and rollback did not report a terminal state."
}

function Invoke-InstallFlow {
    param(
        [string]$ReleaseUrl = "",
        [string]$LocalDir = "",
        [string]$InstallDirectory = $INSTALL_DIR,
        [string]$TransactionRoot = (Join-Path ([System.IO.Path]::GetTempPath()) $TRANSACTION_ROOT_NAME)
    )
    $stagedRelease = $null
    try {
        if ($LocalDir) {
            $stagedRelease = Stage-LocalArtifacts -LocalDir $LocalDir
        } else {
            if (-not $ReleaseUrl) { throw "Either -ReleaseUrl or -LocalDir is required." }
            $stagedRelease = Stage-ReleaseArtifacts -ReleaseUrl $ReleaseUrl
        }
        Invoke-StagedCommit -StagingDirectory $stagedRelease -InstallDirectory $InstallDirectory -TransactionRoot $TransactionRoot
    } finally {
        if ($stagedRelease) {
            Remove-StagingDirectory -StagingDirectory $stagedRelease
        }
    }
}

if ($Version -eq "latest") {
    $releaseUrl = "https://github.com/$REPO_OWNER/$REPO_NAME/releases/latest/download"
} else {
    $releaseUrl = "https://github.com/$REPO_OWNER/$REPO_NAME/releases/download/$Version"
}

if ($SkipStaging) { return }
if ($LocalDir) {
    Invoke-InstallFlow -LocalDir $LocalDir
} else {
    Invoke-InstallFlow -ReleaseUrl $releaseUrl
}
return

