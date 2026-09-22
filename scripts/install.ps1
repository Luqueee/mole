# scripts/install.ps1 — install mole on Windows.
#
# A local clone is built with Go. Otherwise the published Windows archive
# is downloaded and verified against the release asset's SHA-256 digest.
# Configuration is handled by `mole init`.
#
# Usage (PowerShell):
#   .\scripts\install.ps1                         # from a clone
#   irm .../install.ps1 | iex                     # one-liner
#   .\scripts\install.ps1 -Prefix 'C:\Tools'      # custom prefix
#   .\scripts\install.ps1 -InstallDir 'C:\Tools'  # custom directory
#   .\scripts\install.ps1 -Init                   # also run mole init
#
# Environment variables:
#   MOLE_VERSION    release tag to install (default: latest release)
#   MOLE_SRC        path to an existing local clone to build with Go
#   INSTALL_DIR     absolute path for the installed binary
#                   (overrides -InstallDir and -Prefix)

[CmdletBinding()]
param(
    [string]$Prefix,
    [string]$InstallDir,
    [switch]$NoVerify,
    [switch]$Init,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'

# Track the temporary directory used for a downloaded release archive.
$script:installTmpRoot = $null

# Run cleanup on any error path (exception, Ctrl+C) too. `continue`
# re-raises the original error after cleanup runs.
trap {
    if ($script:installTmpRoot -and (Test-Path -LiteralPath $script:installTmpRoot)) {
        Remove-Item -LiteralPath $script:installTmpRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
    continue
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# ANSI escape codes for coloured output. PowerShell 5.1+ supports them
# on Windows 10+; older versions just show raw codes harmlessly.
$esc = [char]27
$script:UseColor = -not ([Console]::IsOutputRedirected -or $env:NO_COLOR)

function C($code, $text) {
    if ($script:UseColor) { return "$esc[${code}m$text$esc[0m" }
    return $text
}

function Step($msg) { Write-Host "$(C '1;38;2;125;133;144' '==') $msg" }
function Ok($msg)   { Write-Host "  $(C '38;2;63;185;80' 'OK') $msg" }
function Warn($msg) { Write-Host "  $(C '38;2;210;153;34' '!') $msg" }
function Die($msg)  { Write-Host "$(C '38;2;248;81;73' 'error:') $msg"; exit 1 }

function Test-MoleRepo {
    param([string]$dir)
    if (-not (Test-Path (Join-Path $dir 'go.mod')))    { return $false }
    if (-not (Test-Path (Join-Path $dir 'cmd/mole')))  { return $false }
    $goMod = Get-Content (Join-Path $dir 'go.mod') -Raw
    return ($goMod -match 'github\.com/Luqueee/mole')
}

function Find-LocalSource {
    if ($env:MOLE_SRC) {
        if (-not (Test-MoleRepo $env:MOLE_SRC)) { Die "MOLE_SRC is not a mole source directory: $env:MOLE_SRC" }
        return $env:MOLE_SRC
    }
    if ($env:MOLE_RELEASE_ONLY) { return $null }
    if (Test-MoleRepo $PWD) { return $PWD.Path }
    # $PSCommandPath is a script-scope automatic variable that always
    # points at the running .ps1 file. $MyInvocation.MyCommand.Path
    # would refer to this *function* (i.e. return "Find-LocalSource"),
    # which isn't what we want when looking for the script's parent
    # directory.
    if ($PSCommandPath) {
        $scriptDir = Split-Path -Parent $PSCommandPath
        $parent    = Split-Path -Parent $scriptDir
        if ($parent -and (Test-MoleRepo $parent)) { return (Resolve-Path $parent).Path }
    }
    return $null
}

function Show-Help {
    # Try to read the header comment block at the top of this script.
    # If we're being executed from a real file (e.g. `.\install.ps1
    # -Help` or `Get-Help .\install.ps1`), $PSCommandPath points at it.
    # If we're being run via `irm ... | iex`, the script is in memory
    # and $PSCommandPath is empty — fall back to a hardcoded copy.
    $fromFile = $false
    if ($PSCommandPath -and (Test-Path -LiteralPath $PSCommandPath)) {
        foreach ($line in (Get-Content -LiteralPath $PSCommandPath)) {
            if ($line -match '^#\s?(.*)$') {
                $matches[1]
                $fromFile = $true
            } else {
                break
            }
        }
    }
    if (-not $fromFile) {
        # Mirror of the header block at the top of this file. If you
        # update the header, update this too.
        'Install mole on Windows: irm https://raw.githubusercontent.com/Luqueee/mole/main/scripts/install.ps1 | iex'
    }
}

if ($Help) { Show-Help; exit 0 }

# ---------------------------------------------------------------------------
# 1. Acquire binary
# ---------------------------------------------------------------------------

$projectRoot = Find-LocalSource
if ($projectRoot) {
    $goBin = $env:GO
    if (-not $goBin) { $goBin = (Get-Command go -ErrorAction SilentlyContinue).Source }
    if (-not $goBin) { Die "'go' is required to install from a local clone. Install Go or run the one-liner outside the clone." }
    $buildDir = Join-Path $projectRoot 'dist'
    New-Item -ItemType Directory -Path $buildDir -Force | Out-Null
    $binary = Join-Path $buildDir 'mole.exe'
    Step "building mole from $projectRoot"
    Push-Location $projectRoot
    try { & $goBin build -trimpath -o $binary ./cmd/mole } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path $binary)) { Die 'go build failed' }
} else {
    $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($arch.ToUpperInvariant()) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { Die "unsupported Windows architecture: $arch" }
    }
    $headers = @{ 'User-Agent' = 'mole-installer' }
    $api = 'https://api.github.com/repos/Luqueee/mole/releases'
    if ($env:MOLE_VERSION) {
        $tag = $env:MOLE_VERSION
        if ($tag -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') { Die 'MOLE_VERSION must be a release tag such as v0.1.0 on Windows' }
        $release = Invoke-RestMethod -Headers $headers -Uri "$api/tags/$tag"
    } else {
        $release = Invoke-RestMethod -Headers $headers -Uri "$api/latest"
    }
    $name = "mole_$($release.tag_name.TrimStart('v'))_windows_$arch.zip"
    $asset = @($release.assets | Where-Object { $_.name -eq $name }) | Select-Object -First 1
    if (-not $asset -or $asset.digest -notmatch '^sha256:([0-9a-fA-F]{64})$') { Die "release $($release.tag_name) has no verified $name asset" }
    $expectedHash = $Matches[1]
    $script:installTmpRoot = Join-Path $env:TEMP "mole-install-$(New-Guid)"
    New-Item -ItemType Directory -Path $script:installTmpRoot -Force | Out-Null
    $archive = Join-Path $script:installTmpRoot $name
    Step "downloading $name"
    Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri $asset.browser_download_url -OutFile $archive
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash
    if ($actualHash -ne $expectedHash) { Die "SHA-256 mismatch for $name" }
    Expand-Archive -LiteralPath $archive -DestinationPath $script:installTmpRoot
    $binary = Join-Path $script:installTmpRoot 'mole.exe'
    if (-not (Test-Path -LiteralPath $binary)) { Die "$name does not contain mole.exe" }
    Ok "verified SHA-256 for $name"
}

# ---------------------------------------------------------------------------
# 3. Install
# ---------------------------------------------------------------------------

function Resolve-Dest {
    if ($env:INSTALL_DIR) { return $env:INSTALL_DIR }
    if ($InstallDir)     { return (Join-Path $InstallDir 'mole.exe') }
    if ($Prefix)          { return (Join-Path (Join-Path $Prefix 'bin') 'mole.exe') }
    return (Join-Path $env:LOCALAPPDATA 'Programs\mole\mole.exe')
}

$dest    = Resolve-Dest
$destDir = Split-Path -Parent $dest
Step "installing to $dest"
if (-not (Test-Path $destDir)) { New-Item -ItemType Directory -Path $destDir -Force | Out-Null }
Copy-Item -LiteralPath $binary -Destination $dest -Force
Ok "installed $dest"

# ---------------------------------------------------------------------------
# 4. Verify
# ---------------------------------------------------------------------------

if (-not $NoVerify) {
    Step "verifying"
    try {
        $ver = & $dest version 2>&1
        if ($LASTEXITCODE -ne 0) { Die "'$dest version' failed: $ver" }
        Ok "$ver"
    } catch {
        Die "could not run '$dest version': $_"
    }
}

# ---------------------------------------------------------------------------
# 5. PATH hint
# ---------------------------------------------------------------------------

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$onPath   = $false
if ($userPath) {
    foreach ($p in $userPath.Split(';')) {
        if ($p.TrimEnd('\') -eq $destDir.TrimEnd('\')) { $onPath = $true; break }
    }
}
if (-not $onPath) {
    $machine = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if ($machine) {
        foreach ($p in $machine.Split(';')) {
            if ($p.TrimEnd('\') -eq $destDir.TrimEnd('\')) { $onPath = $true; break }
        }
    }
}
if (-not $onPath) {
    $newUserPath = if ($userPath) { "$userPath;$destDir" } else { $destDir }
    [Environment]::SetEnvironmentVariable('Path', $newUserPath, 'User')
    $env:Path += ";$destDir"
    Ok "added $destDir to user PATH"
}

# ---------------------------------------------------------------------------
# 6. Optional init
# ---------------------------------------------------------------------------

if ($Init) {
    # When stdin is a TTY, run interactively. When it's redirected
    # (the typical `irm | iex` case), run non-interactively so the
    # install is scriptable via the $env:MOLE_* variables documented
    # in `mole init -h`.
    if ([Console]::IsInputRedirected) {
        Step "running mole init (non-interactive; using MOLE_* env vars)"
        & $dest init -no-prompt
    } else {
        Step "running mole init (interactive)"
        & $dest init
    }
}

Step "done"
Write-Host ""
Write-Host "  $(C '1' $(C '38;2;230;237;243' 'mole')) $(C '38;2;63;185;80' 'installed successfully')"
Write-Host "  $(C '38;2;110;114;125' '-------------------------------------------------')"
Write-Host "  $(C '38;2;110;114;125' 'binary    ')$dest"
Write-Host "  $(C '38;2;110;114;125' 'configure ')$(C '38;2;173;186;199' 'mole init')   (interactive, run once per machine)"
Write-Host "  $(C '38;2;110;114;125' 'start     ')$(C '38;2;173;186;199' 'mole up')      (uses .\mole.yaml by default)"

# ---------------------------------------------------------------------------
# 7. Cleanup
# ---------------------------------------------------------------------------

# Explicit cleanup for the success path; the trap at top scope handles
# exception / Ctrl+C paths. Best-effort: failures here are suppressed so
# they don't mask any earlier error that put us on this code path.
if ($script:installTmpRoot -and (Test-Path -LiteralPath $script:installTmpRoot)) {
    Remove-Item -LiteralPath $script:installTmpRoot -Recurse -Force -ErrorAction SilentlyContinue
}
