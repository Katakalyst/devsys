#!/usr/bin/env sh
# devsys installer
#
# Usage:
#   curl -fsSL https://github.com/katakalyst/devsys/releases/latest/download/install.sh | sh
#
# What this script does:
#   1. Detects OS and CPU architecture.
#   2. Checks that Podman is installed (devsys's only host dependency).
#      If not, prints the Podman install URL and exits — this script does not
#      attempt to install Podman itself.
#   3. Compares the latest published devsys release against any devsys
#      already on PATH, and prints what it's about to do (install / upgrade /
#      already up to date) — exits early, without downloading anything, if
#      already current. Re-running this script is therefore just as valid a
#      way to update devsys as "devsys update" itself.
#   4. Downloads the devsys binary for your platform to ~/.local/bin/devsys.
#   5. Adds ~/.local/bin to PATH in your shell profile if it isn't there yet.
#   6. Tells you to run "devsys setup" next.
#
# Supported platforms: Linux x86_64 / arm64, macOS x86_64 / arm64 (Apple Silicon).
#
# On native Windows (PowerShell), use install.ps1 instead — devsys is a
# native binary there and does not need to run inside WSL2. If you prefer to
# run devsys inside a WSL2 shell anyway, this script works fine there too
# (WSL2 is just Linux from devsys's point of view); it's simply not required
# the way it is for Podman itself, which does need a WSL2-backed machine on
# Windows.

set -e

REPO="katakalyst/devsys"
INSTALL_DIR="${HOME}/.local/bin"
BINARY_NAME="devsys"

# --------------------------------------------------------------------------
# Minimal output helpers (no colour dependencies)
# --------------------------------------------------------------------------
info()  { printf '  %s\n' "$1"; }
ok()    { printf '  [OK]   %s\n' "$1"; }
fail()  { printf '  [FAIL] %s\n' "$1" >&2; }
abort() { printf '\nError: %s\n\n' "$1" >&2; exit 1; }

# --------------------------------------------------------------------------
# 1. Detect OS
# --------------------------------------------------------------------------
printf 'Detecting platform...\n'

OS_RAW="$(uname -s)"
case "${OS_RAW}" in
    Linux*)  OS_NAME="Linux" ;;
    Darwin*) OS_NAME="Darwin" ;;
    MINGW*|MSYS*|CYGWIN*)
        abort "This looks like a POSIX shell on native Windows (Git Bash or similar).
Use install.ps1 instead — devsys is a native Windows binary and doesn't need
a POSIX shell or WSL2 to run:
  irm https://github.com/${REPO}/releases/latest/download/install.ps1 | iex" ;;
    *)       abort "Unsupported OS: ${OS_RAW}.
devsys currently supports Linux, macOS, and (via this script) WSL2 on Windows.
For native Windows, use install.ps1 instead.
Please open an issue: https://github.com/${REPO}/issues" ;;
esac

ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
    x86_64)        ARCH_NAME="x86_64" ;;
    aarch64|arm64) ARCH_NAME="arm64" ;;
    *)             abort "Unsupported architecture: ${ARCH_RAW}. Supported: x86_64, arm64." ;;
esac

ok "Platform: ${OS_NAME}/${ARCH_NAME}"

# --------------------------------------------------------------------------
# 2. Check Podman
# --------------------------------------------------------------------------
printf '\nChecking prerequisites...\n'

if ! command -v podman >/dev/null 2>&1; then
    fail "Podman not found"
    printf >&2 '
devsys requires Podman. Please install it first, then re-run this script.

  Linux:  https://podman.io/docs/installation#installing-on-linux
          (includes the one-time rootless setup steps)
  macOS:  https://podman.io/docs/installation#macos
  WSL2:   install inside the WSL2 distro following the Linux instructions

'
    exit 1
fi
ok "Podman found: $(podman --version 2>/dev/null || printf 'unknown version')"

# Warn but do not abort if "podman info" fails — the binary install should
# still complete. The user may just need to finish rootless setup.
if ! podman info >/dev/null 2>&1; then
    fail "'podman info' failed — rootless setup may be incomplete."
    info "Linux:  https://github.com/containers/podman/blob/main/docs/tutorials/rootless_tutorial.md"
    info "macOS:  run 'podman machine init && podman machine start'"
    info "(continuing with install anyway)"
fi

# --------------------------------------------------------------------------
# 3. Compare against any existing install
# --------------------------------------------------------------------------
printf '\nChecking latest devsys release...\n'

