#Requires -RunAsAdministrator

# ────────────────────────────────────────────────────────────
# sdwan Windows uninstaller
#
# One-liner (admin PowerShell, via GitHub proxy):
#   iwr -useb https://ghproxy.com/https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.ps1 | iex
# ────────────────────────────────────────────────────────────

$ErrorActionPreference = "SilentlyContinue"
$INSTALL_DIR = "C:\ProgramData\sdwan"

Write-Host "🗑️  正在卸载 SDWAN..." -ForegroundColor Yellow

# Kill running processes
Get-Process -Name "sdwan-panel" | Stop-Process -Force
Get-Process -Name "sdwan-windows-amd64" | Stop-Process -Force
Write-Host "✅ 进程已终止" -ForegroundColor Green

# Remove scheduled task
schtasks /delete /tn "SDWAN Panel" /f 2>&1 | Out-Null
Write-Host "✅ 开机自启已移除" -ForegroundColor Green

# Delete install directory
if (Test-Path $INSTALL_DIR) {
    Remove-Item -Recurse -Force $INSTALL_DIR
    Write-Host "✅ $INSTALL_DIR 已删除" -ForegroundColor Green
}

Write-Host ""
Write-Host "🗑️  卸载完成" -ForegroundColor Green
