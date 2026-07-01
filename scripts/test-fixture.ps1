# scripts/test-fixture.ps1 — set up a scratch cwd for exercising
# scripts/install.ps1 (Windows).
#
# What this script does:
#   setup   create $FIXTURE_DIR (kept across runs so you can re-run
#           install.ps1 against a mole.yaml that was written by a
#           previous `mole init`), wipe and recreate $INSTALL_PREFIX
#           (so a stale mole.exe from a previous run cannot mask a
#           broken install), and drop a .fixture-info marker with
#           the exact commands to drive the test.
#   clean   remove both $FIXTURE_DIR and $INSTALL_PREFIX.
#
# Usage (PowerShell):
#   .\scripts\test-fixture.ps1                # default: setup
#   .\scripts\test-fixture.ps1 setup
#   .\scripts\test-fixture.ps1 clean
#   .\scripts\test-fixture.ps1 -h
#
# Environment variables:
#   FIXTURE_DIR    scratch CWD for the install (default: C:\mole-cfg-test)
#   INSTALL_PREFIX install destination prefix (default: C:\mole-prefix)
#   REPO_ROOT      path to the mole repo (default: parent of this script)

[CmdletBinding()]
param(
    [string]$Command = 'setup'
)

$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

# Resolve this script's directory. $MyInvocation.MyCommand.Path is reliable
# for `-File` and dot-sourced invocations. For `irm ... | iex` there is no
# real file path, but a fixture is never driven that way, so we don't
# bother with a stdin-to-tmp fallback (unlike install.ps1).
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRootDefault = (Resolve-Path (Join-Path $scriptDir '..')).Path

$fixtureDir   = if ($env:FIXTURE_DIR)   { $env:FIXTURE_DIR }   else { 'C:\mole-cfg-test' }
$installPrefix = if ($env:INSTALL_PREFIX) { $env:INSTALL_PREFIX } else { 'C:\mole-prefix' }
$repoRoot     = if ($env:REPO_ROOT)     { $env:REPO_ROOT }     else { $repoRootDefault }

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

function Die($msg) {
    Write-Error "error: $msg"
    exit 1
}

function Test-MoleRepo {
    param([string]$dir)
    if (-not (Test-Path (Join-Path $dir 'go.mod')))   { return $false }
    if (-not (Test-Path (Join-Path $dir 'cmd/mole'))) { return $false }
    return $true
}

function Show-Usage {
    @'
scripts/test-fixture.ps1 — set up a scratch cwd for exercising scripts/install.ps1.

Usage:
  .\scripts\test-fixture.ps1                # default: setup
  .\scripts\test-fixture.ps1 setup
  .\scripts\test-fixture.ps1 clean
  .\scripts\test-fixture.ps1 -h

Environment variables:
  FIXTURE_DIR     scratch CWD for the install (default: C:\mole-cfg-test)
  INSTALL_PREFIX  install destination prefix (default: C:\mole-prefix)
  REPO_ROOT       path to the mole repo (default: parent of this script)
'@
}

# ---------------------------------------------------------------------------
# Dispatch
# ---------------------------------------------------------------------------

if ($Command -eq '-h' -or $Command -eq '--help') {
    Show-Usage
    exit 0
}

# Validate the repo root early so any subsequent error is unambiguous.
if (-not (Test-MoleRepo $repoRoot)) {
    Die "REPO_ROOT does not look like a mole repo: $repoRoot (set REPO_ROOT to override)"
}

switch ($Command) {
    'setup' {
        # mkdir -p is safe to re-run; we just need the dir to exist and
        # to be empty enough to be a credible scratch CWD. Leave any
        # prior artifacts (e.g. mole.yaml from a previous --init run) in
        # place so the test can also exercise the "config already exists"
        # branch of the installer — that is the whole point of a fixture
        # you can poke at. Just make sure the dir exists.
        if (-not (Test-Path $fixtureDir)) {
            New-Item -ItemType Directory -Path $fixtureDir -Force | Out-Null
        }

        # The install prefix must be a clean directory: a stale mole
        # binary from a previous run would let `mole version` succeed
        # and mask a broken install. Wipe it.
        if (Test-Path $installPrefix) {
            Remove-Item -Recurse -Force $installPrefix
        }
        New-Item -ItemType Directory -Path $installPrefix -Force | Out-Null

        # A small marker so anyone landing in the dir later can see what
        # it is. The marker carries the exact commands to drive the test
        # so a re-run of the fixture (or a fresh shell) is self-describing.
        $marker = @"
# Test fixture created by scripts/test-fixture.ps1
# (this file is informational; safe to delete)

repo root:      $repoRoot
fixture dir:    $fixtureDir
install prefix: $installPrefix

# Exercise the install (no -Init):
cd $fixtureDir
& $repoRoot\scripts\install.ps1 -Prefix $installPrefix

# Or with -Init (interactive; needs a console):
cd $fixtureDir
& $repoRoot\scripts\install.ps1 -Prefix $installPrefix -Init

# Or -Init non-interactively (stdin redirected => -no-prompt + env vars):
cd $fixtureDir
`$env:MOLE_REMOTE = 'dev@host'
`$env:MOLE_PORTS  = '3000,5173'
& $repoRoot\scripts\install.ps1 -Prefix $installPrefix -Init < `$null

# Tear down:
& $repoRoot\scripts\test-fixture.ps1 clean
"@
        $markerPath = Join-Path $fixtureDir '.fixture-info'
        Set-Content -Path $markerPath -Value $marker -Encoding UTF8

        Write-Host '== test fixture ready'
        Write-Host "   cwd:         $fixtureDir"
        Write-Host "   install to:  $installPrefix"
        Write-Host "   repo root:   $repoRoot"
        Write-Host "   marker file: $markerPath"
        Write-Host ''
        Write-Host "See $markerPath for the exact commands."
    }

    'clean' {
        # Remove both the scratch CWD and the install destination. Safe
        # to run repeatedly.
        if (Test-Path $fixtureDir)     { Remove-Item -Recurse -Force $fixtureDir }
        if (Test-Path $installPrefix)  { Remove-Item -Recurse -Force $installPrefix }
        Write-Host "== removed $fixtureDir and $installPrefix"
    }

    default {
        Write-Error "error: unknown command: $Command (try -h)"
        exit 2
    }
}
