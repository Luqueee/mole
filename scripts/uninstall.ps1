<#
scripts/uninstall.ps1 - remove a previously installed mole binary (Windows)

Removes the binary that scripts/install.ps1 would have placed, and
optionally removes its folder from the user PATH. Safe to run even if
mole isn't installed (exits 0 in that case).

Usage:
  pwsh -ExecutionPolicy Bypass -File scripts\uninstall.ps1
  pwsh -ExecutionPolicy Bypass -File scripts\uninstall.ps1 -InstallDir C:\Tools\mole
  pwsh -ExecutionPolicy Bypass -File scripts\uninstall.ps1 -Purge

Parameters:
  -InstallDir  Directory to remove from. Defaults to the install location
               used by install.ps1.
  -NoPath      Don't touch the user PATH.
  -Purge       Also remove $env:LOCALAPPDATA\mole\ (config files).
#>

[CmdletBinding()]
param(
    [string]$InstallDir = "",
    [switch]$NoPath,
    [switch]$Purge
)

$ErrorActionPreference = "Stop"

$Binary = "mole.exe"

# ---------------------------------------------------------------------------
# Resolve default location
# ---------------------------------------------------------------------------

if ([string]::IsNullOrEmpty($InstallDir)) {
    if ($env:LOCALAPPDATA) {
        $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\mole"
    } else {
        $InstallDir = Join-Path $env:USERPROFILE "scoop\apps\mole\current"
    }
}

$dest = Join-Path $InstallDir $Binary

# ---------------------------------------------------------------------------
# Remove binary
# ---------------------------------------------------------------------------

$removed = 0
if (Test-Path $dest) {
    Write-Host ">> removing $dest"
    Remove-Item -Force $dest
    $removed = 1
} else {
    Write-Host ">> mole was not installed at $dest."
}

# Remove the directory if it's now empty.
if (Test-Path $InstallDir) {
    $items = Get-ChildItem -Force $InstallDir
    if (-not $items) {
        Remove-Item -Force $InstallDir
    }
}

# ---------------------------------------------------------------------------
# PATH
# ---------------------------------------------------------------------------

if (-not $NoPath) {
    $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $segments = if ($currentPath) { $currentPath -split ";" } else { @() }
    if ($segments -contains $InstallDir) {
        $newPath = ($segments | Where-Object { $_ -ne $InstallDir }) -join ";"
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        $env:Path = ($env:Path -split ";" | Where-Object { $_ -ne $InstallDir }) -join ";"
        Write-Host "   removed $InstallDir from user PATH"
    }
}

# ---------------------------------------------------------------------------
# Optional config purge
# ---------------------------------------------------------------------------

if ($Purge) {
    $configDir = if ($env:LOCALAPPDATA) {
        Join-Path $env:LOCALAPPDATA "mole"
    } else {
        Join-Path $env:APPDATA "mole"
    }
    if (Test-Path $configDir) {
        Write-Host ">> purging $configDir"
        Remove-Item -Recurse -Force $configDir
    }
}

if ($removed -gt 0) {
    Write-Host ">> done."
} else {
    Write-Host ">> nothing to remove."
}
