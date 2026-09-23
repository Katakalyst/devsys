#!/usr/bin/env sh
# devsys installer
#
# Usage:
#   curl -fsSL https://github.com/katakalyst/devsys/releases/latest/download/install.sh | sh
#
# What this script does:
#   0. Checks that the tools it needs are actually present — sed/grep/awk
#      (required), and curl or wget (required; at least one). Aborts with a
#      clear message before doing any other work if something's missing.
#   1. Detects OS and CPU architecture.
#   2. Checks that Podman is installed (devsys's only host dependency).
#      If not, prints the Podman install URL and exits — this script does not
#      attempt to install Podman itself.
#   3. Compares the latest published devsys release against any devsys
#      already on PATH, and prints what it's about to do (install / upgrade /
#      already up to date). Re-running this script is therefore just as valid
#      a way to update devsys as "devsys update" itself.
#   4. Downloads the devsys binary for your platform to ~/.local/bin/devsys,
#      unless already up to date.
#   5. On macOS: adds ~/.local/bin to PATH in your shell profile if it isn't
#      there yet. On Linux this is unnecessary — ~/.local/bin is already in
#      PATH on any modern systemd-based distro.
#   6. Pulls the newest devsys-base image directly (no devsys subcommand
#      involved) — looks up the current tags on GHCR, picks the highest
#      semver one, and `podman pull`s it. Requires curl specifically (see
#      step 0) — skipped with a message on wget-only systems, or if Podman
#      isn't fully working yet; neither case is fatal, since `devsys init`
#      pulls whatever it needs anyway.
#   7. Tells you to run "devsys auth gitlab/claude/codex" next — those are
#      interactive and deliberately not run by this script: it can be
#      invoked as "curl | sh", which leaves stdin bound to the script body,
#      not a terminal, so anything needing a prompt has to happen
#      afterward, from a real shell.
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
BASE_REPO="katakalyst/devsys-base"
GHCR_HOST="ghcr.io"
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
# 0. Check required tools before doing anything else
# --------------------------------------------------------------------------
printf 'Checking required tools...\n'

_missing=""
for _tool in sed grep awk; do
    command -v "${_tool}" >/dev/null 2>&1 || _missing="${_missing} ${_tool}"
done
if [ -n "${_missing}" ]; then
    abort "Missing required tool(s):${_missing}
This script needs sed, grep, and awk, which should already be present on
any Linux, macOS, or WSL2 system. Please install the missing tool(s) and
re-run."
fi

HAVE_CURL=0
HAVE_WGET=0
command -v curl >/dev/null 2>&1 && HAVE_CURL=1
command -v wget >/dev/null 2>&1 && HAVE_WGET=1
if [ "${HAVE_CURL}" -eq 0 ] && [ "${HAVE_WGET}" -eq 0 ]; then
    abort "Neither curl nor wget is available. Please install one and try again."
fi
if [ "${HAVE_CURL}" -eq 0 ]; then
    info "curl not found (wget is present) — devsys is downloadable, but the"
    info "automatic devsys-base pull in step 6 needs curl specifically and"
    info "will be skipped. 'devsys init' pulls it anyway when needed."
fi
ok "Required tools present."

# --------------------------------------------------------------------------
# 1. Detect OS
# --------------------------------------------------------------------------
printf '\nDetecting platform...\n'

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
# still complete. The user may just need to finish rootless setup. Recorded
# so step 6 knows not to attempt the devsys-base pull (it would just fail
# the same way).
PODMAN_INFO_OK=1
if ! podman info >/dev/null 2>&1; then
    PODMAN_INFO_OK=0
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
    if [ "${HAVE_CURL}" -eq 1 ]; then
        curl -fsS "$1" 2>/dev/null
    else
        wget -qO- "$1" 2>/dev/null
    fi
}

LATEST_VERSION="$(_fetch "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/')"

EXISTING_VERSION=""
if command -v "${BINARY_NAME}" >/dev/null 2>&1; then
    EXISTING_VERSION="$("${BINARY_NAME}" --version 2>/dev/null | awk '{print $NF}')"
fi

SKIP_DOWNLOAD=0
if [ -z "${LATEST_VERSION}" ]; then
    fail "Cannot determine latest devsys version — continuing with install anyway."
elif [ -n "${EXISTING_VERSION}" ] && [ "${EXISTING_VERSION}" = "${LATEST_VERSION}" ]; then
    ok "devsys v${EXISTING_VERSION} is already up to date."
    SKIP_DOWNLOAD=1
