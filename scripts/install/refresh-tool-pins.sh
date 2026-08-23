#!/usr/bin/env bash
#
# scripts/install/refresh-tool-pins.sh
# ====================================
#
# Capability: install.refreshPins -- regenerate scripts/install/tool-pins.env.
#
# Resolves an upstream release for each installer-managed tool (k3d, kubectl,
# mkcert), DOWNLOADS the exact artifact FOR EVERY SUPPORTED PLATFORM, and
# records the triple
#
#     <TOOL>_<OS>_<ARCH>_VERSION   the release tag (semver)
#     <TOOL>_<OS>_<ARCH>_URL       the artifact URL that tag resolves to
#     <TOOL>_<OS>_<ARCH>_SHA256    the sha256 of the bytes actually downloaded
#
# The key is platform-qualified (memql#4295) because the whole content of a pin
# is "these exact bytes", and the bytes differ per platform. One run writes
# every platform, so a machine can never be the reason a platform's pins are
# stale -- this script downloads a darwin artifact from Linux and hashes it
# without running it.
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
# shellcheck source=../lib/platform.sh
source "${SCRIPT_DIR}/../lib/platform.sh"

cap_init "install.refreshPins" "Regenerate the digest-pinned installer tool manifest."
cap_spec_param "out"              "output path for the generated pins file"
cap_spec_param "k3d-version"      "k3d release tag to pin (default: latest upstream)"
cap_spec_param "kubectl-version"  "kubectl release tag to pin (default: latest stable)"
cap_spec_param "mkcert-version"   "mkcert release tag to pin (default: latest upstream)"

# The platform set is scripts/lib/platform.sh's SUPPORTED_PLATFORMS -- the same
# list detect.sh refuses against and install-binary.sh composes keys from. A
# second copy here would let the generator and the consumer disagree about what
# is supported, which presents as a platform that refuses despite having pins.
readonly PIN_TOOLS=(k3d kubectl mkcert)

# Anything smaller than this is an error page or a truncated transfer, not a
# release binary. Guards against pinning the digest of a 404 body.
readonly MIN_ARTIFACT_BYTES=1000000

#=============================================================================
# SCRATCH CLEANUP
#=============================================================================

# Artifacts are downloaded into a temp dir. Removing it needs an EXIT trap,
# which REPLACES the one cap_init installs -- the trap that guarantees a failure
# envelope on an unexpected abort -- so the two are chained here.
#
# `set +e` is load-bearing: the handler runs under the script's errexit, and
# `(exit "$rc")` (which restores $? for _cap_on_exit) is by definition a failing
# command whenever rc is non-zero. Without it, errexit abandons the handler and
# the caller gets a non-zero exit with no envelope at all.
#
# A RETURN trap inside main() does NOT work here: cap_ok exits the shell from
# within main, so main never returns and the scratch dir -- holding the last
# release binary downloaded, tens of MB -- leaks on every successful run.
_REFRESH_SCRATCH=""

function _refresh_on_exit() {
    local rc=$?
    set +e
    if [[ -n "$_REFRESH_SCRATCH" ]]; then
        rm -rf "$_REFRESH_SCRATCH" 2>/dev/null
    fi
    (exit "$rc")
    _cap_on_exit
}
trap _refresh_on_exit EXIT

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

# artifact_url <tool> <version> <os> <arch> -- the artifact that version names
# for that platform.
#
# Each upstream spells the platform differently in its asset names, which is
# exactly why this is a function per tool rather than a template: k3d uses
# `k3d-<os>-<arch>`, kubectl a path segment pair, mkcert `mkcert-<v>-<os>-<arch>`.
function artifact_url() {
    local tool="$1" v="$2" os="$3" arch="$4"
    case "$tool" in
        k3d)     printf 'https://github.com/k3d-io/k3d/releases/download/%s/k3d-%s-%s' "$v" "$os" "$arch" ;;
        kubectl) printf 'https://dl.k8s.io/release/%s/bin/%s/%s/kubectl' "$v" "$os" "$arch" ;;
        mkcert)  printf 'https://github.com/FiloSottile/mkcert/releases/download/%s/mkcert-%s-%s-%s' "$v" "$v" "$os" "$arch" ;;
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
# carries BOTH a semver version and the sha256 of the exact artifact that
# version resolves to. install.binary refuses any download whose digest does
# not match, which is why the installer never pipes curl into a shell. Changing
# a pin is a reviewed diff, never a silent auto-update.
#
# KEYS ARE PLATFORM-QUALIFIED: <TOOL>_<OS>_<ARCH>_{VERSION,URL,SHA256}
# (memql#4295). They were bare -- K3D_URL -- when the installer targeted one
# platform. A second platform cannot share a key, because the whole content of
# a pin is "these exact bytes", and the bytes differ per platform; a bare key
# would either silently serve a Linux binary to a Mac or force one platform's
# pins to be the ones nobody could verify.
#
# platforms: $(platform_supported_csv)
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

    _REFRESH_SCRATCH="$(mktemp -d)"
    scratch="$_REFRESH_SCRATCH"
    body="${scratch}/body"
    : > "$body"

    # Versions are resolved ONCE per tool and shared across platforms: a pin
    # set where one platform lags a release is a pin set nobody can reason
    # about, and every upstream here cuts one tag across all platforms.
    local -A versions=()
    for tool in "${PIN_TOOLS[@]}"; do
        cap_step "Resolving ${tool}..."
        versions["$tool"]="$(resolve_version "$tool" "$(cap_param "${tool}-version" "")")"
        cap_info "${tool} ${versions[$tool]}"
    done

    local summaries=() platform os arch suffix
    for platform in "${SUPPORTED_PLATFORMS[@]}"; do
        os="${platform%%/*}"
        arch="${platform##*/}"
        suffix="$(platform_pin_suffix "$platform")"
        printf '\n# --- %s %s\n' "$platform" "$(printf -- '-%.0s' $(seq 1 $((60 - ${#platform}))))" >> "$body"
        for tool in "${PIN_TOOLS[@]}"; do
            version="${versions[$tool]}"
            url="$(artifact_url "$tool" "$version" "$os" "$arch")"
            cap_info "${tool} ${version} ${platform} -> ${url}"
            digest="$(digest_of_url "$url" "$scratch")"
            if [[ ! "$digest" =~ ^[0-9a-f]{64}$ ]]; then
                cap_fail 5 "sha256 of ${url} came back as '${digest}', not a 64-hex digest"
            fi
            cap_info "${tool} ${platform} sha256 ${digest}"

            local key
            key="$(printf '%s' "$tool" | tr '[:lower:]-' '[:upper:]_')_${suffix}"
            {
                printf '\n%s_VERSION=%s\n' "$key" "$version"
                printf '%s_URL=%s\n'       "$key" "$url"
                printf '%s_SHA256=%s\n'    "$key" "$digest"
            } >> "$body"
            summaries+=("{\"tool\":\"${tool}\",\"platform\":\"${platform}\",\"version\":\"${version}\",\"sha256\":\"${digest}\"}")
        done
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
    cap_result_set     platforms "$(platform_supported_csv)"
    cap_result_set_raw written  "$changed"
    cap_result_set_raw pins     "[$(IFS=,; printf '%s' "${summaries[*]}")]"
    cap_ok
}

main "$@"
