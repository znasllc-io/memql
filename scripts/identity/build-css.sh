#!/usr/bin/env bash
# scripts/identity/build-css.sh
# =============================
#
# Compile the Tailwind input source into the served app.css.
#
# Resolves the right standalone Tailwind binary for the host
# platform, downloads it on demand into bin/tools/ if missing,
# then runs it against component/identity/web/tailwind/input.css
# scanning the templ sources for class names.
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

# resolve_tailwind_binary → echoes the path to the Tailwind CLI for
# the current platform; downloads it from github releases if missing.
function resolve_tailwind_binary() {
    local platform
    platform="$(detect_platform)"
    local bin_path="${TOOLS_DIR}/tailwindcss-${platform}"
    if [[ -x "${bin_path}" ]]; then
        echo "${bin_path}"
        return 0
    fi
    mkdir -p "${TOOLS_DIR}"
    local url="https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-${platform}"
    log_info "downloading Tailwind ${TAILWIND_VERSION} for ${platform}"
    # Retries: a single TLS hiccup on this ~100MB pull killed entire
    # cluster builds (memql#1351). Docker builds normally never reach
    # this path -- the Dockerfiles pre-fetch the binary into bin/tools
    # in an early cached layer (keep their ARG TAILWIND_VERSION in
    # sync with the pin above) -- so this download is the host-side /
    # cold-layer fallback.
    if ! curl -sSL --fail --retry 5 --retry-all-errors --retry-delay 2 -o "${bin_path}" "${url}"; then
        log_error "failed to download ${url}"
        rm -f "${bin_path}"
        exit 1
    fi
    chmod +x "${bin_path}"
    echo "${bin_path}"
}

function compile_css() {
    local bin_path="$1"
    log_info "compiling Tailwind -> $(basename "${OUTPUT}")"
    "${bin_path}" \
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