elif [ -n "${EXISTING_VERSION}" ]; then
    info "Upgrading devsys v${EXISTING_VERSION} -> v${LATEST_VERSION}"
else
    info "Installing devsys v${LATEST_VERSION}"
fi

# --------------------------------------------------------------------------
# 4. Download the binary (skipped if already up to date)
# --------------------------------------------------------------------------
if [ "${SKIP_DOWNLOAD}" -eq 0 ]; then
    RELEASE_URL="https://github.com/${REPO}/releases/latest/download/${BINARY_NAME}_${OS_NAME}_${ARCH_NAME}"

    printf '\nDownloading devsys...\n'
    info "Source: ${RELEASE_URL}"

    mkdir -p "${INSTALL_DIR}"
    TMP_FILE="$(mktemp)"
    # Ensure the temp file is removed on exit, even on error.
    trap 'rm -f "${TMP_FILE}"' EXIT INT TERM

    if [ "${HAVE_CURL}" -eq 1 ]; then
        if ! curl --fail --silent --show-error --location \
                  --output "${TMP_FILE}" "${RELEASE_URL}"; then
            abort "Download failed.
Check your internet connection or visit:
  https://github.com/${REPO}/releases"
        fi
    else
        if ! wget --quiet --output-document="${TMP_FILE}" "${RELEASE_URL}"; then
            abort "Download failed.
Check your internet connection or visit:
  https://github.com/${REPO}/releases"
        fi
    fi

    chmod +x "${TMP_FILE}"
    mv "${TMP_FILE}" "${INSTALL_DIR}/${BINARY_NAME}"
    ok "Installed: ${INSTALL_DIR}/${BINARY_NAME}"
fi

# --------------------------------------------------------------------------
# 5. Ensure ~/.local/bin is on PATH
# --------------------------------------------------------------------------
# On Linux, ~/.local/bin is already in PATH on any modern systemd-based
# distro — no profile modification needed. On macOS it isn't added by
# default, so append it to the shell profile if missing.

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

if [ "${OS_NAME}" = "Darwin" ]; then
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
        for _profile in "${HOME}/.zshrc" "${HOME}/.profile"; do
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
else
    # Linux: ~/.local/bin is on PATH by default. Print a note if it somehow
    # isn't so the user knows what to do, but don't touch their profile files.
    _on_path=0
    _IFS_OLD="${IFS}"; IFS=:
    for _dir in ${PATH}; do
        [ "${_dir}" = "${INSTALL_DIR}" ] && _on_path=1 && break
    done
    IFS="${_IFS_OLD}"

    if [ "${_on_path}" -eq 0 ]; then
        printf '\nNote: ~/.local/bin is not in your PATH.\n'
        info "Add it yourself if needed: export PATH=\"\${HOME}/.local/bin:\${PATH}\""
    fi
fi

# --------------------------------------------------------------------------
# 6. Pull the newest devsys-base image
# --------------------------------------------------------------------------
# Looks up the newest published tag directly against GHCR's OCI Distribution
# API and pulls it — no devsys subcommand involved, since this can run
# entirely before devsys itself has any credentials configured. Requires
# curl specifically (see step 0's HAVE_CURL check) for the header inspection
# the registry's anonymous-token challenge needs; skipped on wget-only
# systems. Never fatal — a failure here just means the pull happens later,
# inside `devsys init`, which needs the image anyway.
if [ "${HAVE_CURL}" -eq 0 ]; then
    printf '\nSkipping devsys-base pull — curl is required for this step (see above).\n'
elif [ "${PODMAN_INFO_OK}" -eq 0 ]; then
    printf '\nSkipping devsys-base pull — Podman is not fully working yet (see above).\n'
