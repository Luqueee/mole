# scripts/install.ps1 — install mole on Windows.
#
# This script mirrors scripts/install.sh. Configuration is delegated to
# `mole init` (a subcommand of the binary itself), so there is no
# configuration logic in this file. Whatever the Unix installer can do,
# `mole init` can do — keep both installers dumb by design.
#
# Usage (PowerShell):
#   .\scripts\install.ps1                         # from a clone
#   iwr .../install.ps1 | iex                     # one-liner
#   .\scripts\install.ps1 -Prefix 'C:\Tools'      # custom prefix
#   .\scripts\install.ps1 -Init                   # also run mole init
#
# Environment variables:
#   MOLE_VERSION    git ref to checkout when cloning (default: main)
#   MOLE_SRC        path to an existing local clone
#   INSTALL_DIR     absolute path for the installed binary
#                   (overrides -Prefix; if unset, defaults to
#                   C:\Program Files\mole\mole.exe or
#                   $env:USERPROFILE\.local\bin\mole.exe)

[CmdletBinding()]
param(
    [string]$Prefix,
    [switch]$NoVerify,
    [switch]$Init,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'

# Track the temp dir created when we have to clone upstream, so we can
# remove it on exit. Set by Resolve-Source; read by the cleanup block at
# the end of the script and by the trap below.
$script:cloneTmpRoot = $null

# Run cleanup on any error path (exception, Ctrl+C) too. `continue`
# re-raises the original error after cleanup runs.
trap {
    if ($script:cloneTmpRoot -and (Test-Path -LiteralPath $script:cloneTmpRoot)) {
        Remove-Item -LiteralPath $script:cloneTmpRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
    continue
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# ANSI escape codes for coloured output. PowerShell 5.1+ supports them
# on Windows 10+; older versions just show raw codes harmlessly.
$esc = [char]27
$script:UseColor = $true
if ($env:NO_COLOR) { $script:UseColor = $false }

function C($code, $text) {
    if ($script:UseColor) { return "$esc[${code}m$text$esc[0m" }
    return $text
}

function Step($msg) { Write-Host "$(C '1;38;2;125;133;144' '==') $msg" }
function Ok($msg)   { Write-Host "  $(C '38;2;63;185;80' [char]0x2713) $msg" }
function Warn($msg) { Write-Host "  $(C '38;2;210;153;34' '!') $msg" }
function Die($msg)  { Write-Host "$(C '38;2;248;81;73' 'error:') $msg"; exit 1 }

function Test-MoleRepo {
    param([string]$dir)
    if (-not (Test-Path (Join-Path $dir 'go.mod')))    { return $false }
    if (-not (Test-Path (Join-Path $dir 'cmd/mole')))  { return $false }
    $goMod = Get-Content (Join-Path $dir 'go.mod') -Raw
    return ($goMod -match 'github\.com/Luqueee/mole')
}

function Resolve-Source {
    if ($env:MOLE_SRC -and (Test-MoleRepo $env:MOLE_SRC)) {
        return $env:MOLE_SRC
    }
    if (Test-MoleRepo $PWD) {
        return $PWD
    }
    # $PSCommandPath is a script-scope automatic variable that always
    # points at the running .ps1 file. $MyInvocation.MyCommand.Path
    # would refer to this *function* (i.e. return "Resolve-Source"),
    # which isn't what we want when looking for the script's parent
    # directory.
    $scriptDir = Split-Path -Parent $PSCommandPath
    $parent    = Split-Path -Parent $scriptDir
    if ($parent -and (Test-MoleRepo $parent)) {
        return (Resolve-Path $parent).Path
    }

    if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
        Die "'git' is not available and no local mole source was found."
    }
    $ref     = if ($env:MOLE_VERSION) { $env:MOLE_VERSION } else { 'main' }
    $tmpRoot = Join-Path $env:TEMP "mole-install-$(New-Guid)"
    # Track for cleanup. Read by the trap at top scope and the explicit
    # cleanup at the end of the script.
    $script:cloneTmpRoot = $tmpRoot
    $tmp     = Join-Path $tmpRoot 'mole'
    Step "cloning https://github.com/Luqueee/mole.git (ref: $ref) into $tmp"
    New-Item -ItemType Directory -Path $tmpRoot -Force | Out-Null
    $clone = git clone --depth 1 --branch $ref https://github.com/Luqueee/mole.git $tmp 2>&1
    if ($LASTEXITCODE -ne 0) {
        Warn "branch '$ref' not found, falling back to default branch"
        $clone = git clone --depth 1 https://github.com/Luqueee/mole.git $tmp 2>&1
        if ($LASTEXITCODE -ne 0) { Die "git clone failed: $clone" }
    }
    return $tmp
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
        @'
scripts/install.ps1 — install mole on Windows.

This script mirrors scripts/install.sh. Configuration is delegated to
`mole init` (a subcommand of the binary itself), so there is no
configuration logic in this file. Whatever the Unix installer can do,
`mole init` can do — keep both installers dumb by design.

Usage (PowerShell):
  .\scripts\install.ps1                         # from a clone
  iwr .../install.ps1 | iex                     # one-liner
  .\scripts\install.ps1 -Prefix 'C:\Tools'      # custom prefix
  .\scripts\install.ps1 -Init                   # also run mole init

Environment variables:
  MOLE_VERSION    git ref to checkout when cloning (default: main)
  MOLE_SRC        path to an existing local clone
  INSTALL_DIR     absolute path for the installed binary
                  (overrides -Prefix; if unset, defaults to
                  C:\Program Files\mole\mole.exe or
                  $env:USERPROFILE\.local\bin\mole.exe)
'@
    }
}

if ($Help) { Show-Help; exit 0 }

# ---------------------------------------------------------------------------
# 1. Source
# ---------------------------------------------------------------------------

$projectRoot = Resolve-Source
Step "using source: $projectRoot"

# ---------------------------------------------------------------------------
# 2. Build
# ---------------------------------------------------------------------------

$goBin = $env:GO
if (-not $goBin) { $goBin = (Get-Command go -ErrorAction SilentlyContinue).Source }
if (-not $goBin) { Die "'go' is not installed. Install Go 1.22+ from https://go.dev/dl/ and re-run." }

$buildDir = Join-Path $projectRoot 'dist'
if (-not (Test-Path $buildDir)) { New-Item -ItemType Directory -Path $buildDir | Out-Null }
$binary  = Join-Path $buildDir 'mole.exe'

Step "building mole"
$env:GOOS = 'windows'; $env:GOARCH = 'amd64'
Push-Location $projectRoot
try {
    & $goBin build -trimpath -o $binary ./cmd/mole
} finally {
    Pop-Location
}
if ($LASTEXITCODE -ne 0) { Die "go build failed" }
if (-not (Test-Path $binary)) { Die "build did not produce $binary" }
Ok "built $binary"

# ---------------------------------------------------------------------------
# 3. Install
# ---------------------------------------------------------------------------

function Resolve-Dest {
    if ($env:INSTALL_DIR) { return $env:INSTALL_DIR }
    if ($Prefix)          { return (Join-Path (Join-Path $Prefix 'bin') 'mole.exe') }
    # Prefer per-user install when available; fall back to a system path
    # only when running elevated.
    $isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).
        IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if ($isAdmin) {
        return (Join-Path $env:ProgramFiles 'mole/mole.exe')
    }
    return (Join-Path (Join-Path $env:USERPROFILE '.local\bin') 'mole.exe')
}

