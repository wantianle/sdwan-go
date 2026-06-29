#Requires -RunAsAdministrator

# Force UTF-8 encoding for Chinese output (PowerShell console defaults to GBK)
$OutputEncoding = [Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding  = [System.Text.Encoding]::UTF8

# ────────────────────────────────────────────────────────────
# sdwan Windows installer — download binaries from GitHub,
# setup deployment directory, optional auto-start.
#
# One-liner (admin PowerShell):
#   iwr -useb https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/install.ps1 | iex
# ────────────────────────────────────────────────────────────

$ErrorActionPreference = "Stop"
$REPO_OWNER = "wantianle"
$REPO_NAME  = "sdwan-go"
$REPO_BRANCH = "master"
$INSTALL_DIR = "C:\ProgramData\sdwan"

Write-Host "" -ForegroundColor Cyan
Write-Host "═══════════════════════════════════════════" -ForegroundColor Cyan
Write-Host "  SD-WAN Windows 一键安装" -ForegroundColor Cyan
Write-Host "═══════════════════════════════════════════" -ForegroundColor Cyan
Write-Host ""

# ────────────────────────────────────────────────────────────
# 1. Create install directory
# ────────────────────────────────────────────────────────────
if (-not (Test-Path $INSTALL_DIR)) {
    New-Item -ItemType Directory -Path $INSTALL_DIR -Force | Out-Null
}
Write-Host "[1/5] 安装目录: $INSTALL_DIR" -ForegroundColor Green

# ────────────────────────────────────────────────────────────
# 2. Download binaries from GitHub
# ────────────────────────────────────────────────────────────
function Download-File {
    param($Urls, $Dest)
    $name = Split-Path $Urls[0] -Leaf
    Write-Host "  下载: $name ... " -NoNewline
    $ProgressPreference = 'SilentlyContinue'
    foreach ($url in $Urls) {
        # Try direct first (most reliable), then proxy
        foreach ($try in @($url, "https://ghproxy.com/$url")) {
            try {
                Invoke-WebRequest -Uri $try -OutFile $Dest -UseBasicParsing -ErrorAction Stop
                $tag = if ($try -match 'ghproxy') { 'proxy' } else { 'direct' }
                Write-Host "OK ($tag)" -ForegroundColor Green
                return
            } catch {
                $msg = $_.Exception.Message
                if ($msg.Length -gt 80) { $msg = $msg.Substring(0, 80) + "..." }
                Write-Host "`n    ⚠ $try → $msg" -ForegroundColor Yellow
            }
        }
    }
    Write-Host "FAILED (tried $($Urls.Count) sources)" -ForegroundColor Red
    throw "Download failed: $name"
}

$baseUrl = "https://raw.githubusercontent.com/$REPO_OWNER/$REPO_NAME/$REPO_BRANCH/dist"
$releaseUrl = "https://github.com/$REPO_OWNER/$REPO_NAME/releases/latest/download"

Write-Host "[2/5] 下载组件..."

# Each file tries raw dist first, then GitHub Release as fallback
$coreUrls  = @("$baseUrl/sdwan-windows-amd64.exe", "$releaseUrl/sdwan-windows-amd64.exe")
$panelUrls = @("$baseUrl/sdwan-panel.exe", "$releaseUrl/sdwan-panel.exe")
$wintunUrls = @(
    "$baseUrl/wintun.dll",
    "$releaseUrl/wintun.dll",
    "https://www.wintun.net/builds/wintun-0.14.1.zip"
)

Download-File -Urls $coreUrls  -Dest "$INSTALL_DIR\sdwan-windows-amd64.exe"
Download-File -Urls $panelUrls -Dest "$INSTALL_DIR\sdwan-panel.exe"

# wintun.dll: bundled in dist/, or downloaded from wintun.net (zip → extract)
try {
    if (Test-Path "$INSTALL_DIR\wintun.dll") {
        Write-Host "  wintun.dll 已存在，跳过" -ForegroundColor Green
    } else {
        Download-File -Urls $wintunUrls -Dest "$env:TEMP\wintun.zip"
        $extractDir = "$env:TEMP\wintun_extract"
        Expand-Archive -Path "$env:TEMP\wintun.zip" -DestinationPath $extractDir -Force
        $dll = Get-ChildItem -Path $extractDir -Recurse -Filter "wintun.dll" | Where-Object { $_.Directory.Name -eq "amd64" } | Select-Object -First 1
        if ($dll) {
            Copy-Item $dll.FullName "$INSTALL_DIR\wintun.dll"
            Write-Host "  下载: wintun.dll ... OK (官方)" -ForegroundColor Green
        } else {
            throw "未在 zip 中找到 wintun.dll"
        }
        Remove-Item "$env:TEMP\wintun.zip", $extractDir -Recurse -Force -ErrorAction SilentlyContinue
    }
} catch {
    Write-Host "  提示: wintun.dll 下载失败，请手动从 https://www.wintun.net/ 下载放入 $INSTALL_DIR" -ForegroundColor Yellow
}

# ────────────────────────────────────────────────────────────
# 3. Server selection
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[3/5] 服务器选择 (检测延迟中...)" -ForegroundColor Cyan
$servers = @(
    @{Name="minieye.9966.org"; Desc="电信专线 1000M (推荐)"},
    @{Name="dwan.minieye.tech"; Desc="电信普宽 3x100M"},
    @{Name="minieye.8866.org"; Desc="移动专线 500M"},
    @{Name="minieye.2288.org"; Desc="联通普宽 200M"},
    @{Name="youjia.8866.org"; Desc="电信专线 50M (财务)"}
)

for ($i=0; $i -lt $servers.Count; $i++) {
    $s = $servers[$i]
    $lat = "-"
    try {
        $ping = Test-Connection -ComputerName $s.Name -Count 1 -TimeoutSeconds 2000 -ErrorAction SilentlyContinue
        if ($ping) { $lat = "$($ping.ResponseTime)ms" }
    } catch {}
    $mark = if ($i -eq 0) { " [默认]" } else { "" }
    Write-Host "  [$($i+1)] $($s.Name) - $($s.Desc)  $lat$mark"
}

$choice = Read-Host "`n选择服务器 (直接回车=1)"
$idx = 0
if ($choice -match '^\d+$' -and [int]$choice -ge 1 -and [int]$choice -le 5) {
    $idx = [int]$choice - 1
}
$selectedServer = $servers[$idx].Name
Write-Host "  已选择: $selectedServer" -ForegroundColor Green

# ────────────────────────────────────────────────────────────
# 4. Credentials & config
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[4/5] 账户配置" -ForegroundColor Cyan
$username = Read-Host "  工号 (username)"
$password = Read-Host "  SDWAN 密码" -AsSecureString
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
    $overwrite = Read-Host "  配置已存在，覆盖? (y/n)"
    if ($overwrite -ne "y") {
        Write-Host "  保留现有配置" -ForegroundColor Green
    } else {
        Set-Content -Path $configPath -Value $configContent
    }
} else {
    Set-Content -Path $configPath -Value $configContent
}
Write-Host "  配置已保存: $configPath" -ForegroundColor Green

