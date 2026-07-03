#Requires -RunAsAdministrator

param(
    [string]$Version = "latest",
    [string]$TestHost = "hfs.minieye.tech"
)

# Usage:
#   Saved script: .\install.ps1 1.0.29
#   Saved script: .\install.ps1 v1.0.29
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
# 1. Create install directory
# ────────────────────────────────────────────────────────────
if (-not (Test-Path $INSTALL_DIR)) {
    New-Item -ItemType Directory -Path $INSTALL_DIR -Force | Out-Null
}
Write-Host "[1/5] Install dir: $INSTALL_DIR" -ForegroundColor Green

# ────────────────────────────────────────────────────────────
# 2. Download binaries from GitHub Release
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

if ($Version -eq "latest") {
    $releaseUrl = "https://github.com/$REPO_OWNER/$REPO_NAME/releases/latest/download"
} else {
    $releaseUrl = "https://github.com/$REPO_OWNER/$REPO_NAME/releases/download/$Version"
}

Write-Host "[2/5] Download components..."
Write-Host "  Release version: $Version"

# Release only. dist/ is not committed to git.
$coreUrls  = @("$releaseUrl/sdwan-windows-amd64.exe")
$panelUrls = @("$releaseUrl/panel.exe")
$wintunUrls = @(
    "$releaseUrl/wintun.dll",
    "https://www.wintun.net/builds/wintun-0.14.1.zip"
)

Download-File -Urls $coreUrls  -Dest "$INSTALL_DIR\sdwan-windows-amd64.exe"
Download-File -Urls $panelUrls -Dest "$INSTALL_DIR\panel.exe"

# wintun.dll: bundled in Release, or downloaded from wintun.net (zip -> extract)
try {
    if (Test-Path "$INSTALL_DIR\wintun.dll") {
        Write-Host "  wintun.dll already exists, skip" -ForegroundColor Green
    } else {
        Download-File -Urls @($wintunUrls[0]) -Dest "$INSTALL_DIR\wintun.dll"
        if (-not (Test-Path "$INSTALL_DIR\wintun.dll")) {
            throw "wintun.dll not found after download"
        }
    }
} catch {
    Write-Host "  Release wintun.dll unavailable, fallback to official wintun zip..." -ForegroundColor Yellow
    try {
        $zipPath = "$env:TEMP\wintun.zip"
        Download-File -Urls @($wintunUrls[1]) -Dest $zipPath
        $extractDir = "$env:TEMP\wintun_extract"
        Expand-Archive -Path $zipPath -DestinationPath $extractDir -Force
        $dll = Get-ChildItem -Path $extractDir -Recurse -Filter "wintun.dll" | Where-Object { $_.Directory.Name -eq "amd64" } | Select-Object -First 1
        if ($dll) {
            Copy-Item $dll.FullName "$INSTALL_DIR\wintun.dll" -Force
            Write-Host "  wintun.dll OK (official zip)" -ForegroundColor Green
        } else {
            throw "wintun.dll not found inside zip"
        }
        Remove-Item $zipPath, $extractDir -Recurse -Force -ErrorAction SilentlyContinue
    } catch {
        Write-Host "  Hint: download wintun.dll manually from https://www.wintun.net/ and put it into $INSTALL_DIR" -ForegroundColor Yellow
    }
}

# Download tray icon for Start Menu shortcut
$trayIconPath = "$INSTALL_DIR\tray.ico"
if (-not (Test-Path $trayIconPath)) {
    $repoRawUrl = "https://raw.githubusercontent.com/wantianle/sdwan-go/master"
    try {
        Download-File -Urls @("$repoRawUrl/panel/frontend/tray.ico") -Dest $trayIconPath
    } catch {
        Write-Host "  tray.ico download failed, shortcut will use panel.exe icon fallback" -ForegroundColor Yellow
    }
}

