#!/usr/bin/env bash
#
# scripts/install/nss-tools.sh
# ============================
#
# Capability: install.nssTools -- make sure `certutil` is on this machine, so
# the local CA can be trusted by BROWSERS and not only by the system.
#
# WHY THIS IS A STEP AND NOT A README LINE
#
# The install ends by handing the operator a link to open. On Linux, Firefox and
# Chrome do not read the system trust store -- they read their own NSS database,
# and mkcert can only write it through `certutil`, which ships in a package
# nobody has by default (libnss3-tools / nss-tools / nss). Without it
# `mkcert -install` prints a warning and exits 0: the certificate is issued, the
# cluster comes up, every check passes, and the front door is untrusted in the
# one application the last step of the install tells them to use.
#
# memql#3560 made that a refusal instead of a silent success, which was right
# and left the operator holding an apt command. This installs it.
#
# WHAT IT WILL NOT DO. It installs exactly one package, through the package
# manager the machine already uses, and it NEVER removes it again -- which is
# why the install graph marks this step `retained`. certutil is a general
# system tool; other software uses it, and an application uninstaller that takes
# distribution packages away with it is how installers earn their reputation.
# Docker is not installed here either, for a larger version of the same reason:
# see docs/public/operate/install-prerequisites.md.
#
# macOS needs no elevation for this -- Homebrew installs into a prefix the
# operator owns -- and needs it less, because the system keychain covers Safari
# and Chrome there; only Firefox reads NSS. It is still installed when brew is
# present, so a Mac with Firefox is not a second-class install.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/nss-tools.sh --confirm=install-nss-tools
#   scripts/install/nss-tools.sh --certutil=/usr/bin/certutil    # probe only
#   scripts/install/nss-tools.sh --print-spec
#
# Idempotent: certutil already present is changed=false and installs nothing.
#
# Exit codes:
#   0 ok | 2 bad param | 3 refused (no --confirm, and something must be
#   installed) | 4 prerequisite missing (no package manager; no way to reach
#   root -- result.remedy carries the command that finishes it)
#   5 operation failed (the package manager ran and certutil is still absent)
#
# Refs: #3566 #3560 #3562 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/elevate.sh
source "${SCRIPT_DIR}/../lib/elevate.sh"

cap_init "install.nssTools" "Install the NSS tools so browsers can trust the local certificate authority."
cap_spec_param "certutil" "path to the certutil binary (default: resolved from PATH)"
cap_spec_param "confirm"  "exact phrase 'install-nss-tools'; required only when something must be installed"
cap_spec_param "manager"  "package manager to use (default: whichever this machine has)"

readonly CONFIRM_INSTALL="install-nss-tools"

#=============================================================================
# WHICH PACKAGE, ON WHICH MANAGER
#=============================================================================

# The package carrying certutil differs per distribution, and the manager's
# non-interactive flags differ with it. One table, so a new distribution is one
# row rather than a branch in three places.
#
# apt-get is listed before the others because a machine can carry more than one
# (a Debian box with linuxbrew), and the SYSTEM manager is the right one to
# install a system tool with.
function nss_manager() {
    local override="$1" candidate
    if [[ -n "$override" ]]; then
        printf '%s\n' "$override"
        return
    fi
    for candidate in apt-get dnf yum pacman zypper brew; do
        if command -v "$candidate" &>/dev/null; then
            printf '%s\n' "$candidate"
            return
        fi
    done
}

# nss_package <manager> -- the package name certutil lives in.
function nss_package() {
    case "$1" in
        apt-get) printf 'libnss3-tools\n' ;;
        dnf|yum)  printf 'nss-tools\n' ;;
        pacman)   printf 'nss\n' ;;
        zypper)   printf 'mozilla-nss-tools\n' ;;
        brew)     printf 'nss\n' ;;
    esac
}

# nss_install_argv <manager> <package> -- the full non-interactive command,
# printed one argv element per line so the caller can read it into an array
# without a shell ever re-parsing it.
function nss_install_argv() {
    local manager="$1" package="$2"
    case "$manager" in
        apt-get) printf '%s\n' apt-get install -y "$package" ;;
        dnf)     printf '%s\n' dnf install -y "$package" ;;
        yum)     printf '%s\n' yum install -y "$package" ;;
        pacman)  printf '%s\n' pacman -S --noconfirm "$package" ;;
        zypper)  printf '%s\n' zypper --non-interactive install "$package" ;;
        brew)    printf '%s\n' brew install "$package" ;;
    esac
}

