#Requires -RunAsAdministrator

$ErrorActionPreference = "Continue"
$REPO_OWNER = "wantianle"
$REPO_NAME  = "sdwan-go"
$REPO_BRANCH = "master"
$INSTALL_DIR = "C:\ProgramData\sdwan"
$GH_PROXIES = @("https://gh-proxy.com/", "https://gh.ddlc.top/", "https://gh.idayer.com/")  # GitHub mirrors (verified working 2025-06-29)

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
    $name = Split-Path $Urls[0] -Leaf
    Write-Host "  Download: $name"
    $ProgressPreference = 'SilentlyContinue'
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
        Write-Host ("    [{0}/{1}] {2} ... " -f ($i + 1), $allTries.Count, $label) -NoNewline
        try {
            Invoke-WebRequest -Uri $try -OutFile $Dest -UseBasicParsing -ErrorAction Stop
            Write-Host "OK" -ForegroundColor Green
            return
        } catch {
            $msg = $_.Exception.Message
            if ($msg.Length -gt 100) { $msg = $msg.Substring(0, 100) + "..." }
            Write-Host "FAILED" -ForegroundColor Yellow
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
Write-Host ""
Write-Host "[3/5] Server selection (checking latency...)" -ForegroundColor Cyan
$servers = @(
    @{Name="minieye.9966.org"; Desc="Telecom dedicated 1000M (recommended)"},
    @{Name="dwan.minieye.tech"; Desc="Telecom broadband 3x100M"},
    @{Name="minieye.8866.org"; Desc="China Mobile dedicated 500M"},
    @{Name="minieye.2288.org"; Desc="China Unicom broadband 200M"},
    @{Name="youjia.8866.org"; Desc="Telecom dedicated 50M (finance)"}
)

for ($i=0; $i -lt $servers.Count; $i++) {
    $s = $servers[$i]
    $lat = "-"
    try {
        $ping = Test-Connection -ComputerName $s.Name -Count 1 -TimeoutSeconds 2000 -ErrorAction SilentlyContinue
        if ($ping) { $lat = "$($ping.ResponseTime)ms" }
    } catch {}
    $mark = if ($i -eq 0) { " [default]" } else { "" }
    Write-Host "  [$($i+1)] $($s.Name) - $($s.Desc)  $lat$mark"
}

$choice = Read-Host "`nSelect server (Enter = 1)"
$idx = 0
if ($choice -match '^\d+$' -and [int]$choice -ge 1 -and [int]$choice -le 5) {
    $idx = [int]$choice - 1
}
$selectedServer = $servers[$idx].Name
Write-Host "  Selected: $selectedServer" -ForegroundColor Green

# ────────────────────────────────────────────────────────────
# 4. Credentials & config
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[4/5] Account config" -ForegroundColor Cyan
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

$configPath = "$INSTALL_DIR\iwan.conf"
if (Test-Path $configPath) {
    $overwrite = Read-Host "  Config exists, overwrite? (y/n)"
    if ($overwrite -ne "y") {
        Write-Host "  Keep existing config" -ForegroundColor Green
    } else {
        Set-Content -Path $configPath -Value $configContent
    }
} else {
    Set-Content -Path $configPath -Value $configContent
}
Write-Host "  Config saved: $configPath" -ForegroundColor Green

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
