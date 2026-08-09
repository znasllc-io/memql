#!/usr/bin/env bash
#
# scripts/install/refresh-tool-pins.sh
# ====================================
#
# Capability: install.refreshPins -- regenerate scripts/install/tool-pins.env.
#
# Resolves an upstream release for each installer-managed tool (k3d, kubectl,
# mkcert), DOWNLOADS the exact linux/amd64 artifact, and records the triple
#
#     <TOOL>_VERSION   the release tag (semver)
#     <TOOL>_URL       the artifact URL that tag resolves to
#     <TOOL>_SHA256    the sha256 of the bytes actually downloaded
#
# into a committed env file. `install.binary` (scripts/install/install-binary.sh)
# then downloads only those URLs and refuses any artifact whose digest does not
# match. That is why the installer never does `curl | bash`: nothing is executed
# before it has been verified against a digest reviewed in a pull request.
#
# The file is regenerated deliberately, never on the fly -- a pin that a machine
# can silently update is not a pin. Every run rewrites ALL tools, so the file is
# complete by construction; a version without a digest is impossible here.
#
# NETWORK: this script is the ONLY part of the installer that talks to upstream
# release infrastructure at authoring time. It fails loudly (exit 5) on a failed
# or truncated download rather than writing a placeholder digest -- a fake digest
# looks deliberate while verifying nothing.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/refresh-tool-pins.sh                      # latest of each
#   scripts/install/refresh-tool-pins.sh --k3d-version=v5.8.3 # pin one tool
#   scripts/install/refresh-tool-pins.sh --out=/tmp/pins.env  # write elsewhere
#   scripts/install/refresh-tool-pins.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing | 5 operation failed
#
# Refs: #3358 #3357 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "install.refreshPins" "Regenerate the digest-pinned installer tool manifest."
cap_spec_param "out"              "output path for the generated pins file"
cap_spec_param "k3d-version"      "k3d release tag to pin (default: latest upstream)"
cap_spec_param "kubectl-version"  "kubectl release tag to pin (default: latest stable)"
cap_spec_param "mkcert-version"   "mkcert release tag to pin (default: latest upstream)"

# The installer graph is Linux/amd64 only -- this epic is Linux-only by design.
readonly PIN_PLATFORM="linux/amd64"
readonly PIN_TOOLS=(k3d kubectl mkcert)

# Anything smaller than this is an error page or a truncated transfer, not a
# release binary. Guards against pinning the digest of a 404 body.
readonly MIN_ARTIFACT_BYTES=1000000

#=============================================================================
# PREREQUISITES
#=============================================================================

function check_prerequisites() {
    command -v curl &>/dev/null \
        || cap_fail 4 "curl is required to fetch release artifacts"
    _sha256_tool >/dev/null \
        || cap_fail 4 "no sha256 tool found (need sha256sum or shasum)"
}

# _sha256_tool -- prints the sha256 command to use, or fails.
function _sha256_tool() {
    if command -v sha256sum &>/dev/null; then printf 'sha256sum'; return 0; fi
    if command -v shasum    &>/dev/null; then printf 'shasum -a 256'; return 0; fi
    return 1
}

#=============================================================================
# UPSTREAM RESOLUTION
#=============================================================================

# _fetch_text <url> -- GET a small text/JSON document to stdout.
function _fetch_text() {
    curl -fsSL --retry 3 --retry-delay 2 --max-time 60 "$1"
}

# _latest_github_tag <owner/repo> -- newest release tag, e.g. "v5.8.3".
function _latest_github_tag() {
    local repo="$1" body tag
    body="$(_fetch_text "https://api.github.com/repos/${repo}/releases/latest")" \
        || cap_fail 5 "could not query the latest ${repo} release (network or rate limit)"
    tag="$(printf '%s' "$body" \
        | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' \
        | head -1 | sed -E 's/.*"([^"]*)"$/\1/')"
    [[ -n "$tag" ]] || cap_fail 5 "could not parse a tag_name from the ${repo} release payload"
    printf '%s' "$tag"
}

# resolve_version <tool> <explicit-or-empty> -- prints the tag to pin.
function resolve_version() {
    local tool="$1" explicit="${2:-}"
    if [[ -n "$explicit" ]]; then printf '%s' "$explicit"; return; fi
    case "$tool" in
        k3d)     _latest_github_tag "k3d-io/k3d" ;;
        mkcert)  _latest_github_tag "FiloSottile/mkcert" ;;
        kubectl)
            local v
            v="$(_fetch_text "https://dl.k8s.io/release/stable.txt" | tr -d '[:space:]')" \
                || cap_fail 5 "could not resolve the stable kubectl release"
            [[ -n "$v" ]] || cap_fail 5 "dl.k8s.io/release/stable.txt returned nothing"
            printf '%s' "$v" ;;
        *) cap_fail 2 "unknown tool: ${tool}" ;;
    esac
}