else
    printf '\nLooking up the newest devsys-base version...\n'

    TAGS_URL="https://${GHCR_HOST}/v2/${BASE_REPO}/tags/list"

    # is_semver TAG: true if TAG looks like a plain X.Y.Z version (an
    # optional "v" prefix is tolerated for comparison but never required —
    # devsys-base tags are published as "1.0.0", never "v1.0.0" or
    # "latest"; Release Process.md is explicit that "latest" is never
    # published). Anything else is ignored, matching the Go implementation's
    # highestSemver (internal/registry/registry.go).
    is_semver() {
        printf '%s' "$1" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+$'
    }

    # version_gt A B: true (exit 0) if A > B. Both must already satisfy
    # is_semver. Deliberately not "sort -V" — that's a GNU coreutils
    # extension, not available in macOS's BSD sort.
    version_gt() {
        _a="$(printf '%s' "$1" | sed 's/^v//')"
        _b="$(printf '%s' "$2" | sed 's/^v//')"
        _a1="${_a%%.*}"; _arest="${_a#*.}"; _a2="${_arest%%.*}"; _a3="${_arest#*.}"
        _b1="${_b%%.*}"; _brest="${_b#*.}"; _b2="${_brest%%.*}"; _b3="${_brest#*.}"
        if [ "${_a1}" -gt "${_b1}" ]; then return 0; fi
        if [ "${_a1}" -lt "${_b1}" ]; then return 1; fi
        if [ "${_a2}" -gt "${_b2}" ]; then return 0; fi
        if [ "${_a2}" -lt "${_b2}" ]; then return 1; fi
        [ "${_a3}" -gt "${_b3}" ]
    }

    # Registry token-auth challenge (RFC-shaped WWW-Authenticate: Bearer),
    # required by GHCR even for anonymous/public reads — mirrors
    # fetchAnonymousToken in internal/registry/registry.go, but only the
    # curl path (see step 0/HAVE_CURL above).
    _headers="$(curl -sS -D - -o /dev/null "${TAGS_URL}" 2>/dev/null || true)"
    _status="$(printf '%s' "${_headers}" | head -1 | awk '{print $2}')"

    TAGS_JSON=""
    if [ "${_status}" = "401" ]; then
        _www_auth="$(printf '%s' "${_headers}" | grep -i '^www-authenticate:' \
            | sed -E 's/^[Ww][Ww][Ww]-[Aa]uthenticate: *//' | tr -d '\r')"
        _realm="$(printf '%s' "${_www_auth}" | sed -E 's/.*realm="([^"]*)".*/\1/')"
        _service="$(printf '%s' "${_www_auth}" | sed -E 's/.*service="([^"]*)".*/\1/')"
        _scope="$(printf '%s' "${_www_auth}" | sed -E 's/.*scope="([^"]*)".*/\1/')"
        if [ -n "${_realm}" ]; then
            _token="$(curl -fsS "${_realm}?service=${_service}&scope=${_scope}" 2>/dev/null \
                | sed -E 's/.*"token":"([^"]+)".*/\1/')"
            if [ -n "${_token}" ]; then
                TAGS_JSON="$(curl -fsS -H "Authorization: Bearer ${_token}" "${TAGS_URL}" 2>/dev/null || true)"
            fi
        fi
    elif [ "${_status}" = "200" ]; then
        TAGS_JSON="$(curl -fsS "${TAGS_URL}" 2>/dev/null || true)"
    fi

    if [ -z "${TAGS_JSON}" ]; then
        fail "Cannot reach ${GHCR_HOST} to look up devsys-base — skipping pull."
        info "'devsys init' will pull it when needed."
    else
        TAGS_RAW="$(printf '%s' "${TAGS_JSON}" | sed -E 's/.*"tags":\[([^]]*)\].*/\1/')"

        BEST=""
        for _t in $(printf '%s' "${TAGS_RAW}" | tr ',' '\n' | sed -E 's/^"|"$//g'); do
            is_semver "${_t}" || continue
            if [ -z "${BEST}" ] || version_gt "${_t}" "${BEST}"; then
                BEST="${_t}"
            fi
        done

        if [ -z "${BEST}" ]; then
            fail "No semver-tagged devsys-base version found — skipping pull."
            info "'devsys init' will pull it when needed."
        else
            BASE_REF="${GHCR_HOST}/${BASE_REPO}:${BEST}"
            printf 'Pulling %s ...\n' "${BASE_REF}"
            if podman pull "${BASE_REF}"; then
                ok "Pulled ${BASE_REF}."
            else
                fail "Pull failed — 'devsys init' will retry when needed."
            fi
        fi
    fi
fi

# --------------------------------------------------------------------------
# 7. Done — tell the user what to do next
# --------------------------------------------------------------------------
printf '\n'
printf '  devsys is ready.\n'
printf '\n'
printf 'Next, set up credentials (interactive — run these yourself, not from a pipe):\n'
printf '  devsys auth gitlab    # bootstrap GitLab PAT devsys uses to create projects\n'
printf '  devsys auth claude    # seed or log in the shared Claude credential\n'
printf '  devsys auth codex     # seed or log in the shared Codex credential\n'
printf '\n'
printf 'Then:\n'
printf '  devsys init <path> <name>\n'
printf '\n'
