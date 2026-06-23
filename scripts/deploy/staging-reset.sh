#!/usr/bin/env bash
#
# scripts/deploy/staging-reset.sh
# ===============================
#
# ONE idempotent operator entrypoint to RESET staging to a fresh, *fully
# usable* state (znasllc-io/memql#1524, the operationalization umbrella; epic
# #1518). It wraps the destructive, auth-coherent DB reset (#1500 + #1522) and
# then VERIFIES the cluster came back green -- login works, JWKS coherent, the
# mesh reconnected -- so a "fresh start" is never left in the half-broken auth
# state that took a manual reseal + mesh roll to recover on 2026-06-16.
#
# What it does
# ------------
#   1. Read-only auth-coherence PRE-FLIGHT (the shared identity signing seed,
#      #1515) -- belt-and-suspenders with staging-db-reset.sh's own pre-flight,
#      surfaced up front so an operator sees the blocker before any prompt.
#   2. Delegate to staging-db-reset.sh: scale down, wipe schema, re-migrate,
#      bring up IDENTITY-FIRST so node tokens re-mint against a ready issuer
#      (#1521), and verify identity Available + JWKS served (#1522).
#   3. Post-reset FUNCTIONAL verification (post-deploy-gate.sh --gate-only,
#      #1519): not just "identity is up" but the cross-node BFF->agent auth path
#      actually works -- the real meaning of "the reset came up usable".
#
# This NEVER deploys a new version; cutting/rolling a release is the sibling
# entrypoint staging-release.sh. It is staging-only and DESTRUCTIVE (every row
# in the staging DB is gone); the underlying reset requires a typed confirmation
# unless --yes is passed.
#
# Per the Skills+Scripts convention (CLAUDE.md): function-based, main() at the
# bottom, --help, and a --dry-run that plans and mutates NOTHING.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_ENV="staging"
DEFAULT_NS="memql"
SECRET_NAME="memql-secrets"
IDENTITY_DEPLOY="identity"
GENESIS_KEY="MEMQL_GENESIS_B64"
SIGNING_KEY="MEMQL_IDENTITY_SIGNING_KEY_B64"
EPHEMERAL_OPT_IN="MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESET_SCRIPT="$SCRIPT_DIR/staging-db-reset.sh"
GATE_SCRIPT="$SCRIPT_DIR/post-deploy-gate.sh"

#=============================================================================
# FUNCTIONS
#=============================================================================

function info() { echo "INFO: $*"; }
function warn() { echo "WARNING: $*" >&2; }
function err()  { echo "ERROR: $*" >&2; }
function plan() { echo "  [plan] $*"; }
function section() { echo ""; echo "===== $* ====="; }

function show_help() {
    cat <<EOF
Usage: $0 [options]

ONE idempotent operator command to reset staging to a FRESH, FULLY-USABLE state:
auth-coherent DB wipe (#1500/#1522) + a post-reset functional verification (#1519).

Options:
    --env=ENV     Target environment (default: $DEFAULT_ENV). Staging-only by design.
    --yes         Skip the interactive typed confirmation (passthrough to staging-db-reset.sh).
    --no-verify   Skip the post-reset functional gate (still runs the reset's own JWKS verify).
    --dry-run     Print the full plan and mutate NOTHING (no cluster required).
    --help        Show this help.

Examples:
    $0 --dry-run            # preview the reset + verify plan, change nothing
    $0                      # wipe staging (asks you to type 'reset staging'), then verify
    $0 --yes                # wipe non-interactively, then verify

DESTRUCTIVE: every row in the staging DB is gone. Sibling: staging-release.sh
cuts/rolls a release (never wipes).
EOF
}

function parse_arguments() {
    ENV="$DEFAULT_ENV"
    NS="$DEFAULT_NS"
    YES=false
    NO_VERIFY=false
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)    ENV="${1#*=}"; shift ;;
            --yes)      YES=true; shift ;;
            --no-verify) NO_VERIFY=true; shift ;;
            --dry-run)  DRY_RUN=true; shift ;;
            --help|-h)  show_help; exit 0 ;;
            *) err "unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function validate_arguments() {
    if [[ "$ENV" != "staging" ]]; then
        err "--env must be 'staging' (got '${ENV:-<empty>}'). This entrypoint is staging-only and refuses prod."
        exit 1
    fi
    command -v kubectl >/dev/null 2>&1 || { err "kubectl is required"; exit 1; }
    [[ -f "$RESET_SCRIPT" ]] || { err "required script not found: $RESET_SCRIPT"; exit 1; }
    [[ -f "$GATE_SCRIPT"  ]] || { err "required script not found: $GATE_SCRIPT";  exit 1; }
}