# ────────────────────────────────────────────────────────────
# Probe-MTU: binary search (548..1472) the largest IPv4 ICMP
# echo payload that passes with the DF bit set.
# Returns the tunnel MTU (payload + 28 - 64, clamped [1200,1436]).
function Probe-MTU {
    param([string]$Server)

    Write-Host "  Auto-probing MTU ($Server)..." -ForegroundColor Cyan

    $low = 548
    $high = 1472
    $best = 0

    while ($low -le $high) {
        $mid = [math]::Floor(($low + $high) / 2)
        $pingArgs = @("-4", "-f", "-l", "$mid", "-n", "1", "-w", "1000", $Server)
        & ping.exe $pingArgs 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) {
            $best = $mid
            $low = $mid + 1
        } else {
            $high = $mid - 1
        }
    }

    if ($best -eq 0) {
        Write-Host "  MTU probe failed — using default 1436 (non-fatal warning)" -ForegroundColor Yellow
        return 1436
    }

    $candidate = $best + 28 - 64
    if ($candidate -lt 1200) { $candidate = 1200 }
    if ($candidate -gt 1436) { $candidate = 1436 }

    Write-Host "  MTU probe OK: $candidate (max payload=$best)" -ForegroundColor Green
    return $candidate
}

function Grant-ConfigAccess {
    param([string]$Path)

    if (-not (Test-Path $Path)) { return }

    $user = if ($env:USERDOMAIN) { "$($env:USERDOMAIN)\$($env:USERNAME)" } else { $env:USERNAME }
    try {
        & icacls.exe $Path /grant:r "$user`:(M)" | Out-Null
        Write-Host "  Config writable for current user: $user" -ForegroundColor Green
    } catch {
        Write-Host "  Warning: could not grant config write access automatically" -ForegroundColor Yellow
    }
}

# ────────────────────────────────────────────────────────────
# 3. Server selection
# ────────────────────────────────────────────────────────────
$configPath = "$INSTALL_DIR\iwan.conf"
$useExistingConfig = $false
$selectedServer = "minieye.9966.org"

if (Test-Path $configPath) {
    Write-Host ""
    Write-Host "[3/5] Existing config detected: $configPath" -ForegroundColor Cyan
    while ($true) {
        $configChoice = Read-Host "  Choose: [v] view existing / [u] use existing / [o] overwrite"
        switch ($configChoice.ToLower()) {
            "v" {
                Write-Host ""
                Write-Host "  Existing iwan.conf:" -ForegroundColor Cyan
                Get-Content -Path $configPath | ForEach-Object { Write-Host "    $_" }
                Write-Host ""
            }
            "u" {
                $useExistingConfig = $true
                break
            }
            "o" {
                break
            }
            default {
                Write-Host "  Enter v, u, or o" -ForegroundColor Yellow
            }
        }
        if ($useExistingConfig) { break }
    }
}

if (-not $useExistingConfig) {
    Write-Host ""
    Write-Host "[3/5] Server selection" -ForegroundColor Cyan
    Write-Host "  Default server: $selectedServer" -ForegroundColor Green
}