# nss_manager_needs_root <manager> -- Homebrew installs into a prefix the
# operator owns and REFUSES to run as root; every system manager needs it.
function nss_manager_needs_root() {
    [[ "$1" != "brew" ]]
}

#=============================================================================
# INSTALLING
#=============================================================================

_NSS_INSTALLED=false

# install_package <manager> <package> <confirm>
function install_package() {
    local manager="$1" package="$2" confirm="$3"
    local -a argv=()
    while IFS= read -r part; do argv+=("$part"); done < <(nss_install_argv "$manager" "$package")
    if [[ "${#argv[@]}" -eq 0 ]]; then
        cap_fail 2 "no install command is known for package manager '${manager}'"
    fi

    # Installing a system package is a change to shared machine state, so it is
    # gated on the phrase (contract rule 3). A run that finds certutil already
    # present never reaches here and needs no confirmation, because it changes
    # nothing.
    cap_confirm_or_die "$confirm" "$CONFIRM_INSTALL"

    cap_step "installing ${package} with ${manager}"
    if ! nss_manager_needs_root "$manager"; then
        if ! "${argv[@]}" >&2; then
            cap_fail 5 "${manager} failed to install ${package}"
        fi
        _NSS_INSTALLED=true
        return 0
    fi

    if ! elevate_begin "install ${package}, so browsers can trust the local certificate authority"; then
        cap_result_set remedy "sudo ${argv[*]}"
        cap_fail 4 "installing ${package} needs root, and $(elevate_no_ask_reason). Run the command below in a terminal, then retry."
    fi
    cap_info "${package} needs root to install -- $(elevate_explain)"
    if ! elevate_run "${argv[@]}" >&2; then
        elevate_end
        cap_result_set remedy "sudo ${argv[*]}"
        cap_fail 4 "could not install ${package} as root -- the password prompt was cancelled, or the password was not accepted. Retry, or run the command below in a terminal."
    fi
    elevate_end
    _NSS_INSTALLED=true
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local certutil_bin confirm manager_override manager package
    certutil_bin="$(cap_param certutil "certutil")"
    confirm="$(cap_param confirm "")"
    manager_override="$(cap_param manager "")"

    # THE PROBE COMES FIRST, and answers with the binary's own path rather than
    # a boolean: mkcert-setup.sh takes --certutil, so a machine where it lives
    # somewhere unusual is one value away from working.
    local found=""
    if found="$(command -v "$certutil_bin" 2>/dev/null)" && [[ -n "$found" ]]; then
        cap_info "certutil is already here (${found}) -- nothing to install."
        cap_result_set     certutil     "$found"
        cap_result_set     manager      ""
        cap_result_set     package      ""
        cap_result_set_raw preExisting  true
        cap_result_set_raw installed    true
        cap_ok
    fi

    manager="$(nss_manager "$manager_override")"
    if [[ -z "$manager" ]]; then
        cap_fail 4 "certutil is missing and no package manager this script knows (apt-get, dnf, yum, pacman, zypper, brew) is on this machine. Install the NSS tools package for this system by hand, then retry."
    fi
    package="$(nss_package "$manager")"
    if [[ -z "$package" ]]; then
        cap_fail 2 "no NSS tools package is known for package manager '${manager}'"
    fi

    install_package "$manager" "$package" "$confirm"
    cap_changed

    # PROVE IT, rather than trusting the package manager's exit code: the whole
    # point of this step is that certutil is CALLABLE afterwards, and a manager
    # that reported success while installing a package with a different layout
    # would otherwise hand mkcert the same silent failure this step exists to
    # remove.
    if ! found="$(command -v "$certutil_bin" 2>/dev/null)" || [[ -z "$found" ]]; then
        cap_fail 5 "${manager} installed ${package} but certutil is still not on PATH"
    fi

    cap_info "Done. ${found} is available; mkcert can now write the browser trust store."
    cap_result_set     certutil     "$found"
    cap_result_set     manager      "$manager"
    cap_result_set     package      "$package"
    cap_result_set_raw preExisting  false
    cap_result_set_raw installed    true
    cap_ok
}

main "$@"
