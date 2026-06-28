#!/usr/bin/env bash
#
# scripts/deploy/cockpit.sh
# =========================
#
# Thin launcher that resolves the `memql-cockpit` binary and execs it.
#
# This is the convergence point for I16 (znasllc-io/memql#2227, epic #2212):
# `make deploy` (the break-glass imperative path) stops orchestrating deploy
# scripts directly and instead shells into the cockpit, which embeds the engine
# automation runtime, loads the deployment bundle, and runs the pinned
# `deployEngineCluster` automation (role-gated + audited + version-pinned). See
# DEVOPS_DSL_BUNDLE_HANDOFF.md "Execution model".
#
# This script only RESOLVES + EXECS the binary -- it makes no deploy decisions
# (all branching lives in the cockpit / the bundle automation). The cockpit
# prints its own human-readable + honestly-owner-gated outcome.
#
# Binary resolution order (no fallback to the old deploy scripts -- the cockpit
# is the path):
#   1. $COCKPIT_BIN, if set and executable.
#   2. `memql-cockpit` on $PATH.
#   3. Built from the sibling `../memql-cockpit` repo via `make cockpit`
#      (-> ../memql-cockpit/bin/memql-cockpit); built on demand if absent.
#   4. Otherwise: fail loud with bootstrap instructions.
#
# Usage:
#   scripts/deploy/cockpit.sh deploy --env=<env> [--ref=<ver>] [--dry-run] ...
#
# Exit codes: passes through the cockpit's exit code (0 ok/invoked, 1
# error/denied/owner-gated-but-honest, 2 usage). 70 = cockpit binary could not
# be resolved or built.
set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Sibling cockpit repo (go.mod `replace => ../memql`), relative to the engine
# repo root: /Users/.../projects/{memql,memql-cockpit}.
COCKPIT_REPO="${MEMQL_COCKPIT_REPO:-${REPO_ROOT}/../memql-cockpit}"
COCKPIT_REPO_BIN_REL="bin/memql-cockpit"

#=============================================================================
# FUNCTIONS
#=============================================================================

function log()  { printf '%s\n'        "$*" >&2; }
function info() { printf 'INFO:  %s\n' "$*" >&2; }
function error(){ printf 'ERROR: %s\n' "$*" >&2; }

function bootstrap_instructions() {
    cat >&2 <<EOF
ERROR: could not resolve the 'memql-cockpit' binary.

'make deploy' delegates to the cockpit (the control plane). Make it resolvable
by ONE of:

  1. Install it on PATH (operator machines / CI -- see I17, memql#2228):
       go install github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit@latest
  2. Build it from the sibling repo checkout (expected at: ${COCKPIT_REPO}):
       git -C "${COCKPIT_REPO}" pull   # if not present, clone it next to memql/
       make -C "${COCKPIT_REPO}" cockpit
  3. Point at an existing binary explicitly:
       make deploy ENV=<env> COCKPIT_BIN=/path/to/memql-cockpit
EOF
}

# Resolve the cockpit binary path, building from the sibling repo if needed.
# Prints the resolved path to stdout; returns non-zero if unresolvable.
function resolve_cockpit_bin() {
    # 1. Explicit override.
    if [ -n "${COCKPIT_BIN:-}" ]; then
        if [ -x "${COCKPIT_BIN}" ]; then
            printf '%s\n' "${COCKPIT_BIN}"
            return 0
        fi
        error "COCKPIT_BIN is set but not executable: ${COCKPIT_BIN}"
        return 1
    fi

    # 2. On PATH.
    if command -v memql-cockpit >/dev/null 2>&1; then
        command -v memql-cockpit
        return 0
    fi

    # 3. Sibling repo: use an existing build, else build it on demand.
    local repo_bin="${COCKPIT_REPO}/${COCKPIT_REPO_BIN_REL}"
    if [ -x "${repo_bin}" ]; then
        printf '%s\n' "${repo_bin}"
        return 0
    fi
    if [ -d "${COCKPIT_REPO}" ] && [ -f "${COCKPIT_REPO}/Makefile" ]; then
        info "memql-cockpit not on PATH; building it from ${COCKPIT_REPO} ..."
        if make -C "${COCKPIT_REPO}" cockpit >&2 && [ -x "${repo_bin}" ]; then
            printf '%s\n' "${repo_bin}"
            return 0
        fi
        error "build of memql-cockpit in ${COCKPIT_REPO} did not produce ${COCKPIT_REPO_BIN_REL}"
        return 1
    fi

    return 1
}

function main() {
    local bin
    if ! bin="$(resolve_cockpit_bin)"; then
        bootstrap_instructions
        exit 70
    fi
    info "delegating to cockpit: ${bin} $*"
    exec "${bin}" "$@"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
