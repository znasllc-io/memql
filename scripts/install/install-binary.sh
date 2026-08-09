#!/usr/bin/env bash
#
# scripts/install/install-binary.sh
# =================================
#
# Capability: install.binary -- install ONE pinned tool into a private bin dir.
#
# Reads scripts/install/tool-pins.env (#3358), downloads exactly the artifact
# that manifest names, verifies its sha256 against the committed digest, and
# only then moves it into place (default ~/.memql/bin). Nothing is executed --
# and nothing is even moved out of staging -- before it has been verified, which
# is what lets the installer avoid `curl | bash` entirely.
#
# Three behaviours worth stating outright, because each one is load-bearing:
#
#   --dry-run writes NOTHING. Not the binary, not the dest directory, not a
#   staging file. A dry run is a plan; an install that is merely quieter is a
#   trap, because it is exactly what a caller reaches for to inspect before
#   committing.
#
#   An unknown --tool is exit 2 (bad param). Nothing was attempted, and no
#   retry or prerequisite will make a tool appear in a manifest that does not
#   pin it.
#
#   preExisting records whether the tool ALREADY resolved on PATH from outside
#   dest. That flag is what a later uninstall reads to leave the user's own k3d
#   alone: we clean up what we put there, never what we found.
#
# A digest mismatch is exit 5 and the staged bytes are destroyed. There is no
# install-with-a-warning path -- an unverified artifact is simply not installed.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/install-binary.sh --tool=k3d
#   scripts/install/install-binary.sh --tool=kubectl --dest="$HOME/.memql/bin"
#   scripts/install/install-binary.sh --tool=mkcert --dry-run
#   scripts/install/install-binary.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 operation failed
#
# Refs: #3360 #3358 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.binary" "Download, digest-verify and install one pinned tool."
cap_spec_param "tool"    "which pinned tool to install (k3d | kubectl | mkcert)"
cap_spec_param "dest"    "directory to install into (default: \$HOME/.memql/bin)"
cap_spec_param "pins"    "path to the pins manifest (default: the committed tool-pins.env)"
cap_spec_param "dry-run" "report the plan and write nothing (flag)"

#=============================================================================
# PINS MANIFEST
#=============================================================================

# _pin_lookup <pins-file> <KEY> -- value of KEY, or empty. Parses the manifest
# by hand rather than sourcing it: the pins file is data, and data should never
# get a chance to execute.
function _pin_lookup() {
    local file="$1" key="$2" line k v
    while IFS= read -r line; do
        line="${line%%$'\r'}"
        [[ -z "$line" || "${line:0:1}" == "#" ]] && continue
        k="${line%%=*}"
        v="${line#*=}"
        k="${k//[[:space:]]/}"
        if [[ "$k" == "$key" ]]; then
            v="${v#\"}"; v="${v%\"}"
            printf '%s' "$v"
            return
        fi
    done < "$file"
}

# _pinned_tools <pins-file> -- the tool names the manifest actually pins,
# lowercased. The known-tool set is derived from the manifest, never
# hardcoded here, so adding a tool is a pins regeneration and not a code edit.
function _pinned_tools() {
    local file="$1" line k out=""
    while IFS= read -r line; do
        [[ -z "$line" || "${line:0:1}" == "#" ]] && continue
        k="${line%%=*}"
        k="${k//[[:space:]]/}"
        if [[ "$k" == *_URL ]]; then
            out+=" $(printf '%s' "${k%_URL}" | tr '[:upper:]_' '[:lower:]-')"
        fi
    done < "$file"
    printf '%s' "${out# }"
}

#=============================================================================
# DIGEST
#=============================================================================

function _sha256_of() {
    local file="$1"
    if command -v sha256sum &>/dev/null; then
        sha256sum "$file" | { read -r sum _; printf '%s' "$sum"; }
    elif command -v shasum &>/dev/null; then
        shasum -a 256 "$file" | { read -r sum _; printf '%s' "$sum"; }
    else
        return 1
    fi
}

#=============================================================================
# FETCH
#=============================================================================

# fetch <url> <out-path> -- retrieve an artifact to a staging path. file:// is
# handled directly (local artifacts, fixtures, air-gapped mirrors) so the fetch
# path does not depend on a downloader's protocol support.
function fetch() {
    local url="$1" out="$2"
    case "$url" in
        file://*)
            local src="${url#file://}"
            [[ -f "$src" ]] || return 1
            cp "$src" "$out" ;;
        http://*|https://*)
            if command -v curl &>/dev/null; then
                curl -fsSL --retry 3 --retry-delay 2 --max-time 600 -o "$out" "$url"
            elif command -v wget &>/dev/null; then
                wget -q -O "$out" "$url"
            else
                cap_fail 4 "neither curl nor wget is available to download ${url}"
            fi ;;
        *)
            cap_fail 2 "unsupported URL scheme in the pins manifest: ${url}" ;;
    esac
}

#=============================================================================
# STAGING CLEANUP
#=============================================================================

# The download is staged in a temp dir so a rejected artifact can never leave a
# partial file next to real binaries. Cleaning it up needs an EXIT trap, which
# would otherwise REPLACE the one cap_init installs -- and that trap is what
# guarantees a failure envelope on an unexpected abort. So we chain: capture the
# real exit status first, clean up, restore the status, then hand off.
_INSTALL_SCRATCH=""

