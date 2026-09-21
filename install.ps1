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
#      (No separate "required tools" check like install.sh's step 0: unlike
#      bash, which needs external curl/wget/sed/grep binaries, everything
#      this script needs — Invoke-RestMethod, Invoke-WebRequest, JSON
#      parsing — is built into PowerShell itself. Podman, checked here, is
#      the only real external dependency.)
#   3. Compares the latest published devsys release against any devsys.exe
#      already on PATH, and prints what it's about to do (install / upgrade /
#      already up to date).
#   4. Downloads the devsys binary for your architecture to
#      %LOCALAPPDATA%\Programs\devsys\devsys.exe, unless already up to date.
#   5. Adds %LOCALAPPDATA%\Programs\devsys to the user PATH if it isn't there yet.
#   6. Pulls the newest devsys-base image directly (no devsys subcommand
#      involved) — looks up the current tags on GHCR, picks the highest
#      semver one, and `podman pull`s it. Skipped with a message if Podman
#      isn't fully working yet; never fatal, since `devsys init` pulls
#      whatever it needs anyway.
#   7. Tells you to run "devsys auth gitlab/claude/codex" next — those are
#      interactive and deliberately not run by this script.
#
# Supported: Windows x86_64 / arm64.
# If you'd rather run devsys inside WSL2 alongside Podman, use install.sh
# from inside your WSL2 shell instead — that also works, it's just not
# required the way it is for Podman itself.

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$Repo        = "katakalyst/devsys"
$BaseRepo    = "katakalyst/devsys-base"
$GhcrHost    = "ghcr.io"
$InstallDir  = Join-Path $env:LOCALAPPDATA "Programs\devsys"
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
# Recorded so step 6 knows not to attempt the devsys-base pull (it would
# just fail the same way).
$podmanInfoOk = $true
& podman info *> $null
if ($LASTEXITCODE -ne 0) {
    $podmanInfoOk = $false
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

$skipDownload = $false
if ($latestVersion) {
    if (-not $existingVersion) {
        Write-Info "Installing devsys v$latestVersion"
    } elseif ($existingVersion -eq $latestVersion) {
        Write-Ok "devsys v$existingVersion is already up to date."
        $skipDownload = $true
    } else {
        Write-Info "Upgrading devsys v$existingVersion -> v$latestVersion"
    }
} else {
    Write-Info "Installing devsys (version comparison unavailable)"
}

# --------------------------------------------------------------------------
# 4. Download the binary (skipped if already up to date)
# --------------------------------------------------------------------------
if (-not $skipDownload) {
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
}

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
# 6. Pull the newest devsys-base image
# --------------------------------------------------------------------------
# Looks up the newest published tag directly against GHCR's OCI Distribution
# API and pulls it — no devsys subcommand involved, since this can run
# entirely before devsys itself has any credentials configured. Never
# fatal — a failure here just means the pull happens later, inside
# `devsys init`, which needs the image anyway.

# Get-GhcrToken: implements the registry token-auth challenge (RFC-shaped
# WWW-Authenticate: Bearer), required by GHCR even for anonymous/public
# reads. Mirrors fetchAnonymousToken in internal/registry/registry.go.
function Get-GhcrToken {
    param([System.Net.Http.Headers.AuthenticationHeaderValue]$WwwAuthenticate)

    if ($WwwAuthenticate.Scheme -ne "Bearer") {
        throw "Unsupported WWW-Authenticate scheme: $($WwwAuthenticate.Scheme)"
    }
    $params = @{}
    foreach ($pair in ($WwwAuthenticate.Parameter -split ',')) {
        if ($pair -match '^\s*(\w+)="([^"]*)"\s*$') {
            $params[$Matches[1]] = $Matches[2]
        }
    }
    if (-not $params.realm) {
        throw "No realm in WWW-Authenticate header"
    }
    $tokenUrl = "$($params.realm)?service=$($params.service)&scope=$($params.scope)"
    $tokenResp = Invoke-RestMethod -Uri $tokenUrl
    if ($tokenResp.token) { return $tokenResp.token }
    if ($tokenResp.access_token) { return $tokenResp.access_token }
    throw "Token endpoint response had no token/access_token field"
}

if (-not $podmanInfoOk) {
    Write-Host ""
    Write-Host "Skipping devsys-base pull — Podman is not fully working yet (see above)."
} else {
    Write-Host ""
    Write-Host "Looking up the newest devsys-base version..."

    $tagsUrl = "https://$GhcrHost/v2/$BaseRepo/tags/list"
    $baseRef = $null

    try {
        try {
            $tagsResp = Invoke-RestMethod -Uri $tagsUrl -ErrorAction Stop
        } catch {
            $webResp = $_.Exception.Response
            if ($webResp -and [int]$webResp.StatusCode -eq 401) {
                $wwwAuth = $webResp.Headers.WwwAuthenticate | Select-Object -First 1
                if (-not $wwwAuth) { throw "401 response had no WWW-Authenticate header" }
                $token = Get-GhcrToken -WwwAuthenticate $wwwAuth
                $tagsResp = Invoke-RestMethod -Uri $tagsUrl -Headers @{ Authorization = "Bearer $token" } -ErrorAction Stop
            } else {
                throw
            }
        }

        # Devsys-base tags are plain semver ("1.0.0"), never "latest" or a
        # "v"-prefixed tag (Release Process.md) — filter to that shape, then
        # let [version] do numeric (not lexical) comparison: "1.10.0" must
        # sort above "1.9.5".
        $semverTags = $tagsResp.tags | Where-Object { $_ -match '^v?\d+\.\d+\.\d+$' }
        if (-not $semverTags) {
            throw "No semver-formatted tags found among $($tagsResp.tags -join ', ')"
        }
        $best = $semverTags | Sort-Object { [version]($_ -replace '^v', '') } | Select-Object -Last 1
        $baseRef = "${GhcrHost}/${BaseRepo}:$best"
    } catch {
        Write-Fail "Cannot look up newest devsys-base version: $($_.Exception.Message)"
        Write-Info "'devsys init' will pull it when needed."
    }

    if ($baseRef) {
        Write-Host "Pulling $baseRef ..."
        & podman pull $baseRef
        if ($LASTEXITCODE -eq 0) {
            Write-Ok "Pulled $baseRef."
        } else {
            Write-Fail "Pull failed — 'devsys init' will retry when needed."
        }
    }
}

# --------------------------------------------------------------------------
# 7. Done — tell the user what to do next
# --------------------------------------------------------------------------
Write-Host ""
Write-Host "  devsys is ready."
Write-Host ""
Write-Host "Next, set up credentials (interactive — run these yourself):"
Write-Host "  devsys auth gitlab    # bootstrap GitLab PAT devsys uses to create projects"
Write-Host "  devsys auth claude    # seed or log in the shared Claude credential"
Write-Host "  devsys auth codex     # seed or log in the shared Codex credential"
Write-Host ""
Write-Host "Then:"
Write-Host "  devsys init <path>"
Write-Host ""