# ────────────────────────────────────────────────────────────
# 5. Auto-start & launch
# ────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "[5/5] 开机自启 & 启动" -ForegroundColor Cyan

$autoStart = Read-Host "  是否开机自启? (y/n, 默认 y)"
if ($autoStart -ne "n") {
    $taskName = "SDWAN Panel"
    schtasks /create /tn $taskName `
        /tr "$INSTALL_DIR\sdwan-panel.exe" `
        /sc onstart /ru SYSTEM /rl highest /f 2>&1 | Out-Null
    Write-Host "  已添加开机自启任务: $taskName" -ForegroundColor Green
}

Write-Host "  正在启动 SDWAN 面板..." -ForegroundColor Cyan
Start-Process -FilePath "$INSTALL_DIR\sdwan-panel.exe" -WorkingDirectory $INSTALL_DIR

Write-Host ""
Write-Host "═══════════════════════════════════════════" -ForegroundColor Cyan
Write-Host "  安装完成！" -ForegroundColor Green
Write-Host ""
Write-Host "  托盘已出现蓝色图标，左键双击打开面板" -ForegroundColor White
Write-Host "  右键托盘图标 → 退出" -ForegroundColor White
Write-Host ""
Write-Host "  管理命令:" -ForegroundColor Cyan
Write-Host "    Get-Content $INSTALL_DIR\sdwan.log -Wait -Tail 20  # 实时日志"
Write-Host "    Get-Content $INSTALL_DIR\sdwan-panel.log            # 面板日志"
Write-Host "    schtasks /delete /tn 'SDWAN Panel' /f               # 取消开机自启"
Write-Host "═══════════════════════════════════════════" -ForegroundColor Cyan