# artifact_url <tool> <version> -- the linux/amd64 artifact that version names.
function artifact_url() {
    local tool="$1" v="$2"
    case "$tool" in
        k3d)     printf 'https://github.com/k3d-io/k3d/releases/download/%s/k3d-linux-amd64' "$v" ;;
        kubectl) printf 'https://dl.k8s.io/release/%s/bin/linux/amd64/kubectl' "$v" ;;
        mkcert)  printf 'https://github.com/FiloSottile/mkcert/releases/download/%s/mkcert-%s-linux-amd64' "$v" "$v" ;;
        *) cap_fail 2 "unknown tool: ${tool}" ;;
    esac
}

#=============================================================================
# DIGEST CAPTURE
#=============================================================================

# digest_of_url <url> <scratch-dir> -- downloads and prints the sha256. Fails
# loudly (exit 5) rather than yielding a digest of nothing.
function digest_of_url() {
    local url="$1" scratch="$2" file bytes
    file="${scratch}/artifact"
    rm -f "$file"
    if ! curl -fsSL --retry 3 --retry-delay 2 --max-time 600 -o "$file" "$url"; then
        cap_fail 5 "download failed: ${url} (refusing to record a placeholder digest)"
    fi
    bytes="$(wc -c < "$file" | tr -d '[:space:]')"
    if [[ "$bytes" -lt "$MIN_ARTIFACT_BYTES" ]]; then
        cap_fail 5 "artifact from ${url} is only ${bytes} bytes -- that is an error page, not a release binary"
    fi
    $(_sha256_tool) "$file" | awk '{print $1}'
}

#=============================================================================
# GENERATION
#=============================================================================

# render_pins <generated-body-file> -- assembles the full pins file on stdout.
function render_pins() {
    local body="$1"
    cat <<EOF
# scripts/install/tool-pins.env
# =============================
#
# GENERATED FILE -- do not edit by hand.
# Regenerate with: scripts/install/refresh-tool-pins.sh
#
# Digest-pinned downloads for the local-cluster installer (#3358). Every tool
# carries BOTH a semver version and the sha256 of the exact linux/amd64
# artifact that version resolves to. install.binary refuses any download whose
# digest does not match, which is why the installer never pipes curl into a
# shell. Changing a pin is a reviewed diff, never a silent auto-update.
#
# platform: ${PIN_PLATFORM}
EOF
    cat "$body"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local out scratch body tool version url digest changed=false
    out="$(cap_param out "${SCRIPT_DIR}/tool-pins.env")"
    cap_require out "$out"

    check_prerequisites

    scratch="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '${scratch}'" RETURN
    body="${scratch}/body"
    : > "$body"

    local summaries=()
    for tool in "${PIN_TOOLS[@]}"; do
        cap_step "Resolving ${tool}..."
        version="$(resolve_version "$tool" "$(cap_param "${tool}-version" "")")"
        url="$(artifact_url "$tool" "$version")"
        cap_info "${tool} ${version} -> ${url}"
        digest="$(digest_of_url "$url" "$scratch")"
        if [[ ! "$digest" =~ ^[0-9a-f]{64}$ ]]; then
            cap_fail 5 "sha256 of ${url} came back as '${digest}', not a 64-hex digest"
        fi
        cap_info "${tool} sha256 ${digest}"

        local key
        key="$(printf '%s' "$tool" | tr '[:lower:]-' '[:upper:]_')"
        {
            printf '\n%s_VERSION=%s\n' "$key" "$version"
            printf '%s_URL=%s\n'       "$key" "$url"
            printf '%s_SHA256=%s\n'    "$key" "$digest"
        } >> "$body"
        summaries+=("{\"tool\":\"${tool}\",\"version\":\"${version}\",\"sha256\":\"${digest}\"}")
    done

    render_pins "$body" > "${scratch}/pins.env"
    if [[ -f "$out" ]] && cmp -s "${scratch}/pins.env" "$out"; then
        cap_info "Pins unchanged: ${out}"
    else
        mkdir -p "$(dirname "$out")"
        mv "${scratch}/pins.env" "$out"
        changed=true
        cap_changed
        cap_info "Wrote ${out}"
    fi

    cap_result_set     out      "$out"
    cap_result_set     platform "$PIN_PLATFORM"
    cap_result_set_raw written  "$changed"
    cap_result_set_raw pins     "[$(IFS=,; printf '%s' "${summaries[*]}")]"
    cap_ok
}

main "$@"
