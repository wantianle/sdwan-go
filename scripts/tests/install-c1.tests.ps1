# Run from Windows PowerShell as Administrator:
#   powershell -ExecutionPolicy Bypass -File .\scripts\tests\install-c1.tests.ps1
# This is deliberately Pester-free and uses -SkipStaging, so it does not download
# or modify the live installation. ACL read-back is not exercised because the
# policy is intentionally not applied until C2.

$ErrorActionPreference = "Stop"
$scriptPath = Join-Path $PSScriptRoot "..\install.ps1"
. $scriptPath -SkipStaging

function Assert-Throws {
    param([scriptblock]$Action, [string]$Message)
    try { & $Action } catch { return }
    throw $Message
}

$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("sdwan-c1-test-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
try {
    $artifact = Join-Path $tempDir "panel.exe"
    [System.IO.File]::WriteAllBytes($artifact, [byte[]](1, 2, 3, 4))
    $hash = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
    $manifest = Join-Path $tempDir "SHA256SUMS"
    $otherHash = ("a" * 64)
    $dllHash = ("b" * 64)

    # Positive: real release manifest format. The release runs
    # `sha256sum dist/*`, which emits GNU text mode: HASH + two spaces + NAME.
    Set-Content -LiteralPath $manifest -NoNewline -Value "$hash  panel.exe"
    Test-StagedArtifactHash -ManifestPath $manifest -ArtifactPath $artifact -ArtifactName "panel.exe"

    # Positive: GNU binary mode (space + '*' mode marker) must also match.
    Set-Content -LiteralPath $manifest -NoNewline -Value "$hash *panel.exe"
    Test-StagedArtifactHash -ManifestPath $manifest -ArtifactPath $artifact -ArtifactName "panel.exe"

    # A checksum mismatch (valid name, wrong hash) must fail.
    Set-Content -LiteralPath $manifest -NoNewline -Value ((("0" * 64) -join "") + "  panel.exe")
    Assert-Throws { Test-StagedArtifactHash -ManifestPath $manifest -ArtifactPath $artifact -ArtifactName "panel.exe" } "Expected a checksum mismatch to fail."

    # An invalid (non-hex) manifest entry must fail.
    Set-Content -LiteralPath $manifest -NoNewline -Value "not-a-sha256  panel.exe"
    Assert-Throws { Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe" } "Expected an invalid manifest entry to fail."

    # A single-space separator without a mode marker must fail.
    Set-Content -LiteralPath $manifest -NoNewline -Value "$hash panel.exe"
    Assert-Throws { Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe" } "Expected a single-space separator to fail."

    # A tab separator must fail.
    Set-Content -LiteralPath $manifest -NoNewline -Value "$hash`t*panel.exe"
    Assert-Throws { Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe" } "Expected a tab separator to fail."

    # Case-different filename must NOT match the required lowercase artifact
    # (equality must be ordinal, not PowerShell's case-insensitive -eq).
    Set-Content -LiteralPath $manifest -NoNewline -Value "$hash  PANEL.EXE"
    Assert-Throws { Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe" } "Expected a case-different filename to not match required 'panel.exe'."

    # Realistic three-artifact SHA256SUMS: every required record must parse to
    # its own hash (regression guard for the $Matches accumulator collision).
    Set-Content -LiteralPath $manifest -Value @(
        "$hash  panel.exe",
        "$otherHash  sdwan-windows-amd64.exe",
        "$dllHash  wintun.dll"
    )
    if ((Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe") -ne $hash) { throw "Three-artifact manifest parsed the wrong hash for panel.exe." }
    if ((Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "sdwan-windows-amd64.exe") -ne $otherHash) { throw "Three-artifact manifest parsed the wrong hash for sdwan-windows-amd64.exe." }
    if ((Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "wintun.dll") -ne $dllHash) { throw "Three-artifact manifest parsed the wrong hash for wintun.dll." }

    # Duplicate required record among otherwise valid records must fail (the
    # accumulator collision previously hid this on multi-artifact manifests).
    Set-Content -LiteralPath $manifest -Value @(
        "$hash  panel.exe",
        "$otherHash  sdwan-windows-amd64.exe",
        "$hash *panel.exe"
    )
    Assert-Throws { Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe" } "Expected duplicate required entries among valid records to fail."

    # A malformed line naming a required artifact must fail, even among valid records.
    Set-Content -LiteralPath $manifest -Value @(
        "$otherHash  sdwan-windows-amd64.exe",
        "invalid  panel.exe"
    )
    Assert-Throws { Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe" } "Expected a malformed record naming a required artifact to fail."

    # Missing required entry must fail.
    Set-Content -LiteralPath $manifest -NoNewline -Value "$hash  sdwan-windows-amd64.exe"
    Assert-Throws { Get-ManifestSha256 -ManifestPath $manifest -ArtifactName "panel.exe" } "Expected a missing required entry to fail."

    # Missing required artifact file must fail hash verification.
    Assert-Throws { Test-StagedArtifactHash -ManifestPath $manifest -ArtifactPath (Join-Path $tempDir "missing-panel.exe") -ArtifactName "panel.exe" } "Expected a missing required artifact to fail."

    if ((Redact-ConfigLine -Line "password=secret") -ne "password=REDACTED") { throw "Password was not redacted." }
    if ((Redact-ConfigLine -Line "username=alice") -ne "username=alice") { throw "Non-password line changed." }

    $executablePolicy = Get-ExecutableAclPolicy
    $secretPolicy = Get-SecretAclPolicy
    if ($executablePolicy["S-1-5-32-545"] -ne "RX" -or $executablePolicy.ContainsKey((Get-CurrentUserIdentity))) { throw "Executable ACL policy is too permissive." }
    if ($secretPolicy[(Get-CurrentUserIdentity)] -ne "M") { throw "Secret ACL policy is missing current-user modify access." }
    if ((ConvertFrom-IcaclsRights -Rights "F") -ne [System.Security.AccessControl.FileSystemRights]::FullControl) { throw "F rights mapping is incorrect." }
    if ((ConvertFrom-IcaclsRights -Rights "M") -ne [System.Security.AccessControl.FileSystemRights]::Modify) { throw "M rights mapping is incorrect." }
    if ((ConvertFrom-IcaclsRights -Rights "RX") -ne [System.Security.AccessControl.FileSystemRights]::ReadAndExecute) { throw "RX rights mapping is incorrect." }
    Assert-Throws { ConvertFrom-IcaclsRights -Rights "W" } "Expected unsupported icacls rights to fail."

    Invoke-NativeChecked -FilePath "$env:ComSpec" -Arguments @("/c", "exit 0")
    Assert-Throws { Invoke-NativeChecked -FilePath "$env:ComSpec" -Arguments @("/c", "exit 7") } "Expected non-zero native exit code to fail."

    # ---- Local prebuilt artifacts (-LocalDir) ----
    # Local mode must reach the same staging contract as the release path: an
    # isolated directory holding exactly the three whitelisted artifacts plus a
    # strict GNU SHA256SUMS that C2's Assert-StagingReady can re-verify.
    $localArtifacts = @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")
    $distDir = Join-Path $tempDir "dist"
    New-Item -ItemType Directory -Path $distDir -Force | Out-Null
    foreach ($name in $localArtifacts) {
        [System.IO.File]::WriteAllBytes((Join-Path $distDir $name), [byte[]](9, 9, 9))
    }

    $stagingRoot = Join-Path ([System.IO.Path]::GetTempPath()) "sdwan-install-staging"
    $localManifest = Join-Path $distDir "SHA256SUMS"

    function New-LocalTestManifest {
        param([string]$Directory, [string]$Prefix = "")
        $records = @()
        foreach ($name in @("panel.exe", "sdwan-windows-amd64.exe", "wintun.dll")) {
            $h = (Get-FileHash -LiteralPath (Join-Path $Directory $name) -Algorithm SHA256).Hash.ToLowerInvariant()
            $records += "$h  $Prefix$name"
        }
        Set-Content -LiteralPath (Join-Path $Directory "SHA256SUMS") -Value $records -Encoding ASCII
    }

    function Get-StagingDirectoryCount {
        if (-not (Test-Path -LiteralPath $stagingRoot)) { return 0 }
        $dirs = @(Get-ChildItem -LiteralPath $stagingRoot -Directory)
        return $dirs.Count
    }

    # 1. Copies exactly the three artifacts, verified against the local manifest.
    New-LocalTestManifest -Directory $distDir
    $stagedLocal = Stage-LocalArtifacts -LocalDir $distDir
    try {
        foreach ($name in $localArtifacts) {
            $stagedPath = Join-Path $stagedLocal $name
            if (-not (Test-Path -LiteralPath $stagedPath -PathType Leaf)) { throw "Local staging is missing $name." }
            $stagedHash = (Get-FileHash -LiteralPath $stagedPath -Algorithm SHA256).Hash.ToLowerInvariant()
            $sourceHash = (Get-FileHash -LiteralPath (Join-Path $distDir $name) -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($stagedHash -ne $sourceHash) { throw "Local staging altered $name." }
            [void](Get-ManifestSha256 -ManifestPath (Join-Path $stagedLocal "SHA256SUMS") -ArtifactName $name)
        }
        Assert-StagingReady -StagingDirectory $stagedLocal
        $stagedNames = @(Get-ChildItem -LiteralPath $stagedLocal | ForEach-Object { $_.Name } | Sort-Object)
        $expectedNames = @(($localArtifacts + "SHA256SUMS") | Sort-Object)
        if (($stagedNames -join "|") -ne ($expectedNames -join "|")) {
            throw "Local staging contains unexpected files: $($stagedNames -join ', ')."
        }
    } finally {
        Remove-StagingDirectory -StagingDirectory $stagedLocal
    }

    # 2. `sha256sum dist/*` records ("dist/<name>") must be accepted.
    New-LocalTestManifest -Directory $distDir -Prefix "dist/"
    $stagedPrefixed = Stage-LocalArtifacts -LocalDir $distDir
    try {
        Assert-StagingReady -StagingDirectory $stagedPrefixed
    } finally {
        Remove-StagingDirectory -StagingDirectory $stagedPrefixed
    }

    # 3. A local manifest that disagrees with the artifacts must be rejected, and
    #    the failed attempt must not strand its staging directory.
    New-LocalTestManifest -Directory $distDir
    $corrupted = @()
    foreach ($line in @(Get-Content -LiteralPath $localManifest)) {
        if ($line.EndsWith("panel.exe", [System.StringComparison]::Ordinal)) {
            $corrupted += (("0" * 64) + "  panel.exe")
        } else {
            $corrupted += $line
        }
    }
    Set-Content -LiteralPath $localManifest -Value $corrupted -Encoding ASCII
    $beforeCount = Get-StagingDirectoryCount
    Assert-Throws { Stage-LocalArtifacts -LocalDir $distDir } "Expected a local manifest mismatch to fail."
    if ((Get-StagingDirectoryCount) -ne $beforeCount) { throw "A failed local staging left its staging directory behind." }

    # 4. No manifest: artifacts are still staged (one skip notice) and the staged
    #    manifest still satisfies C2's strict re-verification.
    Remove-Item -LiteralPath $localManifest -Force
    $beforeCount = Get-StagingDirectoryCount
    $stagedNoManifest = Stage-LocalArtifacts -LocalDir $distDir
    try {
        Assert-StagingReady -StagingDirectory $stagedNoManifest
        foreach ($name in $localArtifacts) {
            if (-not (Test-Path -LiteralPath (Join-Path $stagedNoManifest $name) -PathType Leaf)) {
                throw "Manifest-less local staging is missing $name."
            }
        }
    } finally {
        Remove-StagingDirectory -StagingDirectory $stagedNoManifest
        if ((Get-StagingDirectoryCount) -gt $beforeCount) { throw "Successful local staging was not cleaned up." }
    }

    # 5. A missing artifact is rejected and the error names the missing file.
    Remove-Item -LiteralPath (Join-Path $distDir "wintun.dll") -Force
    $missingMessage = ""
    try { Stage-LocalArtifacts -LocalDir $distDir } catch { $missingMessage = $_.Exception.Message }
    if ($missingMessage -notmatch 'wintun\.dll') {
        throw "A missing local artifact must be reported by name (got: '$missingMessage')."
    }

    # 6. A nonexistent local directory is rejected.
    Assert-Throws { Stage-LocalArtifacts -LocalDir (Join-Path $tempDir "no-such-dist") } "Expected a missing local artifact directory to fail."

    $source = Get-Content -LiteralPath $scriptPath -Raw
    if ($source -match 'wintun\.net|Expand-Archive|wintun\.zip') { throw "Unverified Wintun ZIP fallback remains." }
    if ($source -notmatch 'Invoke-NativeChecked.*icacls\.exe') { throw "ACL application does not check native command results." }
    if ($source -notmatch 'Test-AclPolicyApplied') { throw "ACL application has no read-back verification helper." }
    if ($source -notmatch 'S-1-5-32-544' -or $source -notmatch 'S-1-5-18') { throw "ACL policies do not use well-known SIDs." }

    # Staging cleanup must be routed through the observable helper on both the
    # success/commit-exception path and the staging-failure path.
    if ($source -notmatch 'function\s+Remove-StagingDirectory') { throw "Staging directory cleanup helper is missing." }
    if ($source -notmatch 'Remove-StagingDirectory -StagingDirectory \$stagedRelease') { throw "Successful staging cleanup does not use the observable helper." }
    if ($source -notmatch 'Remove-StagingDirectory -StagingDirectory \$stagingDir') { throw "Staging-failure cleanup does not use the observable helper." }
    if ($source -match 'Remove-Item -LiteralPath \$stagedRelease[^\r\n]*-ErrorAction SilentlyContinue') { throw "Silent staging directory cleanup remains." }
    if ($source -notmatch 'Write-Warning') { throw "Staging cleanup failures are not surfaced." }

    # Manifest parser must not use the automatic $Matches variable as an accumulator.
    if ($source -notmatch '\$records') { throw "Manifest parser does not use a non-colliding accumulator." }
    if ($source -match '\$matches\s*\+=') { throw "Manifest parser still appends to the automatic \$Matches variable." }
    if ($source -notmatch '\$record\[3\] -ceq \$ArtifactName') { throw "Manifest filename equality is not ordinal/case-sensitive." }

    # Local artifact mode: present, reachable, and never displacing the remote path.
    if ($source -notmatch '\[string\]\$LocalDir = ""') { throw "The -LocalDir parameter is missing." }
    if ($source -notmatch 'function\s+Stage-LocalArtifacts') { throw "Local staging helper is missing." }
    if ($source -notmatch 'Stage-LocalArtifacts -LocalDir \$LocalDir') { throw "The local staging helper is never called." }
    if ($source -notmatch 'Stage-ReleaseArtifacts -ReleaseUrl \$ReleaseUrl') { throw "The remote staging call was displaced by local mode." }
    if ($source -notmatch '\[local\] SHA256SUMS not found') { throw "Local mode has no manifest-skip notice." }
    if ($source -match 'wintun\.net|Expand-Archive|wintun\.zip') { throw "Local mode introduced an unverified Wintun ZIP fallback." }

    Write-Host "C1 installer tests passed." -ForegroundColor Green
} finally {
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
