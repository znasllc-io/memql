#!/usr/bin/env bash
set -euo pipefail

# Script: package.sh
# Purpose: Build the memql-lsp binary for a target platform, stage it into the
#          VS Code extension, compile the extension, and produce a .vsix.
#
# The offline LSP embeds the engine, so the binary is bundled per platform and
# the extension resolves it at bin/<node-platform>-<node-arch>/memql-lsp -- where
# node-platform / node-arch are Node's `process.platform` / `process.arch` values
# (darwin|linux|win32 / x64|arm64|ia32), NOT Go's GOOS/GOARCH (darwin|linux|windows
# / amd64|arm64|386). We BUILD with Go but STAGE under the Node-named directory the
# extension actually looks in, so producer and consumer always agree. (They only
# happen to coincide for arm64-on-darwin, which is why a Mac-only default silently
# broke every amd64 / windows host.)
#
# Defaults to the HOST platform (via `go env`), so `make vscode-install` bundles a
# binary that runs on THIS machine. Cross-build with --goos / --goarch.

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXT_DIR="$REPO_ROOT/editors/vscode"
# Default to the host platform. Fall back to the standardized dev target
# (darwin/arm64) if `go` is unavailable -- check_prerequisites reports that
# clearly a moment later.
DEFAULT_GOOS="$(go env GOHOSTOS 2>/dev/null || echo darwin)"
DEFAULT_GOARCH="$(go env GOHOSTARCH 2>/dev/null || echo arm64)"
VSCE_VERSION="@vscode/vsce@latest"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [--goos=OS] [--goarch=ARCH] [--out=FILE]

Options:
    --goos=OS      Target OS for the bundled binary (default: host -- $DEFAULT_GOOS)
    --goarch=ARCH  Target arch (default: host -- $DEFAULT_GOARCH)
    --out=FILE     VSIX output path (default: editors/vscode/<name>-<version>.vsix)
    --help         Show this help

Produces a .vsix bundling the memql-lsp binary under the Node-named directory
(bin/<node-platform>-<node-arch>/) the extension resolves at runtime.
EOF
}

function parse_arguments() {
    GOOS_TARGET="$DEFAULT_GOOS"
    GOARCH_TARGET="$DEFAULT_GOARCH"
    OUT=""
    while [[ $# -gt 0 ]]; do
        case $1 in
            --goos=*) GOOS_TARGET="${1#*=}"; shift ;;
            --goarch=*) GOARCH_TARGET="${1#*=}"; shift ;;
            --out=*) OUT="${1#*=}"; shift ;;
            --help) show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function check_prerequisites() {
    command -v go >/dev/null || { echo "ERROR: go is not installed"; exit 1; }
    command -v npm >/dev/null || { echo "ERROR: npm is not installed"; exit 1; }
}

# node_platform maps a Go GOOS to Node's process.platform naming (what the
# extension's resolveServerPath uses to build the bundle directory name).
function node_platform() {
    case "$1" in
        windows) echo "win32" ;;
        *)       echo "$1"    ;; # darwin, linux pass through unchanged
    esac
}

# node_arch maps a Go GOARCH to Node's process.arch naming.
function node_arch() {
    case "$1" in
        amd64) echo "x64"  ;;
        386)   echo "ia32" ;;
        *)     echo "$1"   ;; # arm64 passes through unchanged
    esac
}

function build_binary() {
    local nodeos nodearch bindir binname
    nodeos="$(node_platform "$GOOS_TARGET")"
    nodearch="$(node_arch "$GOARCH_TARGET")"
    bindir="$EXT_DIR/bin/${nodeos}-${nodearch}"
    binname="memql-lsp"
    [[ "$GOOS_TARGET" == "windows" ]] && binname="memql-lsp.exe"
    echo "INFO: building memql-lsp (GOOS=${GOOS_TARGET} GOARCH=${GOARCH_TARGET}) -> bin/${nodeos}-${nodearch}/${binname}"
    mkdir -p "$bindir"
    ( cd "$REPO_ROOT" && GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" CGO_ENABLED=0 \
        go build -o "$bindir/$binname" ./cmd/memql-lsp )
}

function build_extension() {
    echo "INFO: compiling the extension"
    ( cd "$EXT_DIR" && npm ci --no-audit --no-fund && npm run compile )
}

function package_vsix() {
    echo "INFO: packaging the VSIX"
    # --no-dependencies: the extension is bundled (esbuild.js, run by `npm run
    # compile` above), so out/extension.js is already self-contained. Without
    # this flag vsce's own dependency-detection walks the `file:` workspace
    # dependencies (@znasllc-io/memql-sdk-core, @znasllc-io/memql-view-kit) by
    # following their node_modules symlinks OUT of editors/vscode entirely,
    # and absorbs sdk/ts's and sdk/ts-viewkit's own devDependencies
    # (typescript, @types/*) into the VSIX -- which isn't just bloat, it
    # actually fails packaging outright ("invalid relative path:
    # extension/../../sdk/ts/node_modules/..."), because vsce cannot express
    # a path that walks above the extension root inside the VSIX archive.
    local args=(package --no-dependencies)
    [[ -n "$OUT" ]] && args+=(--out "$OUT")
    ( cd "$EXT_DIR" && npx --yes "$VSCE_VERSION" "${args[@]}" )
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    build_binary
    build_extension
    package_vsix
    echo "SUCCESS: VSIX packaged"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
