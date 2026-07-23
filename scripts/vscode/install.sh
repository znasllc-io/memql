#!/usr/bin/env bash
set -euo pipefail

# Script: install.sh
# Purpose: Build the MemQL VS Code extension and (re)install it into a local VS
#          Code, so a developer who changed the LSP server or the extension can
#          refresh their editor with ONE command.
#
# The language intelligence lives in the bundled memql-lsp binary, so "update my
# editor" means rebuild + reinstall + reload. This wraps that loop: it packages a
# fresh .vsix (via package.sh) and installs it with `code --install-extension
# --force`, which overwrites the currently-installed build. Re-run it after every
# local change; pass --no-build to install a .vsix you already packaged.

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXT_DIR="$REPO_ROOT/editors/vscode"
PACKAGE_SH="$REPO_ROOT/scripts/vscode/package.sh"
DEFAULT_EDITOR_CMD="code"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Build the MemQL VS Code extension and (re)install it into a local editor.

Options:
    --editor-cmd=CMD  Editor CLI to install into (default: $DEFAULT_EDITOR_CMD;
                      e.g. code-insiders, cursor, codium)
    --goos=OS         Target OS for the bundled binary (passed to package.sh)
    --goarch=ARCH     Target arch (passed to package.sh)
    --vsix=FILE       Install this .vsix instead of the one package.sh produces
    --no-build        Skip the rebuild; install the existing .vsix
    --help            Show this help

Examples:
    $0                       # rebuild + reinstall into VS Code
    $0 --editor-cmd=cursor   # ... into Cursor
    $0 --no-build            # reinstall the last-built .vsix
EOF
}

function parse_arguments() {
    EDITOR_CMD="$DEFAULT_EDITOR_CMD"
    GOOS_TARGET=""
    GOARCH_TARGET=""
    VSIX=""
    NO_BUILD=false
    while [[ $# -gt 0 ]]; do
        case $1 in
            --editor-cmd=*) EDITOR_CMD="${1#*=}"; shift ;;
            --goos=*) GOOS_TARGET="${1#*=}"; shift ;;
            --goarch=*) GOARCH_TARGET="${1#*=}"; shift ;;
            # Normalize a relative --vsix to absolute: package.sh writes --out
            # relative to editors/vscode/ (it cd's there), but install runs from
            # the invoking CWD, so a relative path would resolve differently in
            # the two steps.
            --vsix=*) VSIX="${1#*=}"; [[ -n "$VSIX" && "$VSIX" != /* ]] && VSIX="$PWD/$VSIX"; shift ;;
            --no-build) NO_BUILD=true; shift ;;
            --help) show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function check_prerequisites() {
    if ! command -v "$EDITOR_CMD" >/dev/null; then
        cat >&2 <<EOF
ERROR: editor CLI '$EDITOR_CMD' is not on your PATH.
       In VS Code, run "Shell Command: Install 'code' command in PATH" from the
       command palette (Cmd/Ctrl+Shift+P), or pass --editor-cmd=<your-editor>.
EOF
        exit 1
    fi
}

# ext_version reads the extension version from package.json (no node/jq needed).
function ext_version() {
    sed -n -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$EXT_DIR/package.json" | head -1
}

# resolve_vsix determines which .vsix to install: an explicit --vsix, else the
# path package.sh writes by default (editors/vscode/memql-<version>.vsix).
function resolve_vsix() {
    if [[ -n "$VSIX" ]]; then
        echo "$VSIX"
        return
    fi
    echo "$EXT_DIR/memql-$(ext_version).vsix"
}

function build_vsix() {
    if [[ "$NO_BUILD" == true ]]; then
        echo "INFO: --no-build; skipping rebuild"
        return
    fi
    local args=()
    [[ -n "$GOOS_TARGET" ]] && args+=("--goos=$GOOS_TARGET")
    [[ -n "$GOARCH_TARGET" ]] && args+=("--goarch=$GOARCH_TARGET")
    [[ -n "$VSIX" ]] && args+=("--out=$VSIX")
    echo "INFO: building the extension (bundles memql-lsp for this host by default)"
    # ${args[@]+...} guards an empty array under `set -u` on bash 3.2 (macOS).
    bash "$PACKAGE_SH" ${args[@]+"${args[@]}"}
}

function install_vsix() {
    local vsix="$1"
    if [[ ! -f "$vsix" ]]; then
        echo "ERROR: vsix not found: $vsix" >&2
        [[ "$NO_BUILD" == true ]] && echo "       run without --no-build to package it first." >&2
        exit 1
    fi
    echo "INFO: installing $vsix into '$EDITOR_CMD'"
    "$EDITOR_CMD" --install-extension "$vsix" --force
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    build_vsix
    local vsix; vsix="$(resolve_vsix)"
    install_vsix "$vsix"
    cat <<EOF
SUCCESS: installed $(basename "$vsix") into '$EDITOR_CMD'.
         Reload the editor to pick up the new language server:
         run "Developer: Reload Window" (Cmd/Ctrl+Shift+P), or restart it.
EOF
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
