#Requires -RunAsAdministrator

$ErrorActionPreference = "Continue"
$REPO_OWNER = "wantianle"
$REPO_NAME  = "sdwan-go"
$REPO_BRANCH = "master"
$INSTALL_DIR = "C:\ProgramData\sdwan"
$START_MENU_DIR = Join-Path $env:ProgramData "Microsoft\Windows\Start Menu\Programs"
$SHORTCUT_PATH = Join-Path $START_MENU_DIR "SDWAN Panel.lnk"
$GH_PROXIES = @("https://gh-proxy.com/", "https://gh.ddlc.top/", "https://gh.idayer.com/")  # GitHub mirrors (verified working 2025-06-29)
$DOWNLOAD_CONNECT_TIMEOUT_MS = 15000
$DOWNLOAD_READ_TIMEOUT_MS = 15000
$DOWNLOAD_OVERALL_TIMEOUT_SEC = 90
$DOWNLOAD_BUFFER_SIZE = 65536

Write-Host ""
Write-Host "===========================================" -ForegroundColor Cyan
Write-Host "  SD-WAN Windows Installer" -ForegroundColor Cyan
Write-Host "===========================================" -ForegroundColor Cyan
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

$releaseUrl = "https://github.com/$REPO_OWNER/$REPO_NAME/releases/latest/download"

Write-Host "[2/5] Download components..."

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

# ────────────────────────────────────────────────────────────
# 3. Server selection
# ────────────────────────────────────────────────────────────
$configPath = "$INSTALL_DIR\iwan.conf"
$useExistingConfig = $false

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

$servers = @(
    @{Name="minieye.9966.org"; Desc="Telecom dedicated 1000M (recommended)"},
    @{Name="dwan.minieye.tech"; Desc="Telecom broadband 3x100M"},
    @{Name="minieye.8866.org"; Desc="China Mobile dedicated 500M"},
    @{Name="minieye.2288.org"; Desc="China Unicom broadband 200M"},
    @{Name="youjia.8866.org"; Desc="Telecom dedicated 50M (finance)"}
)

function Get-TcpLatencyMs {
    param(
        [string]$Server,
        [int]$Port = 443,
        [int]$TimeoutMs = 2000
    )

    $client = New-Object System.Net.Sockets.TcpClient
    $watch = [System.Diagnostics.Stopwatch]::StartNew()

    try {
        $async = $client.BeginConnect($Server, $Port, $null, $null)
        try {
            if (-not $async.AsyncWaitHandle.WaitOne($TimeoutMs, $false)) {
                return -1
            }

            $client.EndConnect($async)
        } finally {
            $async.AsyncWaitHandle.Close()
        }
        $watch.Stop()
        return [Math]::Max(1, [int]$watch.ElapsedMilliseconds)
    } catch {
        return -1
    } finally {
        if ($watch.IsRunning) { $watch.Stop() }
        $client.Close()
    }
}

function Get-ServerLatencyMs {
    param([string]$Server)

    $tcp443 = Get-TcpLatencyMs -Server $Server -Port 443 -TimeoutMs 2000
    if ($tcp443 -gt 0) {
        return $tcp443
    }

    $tcp10010 = Get-TcpLatencyMs -Server $Server -Port 10010 -TimeoutMs 2000
    if ($tcp10010 -gt 0) {
        return $tcp10010
    }

    try {
        $ping = Test-Connection -ComputerName $Server -Count 1 -TimeoutSeconds 2 -ErrorAction SilentlyContinue
        if ($ping) {
            return [Math]::Max(1, [int]$ping.ResponseTime)
        }
    } catch {}

    return -1
}

if (-not $useExistingConfig) {
    Write-Host ""
    Write-Host "[3/5] Server selection (checking latency...)" -ForegroundColor Cyan

    for ($i=0; $i -lt $servers.Count; $i++) {
        $s = $servers[$i]
        $lat = "timeout/unreachable"
        $latColor = "DarkGray"
        try {
            $ms = Get-ServerLatencyMs -Server $s.Name
            if ($ms -gt 0) {
                $lat = "${ms}ms"
                if ($ms -lt 20) {
                    $latColor = "Green"
                } elseif ($ms -le 80) {
                    $latColor = "Yellow"
                } else {
                    $latColor = "Red"
                }
            }
        } catch {
            $latColor = "DarkGray"
        }
        $mark = if ($i -eq 0) { " [default]" } else { "" }
        Write-Host "  [$($i+1)] $($s.Name) - $($s.Desc)  " -NoNewline
        Write-Host $lat -ForegroundColor $latColor -NoNewline
        Write-Host $mark
    }

    $choice = Read-Host "`nSelect server (Enter = 1)"
    $idx = 0
    if ($choice -match '^\d+$' -and [int]$choice -ge 1 -and [int]$choice -le 5) {
        $idx = [int]$choice - 1
    }
    $selectedServer = $servers[$idx].Name
    Write-Host "  Selected: $selectedServer" -ForegroundColor Green
}

# ────────────────────────────────────────────────────────────
# 4. Credentials & config
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[4/5] Account config" -ForegroundColor Cyan
if ($useExistingConfig) {
    Write-Host "  Using existing config: $configPath" -ForegroundColor Green
} else {
    $username = Read-Host "  Username"
    $password = Read-Host "  Password" -AsSecureString
    $passwordPlain = [System.Runtime.InteropServices.Marshal]::PtrToStringAuto(
        [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($password))

    $configContent = @"
server=$selectedServer
username=$username
password=$passwordPlain
port=10010
mtu=1436
encrypt=0
tunname=iwan1
routenet=192.168.0.0/16
"@

    Set-Content -Path $configPath -Value $configContent
    Write-Host "  Config saved: $configPath" -ForegroundColor Green
}

# ────────────────────────────────────────────────────────────
# 5. Auto-start & launch
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[5/5] Auto-start & launch" -ForegroundColor Cyan

$autoStart = Read-Host "  Enable auto-start? (y/n, default y)"
if ($autoStart -ne "n") {
    $taskName = "SDWAN Panel"
    schtasks /create /tn $taskName `
        /tr "$INSTALL_DIR\panel.exe" `
        /sc onstart /ru SYSTEM /rl highest /f 2>&1 | Out-Null
    Write-Host "  Auto-start task created: $taskName" -ForegroundColor Green
}

try {
    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut($SHORTCUT_PATH)
    $shortcut.TargetPath = "$INSTALL_DIR\panel.exe"
    $shortcut.WorkingDirectory = $INSTALL_DIR
    $shortcut.IconLocation = "$INSTALL_DIR\panel.exe,0"
    $shortcut.Description = "SDWAN Panel"
    $shortcut.Save()
    Write-Host "  Start Menu shortcut created: $SHORTCUT_PATH" -ForegroundColor Green
} catch {
    Write-Host "  Start Menu shortcut creation failed" -ForegroundColor Yellow
}

Write-Host "  Launching panel..." -ForegroundColor Cyan
Start-Process -FilePath "$INSTALL_DIR\panel.exe" -WorkingDirectory $INSTALL_DIR

Write-Host ""
Write-Host "===========================================" -ForegroundColor Cyan
Write-Host "  Install complete!" -ForegroundColor Green
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
