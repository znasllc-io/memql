#!/usr/bin/env bash
set -euo pipefail

# Script: package.sh
# Purpose: Build the memql-lsp binary for a target platform, stage it into the
#          VS Code extension, compile the extension, and produce a .vsix.
#
# The offline LSP embeds the engine, so the binary is bundled per platform and
# the extension resolves it at bin/<GOOS>-<GOARCH>/memql-lsp. darwin-arm64 is
# the standardized dev target and is built by default.

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXT_DIR="$REPO_ROOT/editors/vscode"
DEFAULT_GOOS="darwin"
DEFAULT_GOARCH="arm64"
VSCE_VERSION="@vscode/vsce@latest"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [--goos=OS] [--goarch=ARCH] [--out=FILE]

Options:
    --goos=OS      Target OS for the bundled binary (default: $DEFAULT_GOOS)
    --goarch=ARCH  Target arch (default: $DEFAULT_GOARCH)
    --out=FILE     VSIX output path (default: editors/vscode/<name>-<version>.vsix)
    --help         Show this help

Produces a .vsix bundling bin/<goos>-<goarch>/memql-lsp.
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

function build_binary() {
    local bindir="$EXT_DIR/bin/${GOOS_TARGET}-${GOARCH_TARGET}"
    local binname="memql-lsp"
    [[ "$GOOS_TARGET" == "windows" ]] && binname="memql-lsp.exe"
    echo "INFO: building memql-lsp for ${GOOS_TARGET}/${GOARCH_TARGET}"
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
    local args=(package)
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
