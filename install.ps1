#!/usr/bin/env pwsh
# devsys installer — native Windows.
#
# Usage:
#   irm https://github.com/katakalyst/devsys/releases/latest/download/install.ps1 | iex
#
# What this script does:
#   1. Detects CPU architecture.
#   2. Checks that Podman is reachable (devsys's only host dependency). If not,
#      prints the Podman install URL and exits — this script does not attempt
#      to install Podman itself. Podman on Windows needs a WSL2-backed
#      machine, but devsys itself does not run inside WSL2 — it is a native
#      Windows binary, same as this installer.
#   3. Compares the latest published devsys release against any devsys.exe
#      already on PATH, and prints what it's about to do (install / upgrade /
#      already up to date).
#   4. Downloads the devsys binary for your architecture to
#      $HOME\.local\bin\devsys.exe.
#   5. Adds $HOME\.local\bin to the user PATH if it isn't there yet.
#   6. Tells you to run "devsys setup" next.
#
# Supported: Windows x86_64 / arm64.
# If you'd rather run devsys inside WSL2 alongside Podman, use install.sh
# from inside your WSL2 shell instead — that also works, it's just not
# required the way it is for Podman itself.

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$Repo        = "katakalyst/devsys"
$InstallDir  = Join-Path $HOME ".local\bin"
$BinaryName  = "devsys.exe"
$InstallPath = Join-Path $InstallDir $BinaryName

function Write-Info { param([string]$Message) Write-Host "  $Message" }
function Write-Ok   { param([string]$Message) Write-Host "  [OK]   $Message" }
function Write-Fail { param([string]$Message) Write-Host "  [FAIL] $Message" -ForegroundColor Red }
function Abort {
    param([string]$Message)
    Write-Host ""
    Write-Host "Error: $Message" -ForegroundColor Red
    Write-Host ""
    exit 1
}

# --------------------------------------------------------------------------
# 1. Detect architecture
# --------------------------------------------------------------------------
Write-Host "Detecting platform..."

$archRaw = $env:PROCESSOR_ARCHITECTURE
switch ($archRaw) {
    "AMD64" { $archName = "x86_64" }
    "ARM64" { $archName = "arm64" }
    default { Abort "Unsupported architecture: $archRaw. Supported: x86_64 (AMD64), arm64." }
}
Write-Ok "Platform: Windows/$archName"

# --------------------------------------------------------------------------
# 2. Check Podman
# --------------------------------------------------------------------------
Write-Host ""
Write-Host "Checking prerequisites..."

$podmanCmd = Get-Command podman -ErrorAction SilentlyContinue
if (-not $podmanCmd) {
    Write-Fail "Podman not found"
    Write-Host @"

devsys requires Podman. Please install it first, then re-run this script.

  Podman Desktop (recommended on Windows): https://podman.io/docs/installation#windows
  This sets up the WSL2-backed Podman machine devsys talks to. devsys itself
  does not need to run inside that WSL2 machine.

"@
    exit 1
}
$podmanVersion = (& podman --version) 2>$null
if (-not $podmanVersion) { $podmanVersion = "unknown version" }
Write-Ok "Podman found: $podmanVersion"

# Warn but do not abort if "podman info" fails — the binary install should
# still complete. The user may just need to start their Podman machine.
& podman info *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Fail "'podman info' failed — the Podman machine may not be running."
    Write-Info "Run: podman machine start"
    Write-Info "(continuing with install anyway)"
}

# --------------------------------------------------------------------------
# 3. Compare against any existing install
# --------------------------------------------------------------------------
Write-Host ""
Write-Host "Checking latest devsys release..."

$latestVersion = $null
try {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "Accept" = "application/vnd.github+json" }
    $latestVersion = $release.tag_name -replace '^v', ''
} catch {
    Write-Fail "Cannot reach GitHub releases API: $($_.Exception.Message)"
    Write-Info "Continuing with install anyway — version comparison skipped."
}

$existingVersion = $null
$existingCmd = Get-Command devsys -ErrorAction SilentlyContinue
if ($existingCmd) {
    try {
        $existingRaw = (& devsys --version) 2>$null
        if ($existingRaw -match '(\d+\.\d+\.\d+)') { $existingVersion = $Matches[1] }
    } catch {
        # devsys on PATH but --version failed (very old build) — treat as unknown.
    }
}

if ($latestVersion) {
    if (-not $existingVersion) {
        Write-Info "Installing devsys v$latestVersion"
    } elseif ($existingVersion -eq $latestVersion) {
        Write-Ok "devsys v$existingVersion is already up to date."
        Write-Host ""
        Write-Host "Next: run `"devsys setup`" if you haven't already."
        exit 0
    } else {
        Write-Info "Upgrading devsys v$existingVersion -> v$latestVersion"
    }
} else {
    Write-Info "Installing devsys (version comparison unavailable)"
}

# --------------------------------------------------------------------------
# 4. Download the binary
# --------------------------------------------------------------------------
$releaseUrl = "https://github.com/$Repo/releases/latest/download/devsys_Windows_${archName}.exe"

Write-Host ""
Write-Host "Downloading devsys..."
Write-Info "Source: $releaseUrl"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$tmpFile = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())

try {
    Invoke-WebRequest -Uri $releaseUrl -OutFile $tmpFile -UseBasicParsing
} catch {
    Remove-Item -Path $tmpFile -ErrorAction SilentlyContinue
    Abort "Download failed: $($_.Exception.Message)`nCheck your internet connection or visit:`n  https://github.com/$Repo/releases"
}

Move-Item -Path $tmpFile -Destination $InstallPath -Force
Write-Ok "Installed: $InstallPath"

# --------------------------------------------------------------------------
# 5. Ensure $HOME\.local\bin is on PATH
# --------------------------------------------------------------------------
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$pathEntries = @()
if ($userPath) { $pathEntries = $userPath -split ";" }

if ($pathEntries -notcontains $InstallDir) {
    Write-Host ""
    Write-Host "Adding $InstallDir to your user PATH..."
    $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    # Make it available in this session too, without requiring a new shell.
    $env:Path = "$env:Path;$InstallDir"
    Write-Info "Added. Open a new terminal (or this one already has it) for it to take effect everywhere."
}

# --------------------------------------------------------------------------
# 6. Done — tell the user what to do next
# --------------------------------------------------------------------------
Write-Host ""
Write-Host "  devsys installed successfully."
Write-Host ""
Write-Host "Next: run `"devsys setup`" to:"
Write-Host "  - Pull the devsys-base container image"
Write-Host "  - Store your GitLab Personal Access Token as a Podman secret"
Write-Host "  - Initialise the shared Claude / Codex credential volumes"
Write-Host ""
Write-Host "  devsys setup"
Write-Host ""
