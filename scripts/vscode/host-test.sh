#!/usr/bin/env bash
set -euo pipefail

# Script: host-test.sh
# Purpose: Run the VS Code extension's Extension Development Host smoke lane
#          (memql#3302) from a clean checkout: build + stage memql-lsp, install
#          the extension's dependencies, and drive a real VS Code under a
#          display.
#
# WHY THIS EXISTS SEPARATELY FROM `npm test`. The fast lane is bare
# `node --test` over the modules that do not import `vscode`; it runs in about a
# second and needs no Electron. This lane downloads and launches a real VS Code
# to assert the things a unit test structurally cannot reach -- that the host's
# Node runtime provides what the code assumes, that the manifest's commands are
# actually registered, that a file watcher fires for a path outside the
# workspace. Keeping them apart is what keeps the fast lane fast.
#
# THE BINARY IS NOT OPTIONAL. Without a resolvable memql-lsp the extension's
# activate() shows its "binary not found" message and returns before
# registering anything at all, so every case downstream of activation would
# fail for a reason that has nothing to do with what it tests. runner.js
# refuses to start in that state; this script is what makes sure it never
# happens.

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXT_DIR="$REPO_ROOT/editors/vscode"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [--skip-deps] [--help]

Options:
    --skip-deps   Skip the npm install / memql-lsp build (inner-loop reruns)
    --help        Show this help

Environment:
    MEMQL_VSCODE_VERSION   VS Code version to download (default: stable)
EOF
}

function parse_arguments() {
    SKIP_DEPS=0
    while [[ $# -gt 0 ]]; do
        case $1 in
            --skip-deps) SKIP_DEPS=1; shift ;;
            --help) show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function check_prerequisites() {
    command -v go >/dev/null || { echo "ERROR: go is not installed"; exit 1; }
    command -v npm >/dev/null || { echo "ERROR: npm is not installed"; exit 1; }
    # Electron needs a display. A headless machine with neither DISPLAY nor
    # xvfb-run cannot run this lane at all, and saying so here is far clearer
    # than the Electron startup crash it would otherwise produce.
    if [[ -z "${DISPLAY:-}" ]] && ! command -v xvfb-run >/dev/null; then
        echo "ERROR: no DISPLAY and no xvfb-run. Electron cannot start."
        echo "       Install xvfb (apt-get install -y xvfb) or run on a machine with a display."
        exit 1
    fi
}

# Stage the binary under the Node-named directory resolveServerPath() looks in
# (process.platform / process.arch), NOT Go's GOOS/GOARCH -- the same mapping
# package.sh makes, and for the same reason.
function build_server_binary() {
    local nodeos nodearch bindir
    case "$(go env GOHOSTOS)" in
        windows) nodeos="win32" ;;
        *)       nodeos="$(go env GOHOSTOS)" ;;
    esac
    case "$(go env GOHOSTARCH)" in
        amd64) nodearch="x64" ;;
        386)   nodearch="ia32" ;;
        *)     nodearch="$(go env GOHOSTARCH)" ;;
    esac
    bindir="$EXT_DIR/bin/${nodeos}-${nodearch}"
    echo "INFO: building memql-lsp -> bin/${nodeos}-${nodearch}/memql-lsp"
    mkdir -p "$bindir"
    ( cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$bindir/memql-lsp" ./cmd/memql-lsp )
}

function install_dependencies() {
    # The file: workspace deps are built by the shared script, which every lane
    # that compiles the extension calls. Keeping a private copy of that recipe
    # here is what let package.sh be written without one (memql#3340).
    bash "$REPO_ROOT/scripts/vscode/deps.sh"
    ( cd "$EXT_DIR" && npm ci --no-audit --no-fund )
}

function run_lane() {
    echo "INFO: running the Extension Development Host smoke lane"
    if [[ -n "${DISPLAY:-}" ]]; then
        ( cd "$EXT_DIR" && npm run test:host )
    else
        # -a picks a free display number; -s sets a screen big enough that the
        # workbench lays out normally (the default 640x480 is small enough to
        # change which views render).
        ( cd "$EXT_DIR" && xvfb-run -a -s "-screen 0 1280x1024x24" npm run test:host )
    fi
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    if [[ "$SKIP_DEPS" -eq 0 ]]; then
        build_server_binary
        install_dependencies
    fi
    run_lane
    echo "SUCCESS: host smoke lane passed"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
