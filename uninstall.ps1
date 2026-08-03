<#
.SYNOPSIS
  Removes everything installed by install.ps1 (keeps config backups and src/).
#>
[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$BridgeName = "opencode-zen-bridge"

Write-Host "==> Stopping and removing Scheduled Task"
if (Get-ScheduledTask -TaskName $BridgeName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $BridgeName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $BridgeName -Confirm:$false
}

Write-Host "==> Removing binary"
$BridgePath = Join-Path $HOME ".local\bin\$BridgeName.exe"
Remove-Item -Path $BridgePath -Force -ErrorAction SilentlyContinue

Write-Host "==> Removing bridge-generated model catalog"
Remove-Item -Path (Join-Path $HOME ".local\share\opencode\codex-models.json") -Force -ErrorAction SilentlyContinue

Write-Host "==> Reverting codex config"
$ConfigPath = Join-Path $HOME ".codex\config.toml"
$backups = Get-ChildItem -Path "$ConfigPath.bak.*" -ErrorAction SilentlyContinue |
    Sort-Object Name -Descending
if ($backups) {
    Copy-Item $backups[0].FullName $ConfigPath -Force
    Write-Host "    restored $($backups[0].Name)"
} else {
    Write-Host "    no backup found; leaving $ConfigPath as-is"
}

Write-Host "==> Done."