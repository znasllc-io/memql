#!/usr/bin/env bash
# scripts/identity/build-css.sh
# =============================
#
# Compile the Tailwind input source into the served app.css.
#
# Resolves the right standalone Tailwind binary for the host
# platform, reuses a compiler on PATH or in bin/tools/ only at the pinned
# version and otherwise downloads the pin into bin/tools/, then runs it
# against component/identity/web/tailwind/input.css
# scanning the templ sources for class names. On macOS a bin/tools binary
# whose code signature does not verify is re-signed ad hoc before it runs:
# the release assets ship a signature that no longer matches the file, and
# the kernel kills them on launch (ensure_valid_signature).
#
# The output (component/identity/web/static/app.css) is gitignored;
# every build regenerates it. Same script runs in the Dockerfile so
# the production image carries a freshly-compiled CSS bundle.
#
# The palette, the Tailwind bridge and the faces are NOT here -- they are
# brand/ at the repo root, shared with the portal (brand/README.md). This
# script also copies brand/fonts/ next to the output, which is what makes the
# shared stylesheet's relative font URLs resolve when served.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/bin/tools"
TAILWIND_VERSION="v4.1.11"

INPUT="${REPO_ROOT}/component/identity/web/tailwind/input.css"
OUTPUT="${REPO_ROOT}/component/identity/web/static/app.css"

# The standalone compiler embeds Bun. Linux workers prohibit writable memory
# from becoming executable, so run its JavaScript interpreter without a JIT.
# Scope the option to Tailwind (including its version probe); do not relax the
# worker's protections or change the environment of other build tools.
function run_tailwind() {
    BUN_JSC_useJIT=0 "$@"
}

# is_pinned_tailwind <bin> → succeeds only when <bin> runs and the first line
# of its --help is exactly "≈ tailwindcss ${TAILWIND_VERSION}". A different
# version must not silently change the generated CSS, so a compiler on PATH or
# in bin/tools is reused only when this holds.
function is_pinned_tailwind() {
    local help
    help="$(NO_COLOR=1 run_tailwind "$1" --help 2>/dev/null)" && \
        [[ "${help%%$'\n'*}" == "≈ tailwindcss ${TAILWIND_VERSION}" ]]
}

# detect_platform → echoes the asset suffix Tailwind's release uses
# (matches the names under github.com/tailwindlabs/tailwindcss/releases).
function detect_platform() {
    local kernel
    kernel="$(uname -s | tr '[:upper:]' '[:lower:]')"
    local arch
    arch="$(uname -m)"
    case "${kernel}/${arch}" in
        darwin/arm64)  echo "macos-arm64" ;;
        darwin/x86_64) echo "macos-x64" ;;
        linux/x86_64)  echo "linux-x64" ;;
        linux/aarch64) echo "linux-arm64" ;;
        linux/arm64)   echo "linux-arm64" ;;
        *)
            log_error "unsupported platform ${kernel}/${arch}"
            exit 1
            ;;
    esac
}

# ensure_valid_signature <platform> <bin_path> → on macOS, re-signs a
# bin/tools compiler ad hoc when its code signature does not verify, before
# its first run. Linux does not enforce code signatures, so it does nothing
# there.
#
# macOS kills (SIGKILL, "Code Signature Invalid") a process whose pages do
# not match its signature, and Tailwind's macOS release assets ship that way:
# v4.1.11 carries the linker's ad-hoc signature over a file modified after
# linking, so even --help exits 137 and `codesign --verify` reports "code or
# signature have been modified". Re-signing ad hoc over the file as downloaded
# makes it run and compile unchanged. An ad-hoc signature says nothing about
# who built the file, so it grants nothing the pinned download did not.
#
# `codesign --verify` decides, not "was this just downloaded": one rule
# repairs a fresh download and a bin/tools copy cached before this check
# existed, and leaves a compiler that already verifies alone, so a shared
# checkout's binary is not rewritten on every build and a valid upstream
# signature survives a later pin. A compiler on PATH never comes here: it is
# not ours to rewrite, and the --help probe already skips one the kernel kills.
# Each branch is pinned by build_css_test.go's
# TestBuildCSSRepairsTheMacOSCompilerSignatureBeforeRunningIt.
function ensure_valid_signature() {
    local platform="$1" bin_path="$2"
    [[ "${platform}" == macos-* ]] || return 0
    if ! require_cmd codesign; then
        log_error "macOS ships it as /usr/bin/codesign; put /usr/bin on PATH so $(basename "${bin_path}") can be checked before it runs"
        exit 1
    fi
    local reason
    if reason="$(codesign --verify "${bin_path}" 2>&1)"; then
        return 0
    fi
    reason="${reason%%$'\n'*}"
    log_info "re-signing $(basename "${bin_path}") ad hoc: ${reason#"${bin_path}: "}"
    if ! reason="$(codesign --force --sign - "${bin_path}" 2>&1)"; then
        log_error "could not re-sign ${bin_path}: ${reason}"
        exit 1
    fi
}

