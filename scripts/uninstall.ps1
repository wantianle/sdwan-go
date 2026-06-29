#Requires -RunAsAdministrator

# ────────────────────────────────────────────────────────────
# sdwan Windows uninstaller
#
# One-liner (admin PowerShell, via GitHub proxy):
#   iwr -useb https://ghproxy.com/https://raw.githubusercontent.com/wantianle/sdwan-go/master/scripts/uninstall.ps1 | iex
# ────────────────────────────────────────────────────────────

$ErrorActionPreference = "SilentlyContinue"
$INSTALL_DIR = "C:\ProgramData\sdwan"
$SHORTCUT_PATH = Join-Path $env:ProgramData "Microsoft\Windows\Start Menu\Programs\SDWAN Panel.lnk"

Write-Host "Removing SDWAN..." -ForegroundColor Yellow

# Kill running processes
Get-Process -Name "panel" | Stop-Process -Force
Get-Process -Name "sdwan-windows-amd64" | Stop-Process -Force
Write-Host "Processes stopped" -ForegroundColor Green

# Remove scheduled task
schtasks /delete /tn "SDWAN Panel" /f 2>&1 | Out-Null
Write-Host "Auto-start task removed" -ForegroundColor Green

# Remove Start Menu shortcut
if (Test-Path $SHORTCUT_PATH) {
    Remove-Item $SHORTCUT_PATH -Force
    Write-Host "Start Menu shortcut removed" -ForegroundColor Green
}

# Delete install directory
if (Test-Path $INSTALL_DIR) {
    Remove-Item -Recurse -Force $INSTALL_DIR
    Write-Host "$INSTALL_DIR removed" -ForegroundColor Green
}

Write-Host ""
Write-Host "Uninstall complete" -ForegroundColor Green
