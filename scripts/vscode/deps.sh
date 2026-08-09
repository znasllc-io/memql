#!/usr/bin/env bash
set -euo pipefail

# Script: deps.sh
# Purpose: Build the `file:` workspace packages the VS Code extension compiles
#          against, so any lane that compiles the extension is self-sufficient
#          from a clean checkout.
#
# WHY THIS IS A SEPARATE SCRIPT (znasllc-io/memql#3340). The extension depends
# on sdk/ts and sdk/ts-viewkit through `file:` specs. Both publish their types
# from a `dist/` that is .gitignore'd and produced by `npm run build`, and
# `npm ci` inside editors/vscode only creates the symlinks -- it does NOT build
# what they point at. So every lane that compiles the extension has to run this
# first, or `tsc` cannot resolve either package (TS2307) and reports a shower of
# downstream implicit-`any` errors behind it.
#
# That rule used to live in three hand-maintained copies -- the Makefile's
# `vscode-deps` target, host-test.sh, and CI -- and package.sh, which is what
# BOTH `make vscode-install` and `make vscode-package` run, was missed. The
# entire developer-facing "install this extension into my editor" path failed
# from any checkout that had not already built the SDKs for some other reason,
# while CI stayed green because its package step happened to run after a step
# that had already built them. One copy, called by everyone, is the fix; the
# guard that keeps it that way is cmd/memql-lsp/vscodeworkspacedeps_test.go.
#
# Idempotent and safe to re-run: that is what lets every consumer call it
# unconditionally instead of reasoning about whether someone else already did.

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [--help]

Build the file: workspace packages the VS Code extension compiles against
(sdk/ts and sdk/ts-viewkit). Run by every lane that compiles the extension:
\`make vscode-deps\`, scripts/vscode/package.sh, scripts/vscode/host-test.sh.

Idempotent -- re-running it is cheap and always safe.
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --help) show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1" >&2; show_help; exit 1 ;;
        esac
    done
}

function check_prerequisites() {
    command -v npm >/dev/null || { echo "ERROR: npm is not installed" >&2; exit 1; }
}

# build_workspace_deps builds each `file:` dependency of editors/vscode.
#
# Both use `npm ci`: both commit a package-lock.json, so both get the
# reproducible, integrity-pinned install. sdk/ts used `npm install` until
# memql#3344 -- not by choice, but because its lockfile had never been
# committed even though .gitignore already exempted it from the global
# package-lock.json ignore. Committing the file removed the reason for the
# split.
#
# Written as one explicit line per package rather than a loop over a table: with
# two entries a table is more machinery than it saves, and an explicit line
# makes "which directory gets built, and how" greppable -- including by the
# conformance guard, which asserts every `file:` dependency named in
# editors/vscode/package.json is actually built here. Adding a third workspace
# package means adding a line here; that guard fails until you do.
function build_workspace_deps() {
    echo "INFO: building the file: workspace dependencies the extension compiles against"
    ( cd "$REPO_ROOT/sdk/ts" && npm ci --no-audit --no-fund && npm run build )
    ( cd "$REPO_ROOT/sdk/ts-viewkit" && npm ci --no-audit --no-fund && npm run build )
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    build_workspace_deps
    echo "SUCCESS: workspace dependencies built"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
