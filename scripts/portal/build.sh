#!/usr/bin/env bash
#
# scripts/portal/build.sh
# =======================
#
# The memQL Portal's install / typecheck / test / build / clean cycle
# (memql#3314).
#
# WHY A SCRIPT AND NOT FIVE MAKEFILE RECIPES
#
# The portal consumes sdk/ts and sdk/ts-viewkit as `file:` dependencies, and a
# `file:` dependency is a SOURCE tree, not a published tarball -- npm links the
# directory, so `import ... from "@znasllc-io/memql-sdk-core"` resolves through
# that package's own `exports` map to ./dist. If dist is absent the portal's
# typecheck, its tests and `vite build` all fail with a resolution error that
# names the package but not the cause.
#
# So EVERY portal command has the same prerequisite: build the two workspace
# packages first. Encoding that as `cd sdk/ts && npm install && npm run build`
# repeated across five Makefile recipes is how the sixth one gets it wrong; and
# encoding it as an npm script inside clients/portal would only work when
# invoked from that directory. One script, called from one-line Makefile
# targets, per the convention in root CLAUDE.md.
#
# The two packages install DIFFERENTLY and that is not an inconsistency to fix
# here: sdk/ts does not commit a lockfile (so `npm ci` fails), sdk/ts-viewkit
# does (so it gets the reproducible `npm ci`). Same split the vscode-deps
# target makes, for the same reason.
#
# Usage:
#   scripts/portal/build.sh deps        # build the file: dependencies only
#   scripts/portal/build.sh install     # deps + install the portal's own deps
#   scripts/portal/build.sh typecheck   # install + tsc -b
#   scripts/portal/build.sh test        # install + vitest run
#   scripts/portal/build.sh build       # install + tsc -b + vite build -> dist/
#   scripts/portal/build.sh clean       # remove dist + the build caches
#
# Idempotent: every command is safe to re-run.

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

PORTAL_DIR="${REPO_ROOT}/clients/portal"
SDK_CORE_DIR="${REPO_ROOT}/sdk/ts"
VIEW_KIT_DIR="${REPO_ROOT}/sdk/ts-viewkit"

# --no-audit --no-fund everywhere: both make network calls that are pure noise
# in CI and add seconds to every install.
NPM_FLAGS=(--no-audit --no-fund)

#=============================================================================
# FUNCTIONS
#=============================================================================

function info() { echo "INFO: $*" >&2; }

function check_node() {
    if ! command -v npm >/dev/null 2>&1; then
        echo "ERROR: npm not found on PATH." >&2
        echo "       The portal is a Node application; the Go build does not need it," >&2
        echo "       which is why nothing else in this repo asks for Node." >&2
        exit 4
    fi
}

# build_workspace_deps builds the two `file:` dependencies so their dist/
# exists before anything resolves against it.
function build_workspace_deps() {
    info "Building @znasllc-io/memql-sdk-core (sdk/ts)..."
    ( cd "${SDK_CORE_DIR}" && npm install "${NPM_FLAGS[@]}" && npm run build )

    info "Building @znasllc-io/memql-view-kit (sdk/ts-viewkit)..."
    ( cd "${VIEW_KIT_DIR}" && npm ci "${NPM_FLAGS[@]}" && npm run build )
}

# install_portal installs the portal's own dependencies. `npm ci` when the
# lockfile is present (the committed, reproducible path) and `npm install`
# otherwise, so a lockfile-less working tree still bootstraps rather than
# failing with npm's "can only install with an existing package-lock.json".
function install_portal() {
    if [[ -f "${PORTAL_DIR}/package-lock.json" ]]; then
        info "Installing portal dependencies (npm ci)..."
        ( cd "${PORTAL_DIR}" && npm ci "${NPM_FLAGS[@]}" )
    else
        info "Installing portal dependencies (npm install -- no lockfile present)..."
        ( cd "${PORTAL_DIR}" && npm install "${NPM_FLAGS[@]}" )
    fi
}

function run_install() {
    check_node
    build_workspace_deps
    install_portal
}

function run_typecheck() {
    run_install
    info "Typechecking the portal..."
    ( cd "${PORTAL_DIR}" && npm run typecheck )
}

function run_test() {
    run_install
    info "Running the portal test suite..."
    ( cd "${PORTAL_DIR}" && npm test )
}

function run_build() {
    run_install
    info "Building the portal bundle..."
    ( cd "${PORTAL_DIR}" && npm run build )
    info "Bundle written to clients/portal/dist."
}

# clean removes generated output only. node_modules is deliberately left
# alone: re-downloading it is minutes, and nothing in it is ever stale in a
# way `npm ci` does not fix.
function run_clean() {
    info "Removing the portal build output..."
    rm -rf "${PORTAL_DIR}/dist" "${PORTAL_DIR}/node_modules/.vite" "${PORTAL_DIR}/node_modules/.tmp"
}

function usage() {
    echo "usage: $0 {deps|install|typecheck|test|build|clean}" >&2
    exit 2
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    local command="${1:-}"
    case "${command}" in
        deps)      check_node; build_workspace_deps ;;
        install)   run_install ;;
        typecheck) run_typecheck ;;
        test)      run_test ;;
        build)     run_build ;;
        clean)     run_clean ;;
        *)         usage ;;
    esac
}

main "$@"