# GitHub's browser download route can return 504 while the same release asset
# remains available through its documented API. Only use this second route
# after the normal retries fail. Resolve the exact published pin and platform;
# never accept a URL from metadata, another release, or unverified bytes. The
# digest is checked before macOS signing can change the downloaded file.
function download_tailwind_from_api() (
    local platform="$1" bin_path="$2" temporary metadata asset_id asset_size asset_digest
    require_cmd python3 || return 1
    temporary="$(mktemp -d "${TOOLS_DIR}/.tailwind-download.XXXXXX")" || return 1
    trap 'rm -rf "${temporary}"' EXIT
    if ! curl -sSL --fail --proto '=https' --proto-redir '=https' --max-redirs 3 \
        --connect-timeout 20 --max-time 60 --max-filesize 1048576 \
        -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
        -o "${temporary}/release.json" \
        "https://api.github.com/repos/tailwindlabs/tailwindcss/releases/tags/${TAILWIND_VERSION}"; then
        log_error "could not read the official Tailwind release metadata"
        return 1
    fi
    if ! metadata="$(python3 - "${temporary}/release.json" "${TAILWIND_VERSION}" "tailwindcss-${platform}" <<'PY'
import json
import re
import sys

try:
    with open(sys.argv[1], "rb") as source:
        body = source.read(1048577)
    if len(body) > 1048576:
        raise ValueError("release metadata exceeds 1 MiB")
    release = json.loads(body)
    if not isinstance(release, dict) or release.get("tag_name") != sys.argv[2] or release.get("draft") is not False or release.get("prerelease") is not False:
        raise ValueError("release metadata does not name the exact published pin")
    assets = release.get("assets")
    if not isinstance(assets, list):
        raise ValueError("release metadata has no asset list")
    matches = [asset for asset in assets if isinstance(asset, dict) and asset.get("name") == sys.argv[3]]
    if len(matches) != 1:
        raise ValueError("release metadata must contain exactly one matching platform asset")
    asset = matches[0]
    asset_id, size, digest = asset.get("id"), asset.get("size"), asset.get("digest")
    if type(asset_id) is not int or not 0 < asset_id <= 9223372036854775807 or asset.get("state") != "uploaded":
        raise ValueError("release asset has no valid uploaded identity")
    if type(size) is not int or not 0 < size < 256 * 1024 * 1024:
        raise ValueError("release asset size is outside the permitted bound")
    if not isinstance(digest, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None:
        raise ValueError("release asset has no valid SHA256 digest")
    print(asset_id, size, digest[7:])
except (OSError, ValueError) as error:
    sys.exit("Tailwind API fallback refused: " + str(error))
PY
    )"; then
        return 1
    fi
    read -r asset_id asset_size asset_digest <<< "${metadata}"
    if ! curl -sSL --fail --proto '=https' --proto-redir '=https' --max-redirs 3 \
        --connect-timeout 20 --max-time 300 --max-filesize "${asset_size}" \
        -H 'Accept: application/octet-stream' -H 'X-GitHub-Api-Version: 2022-11-28' \
        -o "${temporary}/tailwindcss" \
        "https://api.github.com/repos/tailwindlabs/tailwindcss/releases/assets/${asset_id}"; then
        log_error "could not download the official Tailwind release asset"
        return 1
    fi
    if ! python3 - "${temporary}/tailwindcss" "${asset_size}" "${asset_digest}" <<'PY'
import hashlib
import os
import sys

try:
    if os.path.getsize(sys.argv[1]) != int(sys.argv[2]):
        raise ValueError("release asset size does not match its metadata")
    digest = hashlib.sha256()
    with open(sys.argv[1], "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != sys.argv[3]:
        raise ValueError("release asset SHA256 does not match its metadata")
except (OSError, ValueError) as error:
    sys.exit("Tailwind API fallback refused: " + str(error))
PY
    then
        return 1
    fi
    mv "${temporary}/tailwindcss" "${bin_path}"
)

# resolve_tailwind_binary → echoes the path to the Tailwind CLI for
# the current platform at the pin; downloads it from github releases
# when bin/tools holds no working copy at the pin, and on macOS repairs
# a bin/tools copy's signature before it runs (ensure_valid_signature).
function resolve_tailwind_binary() {
    # Pipeline checkouts are ephemeral; the toolchain image carries this
    # compiler once for every step.
    local installed
    if installed="$(command -v tailwindcss)" && is_pinned_tailwind "${installed}"; then
        echo "${installed}"
        return 0
    fi
    local platform
    platform="$(detect_platform)"
    local bin_path="${TOOLS_DIR}/tailwindcss-${platform}"
    # bin/tools holds what a Dockerfile pre-fetched at its own ARG
    # TAILWIND_VERSION, or what an earlier build downloaded at an earlier
    # pin, so it is held to the pin like PATH. A copy that fails the probe
    # is downloaded over rather than skipped, because this path is the one
    # place the pin is downloaded to. The signature repair comes first
    # because on macOS the version probe is itself a run. Pinned by
    # build_css_test.go's TestBuildCSSReplacesACachedCompilerThatIsNotThePin.
    if [[ -x "${bin_path}" ]]; then
        ensure_valid_signature "${platform}" "${bin_path}"
        if is_pinned_tailwind "${bin_path}"; then
            echo "${bin_path}"
            return 0
        fi
        log_info "$(basename "${bin_path}") is not a working Tailwind ${TAILWIND_VERSION}; replacing it"
    fi
    mkdir -p "${TOOLS_DIR}"
    local url="https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-${platform}"
    log_info "downloading Tailwind ${TAILWIND_VERSION} for ${platform}"
    # Retries: a single TLS hiccup on this ~100MB pull killed entire
    # cluster builds (memql#1351). Docker builds normally never reach
    # this path -- the root Dockerfile pre-fetches the binary into
    # bin/tools in an early cached layer, and build_css_test.go's
    # TestEveryDockerfileDownloadsTheTailwindPin holds its ARG
    # TAILWIND_VERSION to the pin above -- so this download is the
    # host-side / cold-layer fallback.
    if ! curl -sSL --fail --retry 5 --retry-all-errors --retry-delay 2 -o "${bin_path}" "${url}"; then
        rm -f "${bin_path}"
        log_info "browser download failed; trying the official Tailwind asset API"
        if ! download_tailwind_from_api "${platform}" "${bin_path}"; then
            log_error "failed to download verified Tailwind ${TAILWIND_VERSION} for ${platform}"
            rm -f "${bin_path}"
            exit 1
        fi
    fi
    chmod +x "${bin_path}"
    ensure_valid_signature "${platform}" "${bin_path}"
    echo "${bin_path}"
}

function compile_css() {
    local bin_path="$1"
    log_info "compiling Tailwind -> $(basename "${OUTPUT}")"
    run_tailwind "${bin_path}" \
        --input  "${INPUT}" \
        --output "${OUTPUT}" \
        --minify
}

# copy_fonts places the brand faces next to the served stylesheet.
#
# brand/fonts.css declares src: url("./fonts/x.woff2"), which the portal's
# bundler rewrites and the Tailwind CLI passes through untouched. Passing it
# through is what we want: relative to the SERVED file (/static/app.css) it
# resolves to /static/fonts/x.woff2, so the faces just have to be there. The
# alternative -- an absolute URL in the shared file -- would break the portal's
# bundling, so the copy lives here rather than in the stylesheet.
function copy_fonts() {
    local dest="${REPO_ROOT}/component/identity/web/static/fonts"
    log_info "copying brand faces -> static/fonts"
    mkdir -p "${dest}"
    cp "${REPO_ROOT}"/brand/fonts/*.woff2 "${dest}/"

    # The favicon is a real file at a fixed path (the layout links it), so it
    # is copied rather than imported -- the same one deliberate copy the portal
    # keeps in public/. brand_shared_source_test.go pins both to the source.
    log_info "copying brand favicon -> static/favicon.svg"
    cp "${REPO_ROOT}/brand/favicon.svg" "${REPO_ROOT}/component/identity/web/static/favicon.svg"
}

function main() {
    if [[ ! -f "${INPUT}" ]]; then
        log_error "input not found: ${INPUT}"
        exit 1
    fi
    local bin
    bin="$(resolve_tailwind_binary)"
    compile_css "${bin}"
    copy_fonts
}

main "$@"