# Read-only auth-coherence pre-flight (the shared signing seed, #1515). The
# underlying reset re-checks this before it wipes; surfacing it here means the
# operator sees a missing-seed blocker BEFORE the confirmation prompt.
function preflight_coherence() {
    section "Pre-flight: auth coherence (shared identity signing seed, #1515)"
    if [[ "$DRY_RUN" == true ]]; then
        plan "kubectl get secret $SECRET_NAME -- assert it carries $GENESIS_KEY or $SIGNING_KEY"
        plan "kubectl get deploy $IDENTITY_DEPLOY -- assert NOT $EPHEMERAL_OPT_IN=true at >=2 replicas"
        return 0
    fi
    if ! kubectl -n "$NS" get secret "$SECRET_NAME" >/dev/null 2>&1; then
        err "Secret '$SECRET_NAME' not found in '$NS' -- a reset without the shared seed leaves JWKS unrecoverable."
        err "seal + apply the genesis envelope first (scripts/secrets/reseal-genesis.sh); see #1515/#1522."
        exit 1
    fi
    local secret_keys=""
    # shellcheck disable=SC2016
    secret_keys="$(kubectl -n "$NS" get secret "$SECRET_NAME" \
        -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}' 2>/dev/null || true)"
    if ! printf '%s\n' "$secret_keys" | grep -qxE "$GENESIS_KEY|$SIGNING_KEY"; then
        err "Secret '$SECRET_NAME' carries neither $GENESIS_KEY nor $SIGNING_KEY -- reseal before resetting (#1515)."
        exit 1
    fi
    info "  seed source present in $SECRET_NAME -- the post-reset JWKS will be coherent."
}

# Delegate the destructive, auth-coherent reset (#1500/#1522).
function run_reset() {
    section "Auth-coherent DB reset -> staging-db-reset.sh (#1500/#1522)"
    local -a args=( "--env=$ENV" )
    [[ "$DRY_RUN" == true ]] && args+=( "--dry-run" )
    [[ "$YES" == true ]] && args+=( "--yes" )
    if [[ "$DRY_RUN" == true ]]; then
        plan "bash $RESET_SCRIPT ${args[*]}    # scale down -> wipe -> migrate -> identity-first bring-up -> JWKS verify"
        return 0
    fi
    info "delegating to: bash $RESET_SCRIPT ${args[*]}"
    if ! bash "$RESET_SCRIPT" "${args[@]}"; then
        err "the auth-coherent DB reset FAILED -- staging may be in a partial state. See the output above."
        exit 1
    fi
}

# Post-reset functional verification: identity being Available is necessary but
# not sufficient. The gate (#1519) proves the cross-node BFF->agent auth path
# works -- "the reset came up usable", per the #1522 acceptance.
function verify_usable() {
    if [[ "$NO_VERIFY" == true ]]; then
        info "post-reset functional gate skipped (--no-verify); the reset's own JWKS verify still ran."
        return 0
    fi
    section "Post-reset functional verification (#1519: JWKS coherent + BFF->agent auth)"
    local -a gate_args=( "--gate-only" "--env=$ENV" )
    if [[ "$DRY_RUN" == true ]]; then
        plan "bash $GATE_SCRIPT ${gate_args[*]}    # validate the reset cluster is functionally usable"
        return 0
    fi
    if ! bash "$GATE_SCRIPT" "${gate_args[@]}"; then
        err "the reset completed but the post-reset functional gate FAILED -- staging is NOT usable yet."
        err "auth is incoherent or the mesh did not reconnect. See the output above; do NOT report the reset clean."
        exit 1
    fi
}

function main() {
    parse_arguments "$@"
    validate_arguments

    echo "========================================="
    echo "memQL staging RESET (auth-coherent, verified)"
    echo "  env=$ENV  dry-run=$DRY_RUN  verify=$([ "$NO_VERIFY" = true ] && echo false || echo true)"
    echo "========================================="

    preflight_coherence
    run_reset
    verify_usable

    echo ""
    if [[ "$DRY_RUN" == true ]]; then
        echo "RESULT: dry-run plan only -- nothing changed."
    else
        echo "RESULT: staging reset complete -- fresh, auth-coherent, and functionally verified."
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