$dest    = Resolve-Dest
$destDir = Split-Path -Parent $dest
Step "installing to $dest"
if (-not (Test-Path $destDir)) { New-Item -ItemType Directory -Path $destDir -Force | Out-Null }
Copy-Item -Path $binary -Destination $dest -Force
Ok "installed $dest"

# ---------------------------------------------------------------------------
# 4. Verify
# ---------------------------------------------------------------------------

if (-not $NoVerify) {
    Step "verifying"
    try {
        $ver = & $dest version 2>&1
        Ok "$ver"
    } catch {
        Warn "could not run '$dest version': $_"
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
    Write-Host ''
    Write-Host "NOTE: $destDir is not on your PATH."
    Write-Host "  Add it to your user PATH with:"
    Write-Host "    [Environment]::SetEnvironmentVariable('Path',"
    Write-Host "        [Environment]::GetEnvironmentVariable('Path','User') + ';$destDir', 'User')"
    Write-Host ''
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
Write-Host "  $(C '38;2;110;114;125' '─────────────────────────────────────────────────')"
Write-Host "  $(C '38;2;110;114;125' 'binary    ')$dest"
Write-Host "  $(C '38;2;110;114;125' 'configure ')$(C '38;2;173;186;199' 'mole init')   (interactive, run once per machine)"
Write-Host "  $(C '38;2;110;114;125' 'start     ')$(C '38;2;173;186;199' 'mole up')      (uses .\mole.yaml by default)"

# ---------------------------------------------------------------------------
# 7. Cleanup
# ---------------------------------------------------------------------------

# Explicit cleanup for the success path; the trap at top scope handles
# exception / Ctrl+C paths. Best-effort: failures here are suppressed so
# they don't mask any earlier error that put us on this code path.
if ($script:cloneTmpRoot -and (Test-Path -LiteralPath $script:cloneTmpRoot)) {
    Remove-Item -LiteralPath $script:cloneTmpRoot -Recurse -Force -ErrorAction SilentlyContinue
}
