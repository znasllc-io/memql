#!/usr/bin/env bash
#
# scripts/os/build.sh
# =======================
#
# The MemQL OS shell's install / typecheck / test / build / clean cycle
# (memql#4705).
#
# WHY A SCRIPT AND NOT FIVE MAKEFILE RECIPES
#
# The OS consumes sdk/ts as a `file:` dependency, and a `file:` dependency is a
# SOURCE tree, not a published tarball -- npm links the directory, so
# `import ... from "@znasllc-io/memql-sdk-core"` resolves through that
# package's own `exports` map to ./dist. If dist is absent the OS's typecheck,
# its tests and `vite build` all fail with a resolution error that names the
# package but not the cause. (It is also the failure a stale dist gives you,
# which is subtler: it resolves, and reports a missing export for whatever the
# SDK has generated since -- so a mismatch reads as a defect in the CALLER.)
#
# So EVERY OS command has the same prerequisite: build the workspace package
# first. Encoding that as `cd sdk/ts && npm install && npm run build`
# repeated across five Makefile recipes is how the sixth one gets it wrong; and
# encoding it as an npm script inside clients/os would only work when
# invoked from that directory. One script, called from one-line Makefile
# targets, per the convention in root CLAUDE.md.
#
# Every install here is `npm ci` -- the reproducible, integrity-pinned path
# (OSSF Scorecard PinnedDependenciesID). All three package directories commit a
# package-lock.json; sdk/ts used `npm install` until its lockfile landed in
# memql#3344, and scripts/vscode/deps.sh dropped the split then -- this script
# was the straggler. A missing lockfile is a broken checkout, not a bootstrap
# case, so it fails loudly instead of degrading to an unpinned `npm install`.
#
# Usage:
#   scripts/os/build.sh deps        # build the file: dependencies only
#   scripts/os/build.sh install     # deps + install the OS shell's own deps
#   scripts/os/build.sh typecheck   # install + tsc -b
#   scripts/os/build.sh test        # install + vitest run
#   scripts/os/build.sh build       # install + tsc -b + vite build -> dist/,
#                                   # plus dist/memql-bundle.json (its sha256s)
#   scripts/os/build.sh clean       # remove dist + the build caches
#
# Idempotent: every command is safe to re-run.

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

OS_DIR="${REPO_ROOT}/clients/os"
SDK_CORE_DIR="${REPO_ROOT}/sdk/ts"

# --no-audit --no-fund everywhere: both make network calls that are pure noise
# in CI and add seconds to every install.
NPM_FLAGS=(--no-audit --no-fund)

# `npm ci` is the one network operation in this script, and npm does not retry
# a registry read that times out partway through a tarball. The local
# installer runs this inside the edge image build, where one dropped read used
# to fail the whole cluster install. `npm ci` starts from a clean node_modules,
# so it is safe to repeat; nothing that compiles, type-checks or tests is.
NPM_CI_ATTEMPTS="${MEMQL_NPM_CI_ATTEMPTS:-3}"
NPM_RETRY_DELAY_SECONDS="${MEMQL_NPM_RETRY_DELAY_SECONDS:-10}"

#=============================================================================
# FUNCTIONS
#=============================================================================

function info() { echo "INFO: $*" >&2; }

# npm_ci DIR -- `npm ci` in DIR, repeated up to NPM_CI_ATTEMPTS times with a
# growing pause. Says so on stderr when it retries, because a download that
# needed a second attempt is a signal about the network, not noise.
function npm_ci() {
    local dir="$1" attempt=1 status
    while true; do
        status=0
        ( cd "${dir}" && npm ci "${NPM_FLAGS[@]}" ) || status=$?
        if (( status == 0 )); then
            if (( attempt > 1 )); then
                info "npm ci in ${dir#"${REPO_ROOT}"/} succeeded on attempt ${attempt} of ${NPM_CI_ATTEMPTS}."
            fi
            return 0
        fi
        if (( attempt >= NPM_CI_ATTEMPTS )); then
            echo "ERROR: npm ci in ${dir#"${REPO_ROOT}"/} failed after ${attempt} attempt(s) (exit ${status})." >&2
            return "${status}"
        fi
        echo "WARN: npm ci in ${dir#"${REPO_ROOT}"/} failed (exit ${status}); retrying in $(( NPM_RETRY_DELAY_SECONDS * attempt ))s (attempt $(( attempt + 1 )) of ${NPM_CI_ATTEMPTS})." >&2
        sleep "$(( NPM_RETRY_DELAY_SECONDS * attempt ))"
        attempt=$(( attempt + 1 ))
    done
}

function check_node() {
    if ! command -v npm >/dev/null 2>&1; then
        echo "ERROR: npm not found on PATH." >&2
        echo "       The OS shell is a Node application; the Go build does not need it," >&2
        echo "       which is why nothing else in this repo asks for Node." >&2
        exit 4
    fi
}


# install_os installs the OS shell's own dependencies with `npm ci`, always.
# The lockfile is committed, so a tree without it is missing a tracked file --
# say so and stop rather than silently falling back to an unpinned
# `npm install` that would resolve whatever the registry serves today.
function install_os() {
    if [[ ! -f "${OS_DIR}/package-lock.json" ]]; then
        echo "ERROR: ${OS_DIR}/package-lock.json not found." >&2
        echo "       The OS lockfile is committed; restore it (git checkout -- clients/os/package-lock.json)." >&2
        exit 4
    fi
    info "Installing OS dependencies (npm ci)..."
    npm_ci "${OS_DIR}"
}

# build_workspace_deps builds the `file:` dependency so its dist/ exists
# before anything resolves against it: a file: dep is a linked source tree, so
# the OS's typecheck, tests and vite build all resolve
# @znasllc-io/memql-sdk-core through sdk/ts's exports map into ./dist.
# `npm ci` because the lockfile is committed; idempotent, and a no-op rebuild
# when the SPA stage of the Docker build already built it.
function build_workspace_deps() {
    info "Building @znasllc-io/memql-sdk-core (sdk/ts)..."
    npm_ci "${SDK_CORE_DIR}"
    ( cd "${SDK_CORE_DIR}" && npm run build )
}

function run_install() {
    check_node
    build_workspace_deps
    install_os
}

function run_typecheck() {
    run_install
    info "Typechecking the OS shell..."
    ( cd "${OS_DIR}" && npm run typecheck )
}

function run_test() {
    run_install
    info "Running the OS test suite..."
    ( cd "${OS_DIR}" && npm test )
}

# run_build builds the bundle, then writes memql-bundle.json beside it: the
# sha256 of every file in dist/ (bundle-manifest.mjs). The edge image carries
# it at /app/os/memql-bundle.json, which is what verify-rollout (epic
# memql#5480) compares with the bytes the public front door serves. It is
# written HERE, as the last step of the build, so the image stage, `make
# os-build` and the OS lane all produce it the one way; a build that cannot
# write it fails rather than shipping a bundle nothing can be checked against.
function run_build() {
    run_install
    info "Building the OS bundle..."
    ( cd "${OS_DIR}" && npm run build )
    info "Writing the bundle manifest (memql-bundle.json)..."
    node "${SCRIPT_DIR}/bundle-manifest.mjs" "${OS_DIR}/dist"
    info "Bundle written to clients/os/dist."
}

# clean removes generated output only. node_modules is deliberately left
# alone: re-downloading it is minutes, and nothing in it is ever stale in a
# way `npm ci` does not fix.
function run_clean() {
    info "Removing the OS build output..."
    rm -rf "${OS_DIR}/dist" "${OS_DIR}/node_modules/.vite" "${OS_DIR}/node_modules/.tmp"
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