function _install_on_exit() {
    local rc=$?
    # `set +e` is load-bearing: the handler runs under the script's errexit, and
    # `(exit "$rc")` below is by definition a failing command whenever rc is
    # non-zero. Without this, errexit abandons the handler right there and
    # _cap_on_exit never runs -- the caller gets a non-zero exit and NO envelope,
    # which is exactly the silence the result guarantee exists to prevent.
    set +e
    if [[ -n "$_INSTALL_SCRATCH" ]]; then
        rm -rf "$_INSTALL_SCRATCH" 2>/dev/null
    fi
    (exit "$rc")   # restore $? so _cap_on_exit reports the real code
    _cap_on_exit
}
trap _install_on_exit EXIT

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local tool dest pins dry_run
    tool="$(cap_param tool "")"
    dest="$(cap_param dest "${HOME:-/root}/.memql/bin")"
    pins="$(cap_param pins "${SCRIPT_DIR}/tool-pins.env")"
    dry_run="$(cap_flag dry-run)"
    cap_require tool "$tool"
    cap_require dest "$dest"

    # --- manifest -------------------------------------------------------
    if [[ ! -f "$pins" ]]; then
        cap_fail 4 "pins manifest not found at ${pins} -- regenerate it with scripts/install/refresh-tool-pins.sh"
    fi
    local key version url digest known
    key="$(printf '%s' "$tool" | tr '[:lower:]-' '[:upper:]_')"
    url="$(_pin_lookup "$pins" "${key}_URL")"
    if [[ -z "$url" ]]; then
        known="$(_pinned_tools "$pins")"
        cap_fail 2 "unknown tool '${tool}' -- ${pins} pins: ${known:-<none>}"
    fi
    version="$(_pin_lookup "$pins" "${key}_VERSION")"
    digest="$(_pin_lookup  "$pins" "${key}_SHA256")"
    if [[ ! "$digest" =~ ^[0-9a-f]{64}$ ]]; then
        cap_fail 4 "pin for '${tool}' has no usable sha256 in ${pins} -- refusing to install an unverifiable artifact"
    fi
    _sha256_of /dev/null >/dev/null 2>&1 || cap_fail 4 "no sha256 tool found (need sha256sum or shasum)"

    # --- pre-existing on PATH, from OUTSIDE dest ------------------------
    # The flag uninstall reads: true means the machine already had this tool
    # before we arrived, so removing dest must not take it away.
    local resolved dest_abs pre_existing=false
    dest_abs="$dest"
    if [[ -d "$dest" ]]; then
        dest_abs="$(cd "$dest" && pwd)"
    fi
    resolved="$(command -v "$tool" 2>/dev/null || true)"
    if [[ -n "$resolved" && "$resolved" != "${dest_abs}/"* && "$resolved" != "${dest}/"* ]]; then
        pre_existing=true
    fi

    local target="${dest}/${tool}"
    cap_info "${tool} ${version} -> ${target}"
    cap_info "pinned sha256 ${digest}"
    if [[ "$pre_existing" == "true" ]]; then
        cap_info "note: ${tool} already on PATH at ${resolved} (not managed by us)"
    fi

    cap_result_set     tool        "$tool"
    cap_result_set     version     "$version"
    cap_result_set     url         "$url"
    cap_result_set     sha256      "$digest"
    cap_result_set     dest        "$dest"
    cap_result_set     path        "$target"
    cap_result_set_raw preExisting "$pre_existing"

    # --- already installed and verified? --------------------------------
    if [[ -f "$target" ]] && [[ "$(_sha256_of "$target")" == "$digest" ]]; then
        cap_info "${tool} is already installed at the pinned digest -- nothing to do."
        cap_result_set_raw installed        true
        cap_result_set_raw alreadyInstalled true
        cap_result_set_raw dryRun           "$( [[ -n "$dry_run" ]] && echo true || echo false )"
        cap_ok
    fi
    cap_result_set_raw alreadyInstalled false

    # --- dry run: everything above was read-only, and we stop here ------
    if [[ -n "$dry_run" ]]; then
        cap_info "DRY RUN: would download ${url} and install it as ${target}"
        cap_result_set_raw installed false
        cap_result_set_raw dryRun    true
        cap_ok
    fi
    cap_result_set_raw dryRun false

    # --- download to staging OUTSIDE dest, verify, then place -----------
    local staged actual
    _INSTALL_SCRATCH="$(mktemp -d)"
    staged="${_INSTALL_SCRATCH}/${tool}"

    cap_step "Downloading ${url}"
    if ! fetch "$url" "$staged"; then
        cap_fail 5 "download failed: ${url}"
    fi

    actual="$(_sha256_of "$staged")"
    if [[ "$actual" != "$digest" ]]; then
        rm -f "$staged"
        cap_fail 5 "sha256 mismatch for ${tool}: got ${actual}, pinned ${digest} -- refusing to install"
    fi
    cap_info "Digest verified."

    # Placement is the only mutating step. Report its failure as 5 (the
    # operation failed) rather than letting errexit surface a bare 1.
    chmod 0755 "$staged" || cap_fail 5 "could not make the staged ${tool} executable"
    mkdir -p "$dest"     || cap_fail 5 "could not create the install directory ${dest}"
    mv "$staged" "$target" || cap_fail 5 "could not move ${tool} into ${target}"
    cap_changed
    cap_info "Installed ${tool} ${version} at ${target}"

    cap_result_set_raw installed true
    cap_ok
}

main "$@"