# _fetch: print a URL's body to stdout, via curl or wget, whichever is
# available. Empty output (not a hard failure) if neither can reach it —
# version comparison degrades gracefully, install still proceeds.
_fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsS "$1" 2>/dev/null
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "$1" 2>/dev/null
    fi
}

LATEST_VERSION="$(_fetch "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/')"

EXISTING_VERSION=""
if command -v "${BINARY_NAME}" >/dev/null 2>&1; then
    EXISTING_VERSION="$("${BINARY_NAME}" --version 2>/dev/null | awk '{print $NF}')"
fi

if [ -z "${LATEST_VERSION}" ]; then
    fail "Cannot determine latest devsys version — continuing with install anyway."
elif [ -n "${EXISTING_VERSION}" ] && [ "${EXISTING_VERSION}" = "${LATEST_VERSION}" ]; then
    ok "devsys v${EXISTING_VERSION} is already up to date."
    printf '\nNext: run "devsys setup" if you have not already.\n\n'
    exit 0
elif [ -n "${EXISTING_VERSION}" ]; then
    info "Upgrading devsys v${EXISTING_VERSION} -> v${LATEST_VERSION}"
else
    info "Installing devsys v${LATEST_VERSION}"
fi

# --------------------------------------------------------------------------
# 4. Download the binary
# --------------------------------------------------------------------------
RELEASE_URL="https://github.com/${REPO}/releases/latest/download/${BINARY_NAME}_${OS_NAME}_${ARCH_NAME}"

printf '\nDownloading devsys...\n'
info "Source: ${RELEASE_URL}"

mkdir -p "${INSTALL_DIR}"
TMP_FILE="$(mktemp)"
# Ensure the temp file is removed on exit, even on error.
trap 'rm -f "${TMP_FILE}"' EXIT INT TERM

if command -v curl >/dev/null 2>&1; then
    if ! curl --fail --silent --show-error --location \
              --output "${TMP_FILE}" "${RELEASE_URL}"; then
        abort "Download failed.
Check your internet connection or visit:
  https://github.com/${REPO}/releases"
    fi
elif command -v wget >/dev/null 2>&1; then
    if ! wget --quiet --output-document="${TMP_FILE}" "${RELEASE_URL}"; then
        abort "Download failed.
Check your internet connection or visit:
  https://github.com/${REPO}/releases"
    fi
else
    abort "Neither curl nor wget is available. Please install one and try again."
fi

chmod +x "${TMP_FILE}"
mv "${TMP_FILE}" "${INSTALL_DIR}/${BINARY_NAME}"
ok "Installed: ${INSTALL_DIR}/${BINARY_NAME}"

# --------------------------------------------------------------------------
# 5. Ensure ~/.local/bin is on PATH
# --------------------------------------------------------------------------

# Helper: append the PATH export to a profile file if ~/.local/bin is not
# already referenced there.
_add_to_profile() {
    profile="$1"
    if grep -qF '.local/bin' "${profile}" 2>/dev/null; then
        return 0  # already present
    fi
    printf '\n# Added by devsys installer\nexport PATH="${HOME}/.local/bin:${PATH}"\n' \
        >> "${profile}"
    info "Added ~/.local/bin to PATH in ${profile}"
}

# Check whether the install dir is reachable in the current PATH.
_on_path=0
_IFS_OLD="${IFS}"; IFS=:
for _dir in ${PATH}; do
    [ "${_dir}" = "${INSTALL_DIR}" ] && _on_path=1 && break
done
IFS="${_IFS_OLD}"

if [ "${_on_path}" -eq 0 ]; then
    printf '\nAdding ~/.local/bin to your shell PATH...\n'
    _patched=0
    for _profile in "${HOME}/.bashrc" "${HOME}/.zshrc" "${HOME}/.profile"; do
        if [ -f "${_profile}" ]; then
            _add_to_profile "${_profile}"
            _patched=1
        fi
    done
    # No profile file found — create .profile so something picks it up.
    if [ "${_patched}" -eq 0 ]; then
        _add_to_profile "${HOME}/.profile"
    fi
    printf '\n  Reload your shell or run:\n'
    printf '    export PATH="${HOME}/.local/bin:${PATH}"\n'
fi

# --------------------------------------------------------------------------
# 6. Done — tell the user what to do next
# --------------------------------------------------------------------------
printf '\n'
printf '  devsys installed successfully.\n'
printf '\n'
printf 'Next: run "devsys setup" to:\n'
printf '  - Pull the devsys-base container image\n'
printf '  - Store your GitLab Personal Access Token as a Podman secret\n'
printf '  - Initialise the shared Claude / Codex credential volumes\n'
printf '\n'
printf '  devsys setup\n'
printf '\n'