# ────────────────────────────────────────────────────────────
# 4. Credentials & config
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[4/5] Account config" -ForegroundColor Cyan
if ($useExistingConfig) {
    Write-Host "  Using existing config: $configPath" -ForegroundColor Green
} else {
    do {
        $username = Read-Host "  Username"
        if ([string]::IsNullOrWhiteSpace($username)) {
            Write-Host "  Username cannot be empty" -ForegroundColor Yellow
        }
    } while ([string]::IsNullOrWhiteSpace($username))

    do {
        $password = Read-Host "  Password" -AsSecureString
        $passwordPlain = [System.Runtime.InteropServices.Marshal]::PtrToStringAuto(
            [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($password))
        if ([string]::IsNullOrWhiteSpace($passwordPlain)) {
            Write-Host "  Password cannot be empty" -ForegroundColor Yellow
        }
    } while ([string]::IsNullOrWhiteSpace($passwordPlain))

    $probedMtu = Probe-MTU -Server $selectedServer

    $configContent = @"
server=$selectedServer
username=$username
password=$passwordPlain
port=10010
mtu=$probedMtu
encrypt=0
tunname=iwan1
routenet=192.168.0.0/16
"@

    Set-Content -Path $configPath -Value $configContent
    Write-Host "  Config saved: $configPath" -ForegroundColor Green
}

Grant-ConfigAccess -Path $configPath

# ────────────────────────────────────────────────────────────
# 5. Auto-start & launch
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[5/5] Auto-start & launch" -ForegroundColor Cyan

$autoStart = Read-Host "  Enable auto-start? (y/n, default y)"
if ($autoStart -ne "n") {
    $taskName = "SDWAN Panel"
    Write-Host "  Creating auto-start task..." -ForegroundColor Cyan
    schtasks /create /tn $taskName /tr "`"$INSTALL_DIR\panel.exe`"" /sc onlogon /rl highest /f
    if ($LASTEXITCODE -ne 0) {
        Write-Host "  Auto-start task creation failed (exit code $LASTEXITCODE)" -ForegroundColor Red
    } else {
        schtasks /query /tn $taskName 2>&1 | Out-Null
        if ($LASTEXITCODE -eq 0) {
            Write-Host "  Auto-start task created: $taskName" -ForegroundColor Green
        } else {
            Write-Host "  Auto-start task verification failed" -ForegroundColor Yellow
        }
    }
}

try {
    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut($SHORTCUT_PATH)
    $shortcut.TargetPath = "$INSTALL_DIR\panel.exe"
    $shortcut.WorkingDirectory = $INSTALL_DIR
    $iconPath = "$INSTALL_DIR\tray.ico"
    if (Test-Path $iconPath) {
        $shortcut.IconLocation = $iconPath
    } else {
        $shortcut.IconLocation = "$INSTALL_DIR\panel.exe,0"
    }
    $shortcut.Description = "SDWAN Panel"
    $shortcut.Save()
    Write-Host "  Start Menu shortcut created: $SHORTCUT_PATH" -ForegroundColor Green
} catch {
    Write-Host "  Start Menu shortcut creation failed" -ForegroundColor Yellow
}

Write-Host "  Launching panel..." -ForegroundColor Cyan
$logPath = Join-Path $INSTALL_DIR "sdwan.log"
if (Test-Path $logPath) {
    Move-Item -Path $logPath -Destination "$logPath.old" -Force -ErrorAction SilentlyContinue
}
Start-Process -FilePath "$INSTALL_DIR\panel.exe" -WorkingDirectory $INSTALL_DIR
Start-Sleep -Seconds 4

function Test-PostInstallStatus {
    Write-Host ""
    Write-Host "  Validating tunnel startup (up to 15s)..." -ForegroundColor Cyan

    $authRejected = $false
    $sdwanIP = $null
    $routeOK = $false

    for ($i = 0; $i -lt 15; $i++) {
        Start-Sleep -Seconds 1

        # Scan logs ONLY for AUTH REJECTED — not for success
        if (-not $authRejected -and (Test-Path $logPath)) {
            $logText = Get-Content -Path $logPath -Tail 80 -ErrorAction SilentlyContinue | Out-String
            if ($logText -match "AUTH REJECTED") {
                $authRejected = $true
                break
            }
        }

        # Check iwan1 IPv4
        if (-not $sdwanIP) {
            try {
                $ipObj = Get-NetIPAddress -InterfaceAlias "iwan1" -AddressFamily IPv4 -ErrorAction SilentlyContinue | Select-Object -First 1
                if ($ipObj) { $sdwanIP = $ipObj.IPAddress }
            } catch {}
            if (-not $sdwanIP) {
                $netsh = netsh interface ip show addresses "iwan1" 2>$null | Out-String
                if ($netsh -match "IP Address:\s*([0-9\.]+)") { $sdwanIP = $Matches[1] }
            }
        }

        # Check route 192.168.0.0/16 via iwan1
        if (-not $routeOK) {
            try {
                $rt = Get-NetRoute -DestinationPrefix "192.168.0.0/16" -ErrorAction SilentlyContinue | Where-Object { $_.InterfaceAlias -eq "iwan1" }
                if ($rt) { $routeOK = $true }
            } catch {}
            if (-not $routeOK) {
                $rtPrint = route print 192.168.0.0 2>$null | Out-String
                if ($rtPrint -match "192\.168\.0\.0.*iwan1") { $routeOK = $true }
            }
        }

        # Stop waiting if iwan1 has IPv4 and route is present
        if ($sdwanIP -and $routeOK) {
            break
        }
    }

    if ($sdwanIP) {
        Write-Host "  iwan1 IPv4: $sdwanIP" -ForegroundColor Green
    } else {
        Write-Host "  iwan1 IPv4 not detected" -ForegroundColor Yellow
    }

    if ($routeOK) {
        Write-Host "  Route: 192.168.0.0/16 -> iwan1" -ForegroundColor Green
    } else {
        Write-Host "  Route: 192.168.0.0/16 -> iwan1 not confirmed" -ForegroundColor Yellow
    }

    if ($authRejected) {
        Write-Host "  AUTH REJECTED: check username/password" -ForegroundColor Red
        return 1   # auth rejected
    } elseif ($sdwanIP -and $routeOK) {
        Write-Host "  Tunnel status: established" -ForegroundColor Green
    } else {
        Write-Host "  Tunnel status: timeout/unknown" -ForegroundColor Yellow
        return 2   # timeout
    }

    $pingOk = $false
    for ($attempt = 0; $attempt -lt 3; $attempt++) {
        if ($attempt -gt 0) { Start-Sleep -Seconds 1 }
        try {
            $ping = & ping.exe -n 1 -w 3000 $TestHost 2>$null | Out-String
            if ($LASTEXITCODE -eq 0) { $pingOk = $true; break }
        } catch {}
    }
    if ($pingOk) {
        Write-Host "  Internal connectivity: $TestHost reachable" -ForegroundColor Green
    } else {
        Write-Host "  Internal connectivity: $TestHost not reachable yet (warning only)" -ForegroundColor Yellow
    }
    return 0   # ok
}

$result = Test-PostInstallStatus

Write-Host ""
Write-Host "===========================================" -ForegroundColor Cyan
if ($result -eq 1) {
    Write-Host "  AUTH REJECTED - check username/password" -ForegroundColor Red
    Write-Host ""
    Write-Host "  Edit config and restart:" -ForegroundColor Yellow
    Write-Host "    Config file: $INSTALL_DIR\iwan.conf"
    Write-Host "    notepad $INSTALL_DIR\iwan.conf"
    Write-Host "    (If access is still denied, reopen Notepad as Administrator)"
    Write-Host "    taskkill /f /im panel.exe"
    Write-Host "    taskkill /f /im sdwan-windows-amd64.exe"
    Write-Host "    Start-Process $INSTALL_DIR\panel.exe"
} elseif ($result -eq 0) {
    Write-Host "  Install complete!" -ForegroundColor Green
} else {
    Write-Host "  Install complete (tunnel may need attention)" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "  Check config/logs and restart:" -ForegroundColor Yellow
    Write-Host "    Config file: $INSTALL_DIR\iwan.conf"
    Write-Host "    notepad $INSTALL_DIR\iwan.conf"
    Write-Host "    Get-Content $INSTALL_DIR\sdwan.log -Wait -Tail 20"
    Write-Host "    taskkill /f /im panel.exe"
    Write-Host "    taskkill /f /im sdwan-windows-amd64.exe"
    Write-Host "    Start-Process $INSTALL_DIR\panel.exe"
}
Write-Host ""
Write-Host "  Tray icon should appear shortly." -ForegroundColor White
Write-Host "  Double-click tray icon to open panel." -ForegroundColor White
Write-Host "  Right-click tray icon -> Exit" -ForegroundColor White
Write-Host ""
Write-Host "  Useful commands:" -ForegroundColor Cyan
Write-Host "    Get-Content $INSTALL_DIR\sdwan.log -Wait -Tail 20"
Write-Host "    Get-Content $INSTALL_DIR\panel.log"
Write-Host "    schtasks /delete /tn 'SDWAN Panel' /f"
Write-Host "===========================================" -ForegroundColor Cyan
