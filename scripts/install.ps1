#Requires -RunAsAdministrator

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
$REPO_BRANCH = "main"
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
    param($Url, $Dest)
    Write-Host "  下载: $(Split-Path $Dest -Leaf) ... " -NoNewline
    try {
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -Uri $Url -OutFile $Dest -UseBasicParsing
        Write-Host "OK" -ForegroundColor Green
    } catch {
        Write-Host "FAILED" -ForegroundColor Red
        throw "Download failed: $Url"
    }
}

$baseUrl = "https://raw.githubusercontent.com/$REPO_OWNER/$REPO_NAME/$REPO_BRANCH/dist"
$releaseUrl = "https://github.com/$REPO_OWNER/$REPO_NAME/releases/latest/download"

Write-Host "[2/5] 下载组件..."

# Try releases first, fall back to raw dist/
$coreUrl = "$baseUrl/sdwan-windows-amd64.exe"
$panelUrl = "$baseUrl/sdwan-panel.exe"
$wintunUrl = "$baseUrl/wintun.dll"

Download-File -Url $coreUrl -Dest "$INSTALL_DIR\sdwan-windows-amd64.exe"
Download-File -Url $panelUrl -Dest "$INSTALL_DIR\sdwan-panel.exe"

# wintun.dll: bundled in dist/ or downloaded from wintun.net
try {
    Download-File -Url $wintunUrl -Dest "$INSTALL_DIR\wintun.dll"
} catch {
    Write-Host "  提示: wintun.dll 未找到，请手动从 https://www.wintun.net/ 下载放入 $INSTALL_DIR" -ForegroundColor Yellow
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
