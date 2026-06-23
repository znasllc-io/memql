#!/usr/bin/env bash
#
# scripts/deploy/staging-release.sh
# =================================
#
# ONE idempotent, fail-loud operator entrypoint to take STAGING to a
# *validated* release -- and to RECOVER a release that was reported live but is
# actually broken (znasllc-io/memql#1524, the operationalization umbrella for
# the 2026-06-16 incident; epic #1518).
#
# Why this exists
# ---------------
# The 0.9.60 outage was not bad luck -- it was an un-scripted, multi-step
# operator dance with two silent failure modes that a digest-drift check could
# not see (#1519):
#   * the bff blue/green Rollout parked at BlueGreenPause and was never
#     PROMOTED, so bff-active kept selecting the OLD color (no agent peer ->
#     `siSuggest: no connected agent node available`); and
#   * identity JWKS diverged across replicas (no shared signing seed, #1515),
#     so ~half of all token verifications failed.
# Recovery took ~5 ad-hoc /tmp scripts. The durable fixes landed in #1515
# (shared seed), #1519/#1520 (functional gate + bff auto-promote folded into
# aks-deploy.sh), #1521 (mesh re-mint), and #1522 (auth-coherent reset). This
# script is the single, re-runnable command that BAKES THEM IN so the dance --
# and its recovery -- is never improvised again.
#
# Two modes
# ---------
#   (default) FULL release: a read-only auth-coherence PRE-FLIGHT (the seed that
#     was missing in the incident), then delegate to the deploy engine
#     `aks-deploy.sh` -- which builds + pushes, applies the digest-pinned
#     overlay identity-first, asserts live drift, PROMOTES the bff Rollout
#     (#1520), runs the FUNCTIONAL post-deploy gate (#1519: bff promoted + JWKS
#     coherent + BFF->agent auth), smoke-tests the front door, and records the
#     validated version. Re-running is idempotent (immutable tags, declarative
#     apply, promote handles already-promoted, the gate re-validates).
#
#   --verify  RECOVERY / re-validate ONLY (no build, no apply): run just the bff
#     PROMOTE + the functional gate (post-deploy-gate.sh) and the smoke gate
#     against whatever is already live. THIS IS THE 2026-06-16 RECOVERY AS ONE
#     COMMAND: a release that went "green" on drift but parked unpromoted /
#     JWKS-incoherent is healed by `staging-release.sh --verify` -- it promotes
#     the stuck bff and fails LOUDLY if auth is still incoherent, instead of an
#     operator hand-running kubectl argo rollouts promote + a manual probe.
#
# Reset (wipe to a fresh, auth-coherent staging) is the sibling entrypoint
# `staging-reset.sh` (#1500 + #1522). This script NEVER wipes data.
#
# Per the Skills+Scripts convention (CLAUDE.md): pure function-based structure,
# one function per responsibility, main() at the bottom. Supports --help and a
# --dry-run that prints the full plan and mutates NOTHING (no cluster needed).

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_ENV="staging"
DEFAULT_NS="memql"
EXPECTED_CONTEXT_SUBSTR="staging"   # wrong-cluster guard (mirrors staging-db-reset.sh)
SECRET_NAME="memql-secrets"
IDENTITY_DEPLOY="identity"
GENESIS_KEY="MEMQL_GENESIS_B64"           # sealed envelope carrying the signing seed (#1515/#550)
SIGNING_KEY="MEMQL_IDENTITY_SIGNING_KEY_B64"    # the shared signing seed, if surfaced directly
EPHEMERAL_OPT_IN="MEMQL_IDENTITY_ALLOW_EPHEMERAL_KEY"  # per-pod ephemeral-key opt-in (divergent at >=2 replicas, #1515)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_SCRIPT="$SCRIPT_DIR/aks-deploy.sh"
GATE_SCRIPT="$SCRIPT_DIR/post-deploy-gate.sh"
SMOKE_SCRIPT="$SCRIPT_DIR/staging-smoke-test.sh"

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

ONE idempotent operator command to take staging to a VALIDATED release, or to
RECOVER a release that went green-on-drift but is actually broken (#1524).

Options:
    --env=ENV          Target environment (default: $DEFAULT_ENV). Staging-only by design.
    --version=X.Y.Z    Release version to build + roll out (full mode). Omit to deploy the overlay-pinned tags.
    --verify           RECOVERY mode: skip build/apply; run ONLY the bff promote + functional gate + smoke
                       against what is already live (the 2026-06-16 recovery as one command).
    --skip-build       Full mode: deploy already-pushed tags (passthrough to aks-deploy.sh).
    --no-smoke         Skip the front-door smoke gate.
    --no-gate          Downgrade the credential-dependent functional condition to a warning (passthrough).
    --dry-run          Print the full plan and mutate NOTHING (no cluster required).
    --help             Show this help.

Examples:
    $0 --version=0.9.61                 # full release: build -> apply -> promote -> gate -> smoke -> record
    $0 --version=0.9.61 --dry-run       # full plan, no changes
    $0 --verify                         # RECOVER a stuck/false-green release: promote bff + re-gate + smoke
    $0 --skip-build --version=0.9.61    # roll already-pushed tags, then gate

Sibling: staging-reset.sh wipes staging to a fresh, auth-coherent state (#1500/#1522).
EOF
}

function parse_arguments() {
    ENV="$DEFAULT_ENV"
    NS="$DEFAULT_NS"
    VERSION=""
    VERIFY=false
    SKIP_BUILD=false
    NO_SMOKE=false
    NO_GATE=false
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env=*)      ENV="${1#*=}"; shift ;;
            --version=*)  VERSION="${1#*=}"; shift ;;
            --verify|--recover) VERIFY=true; shift ;;
            --skip-build) SKIP_BUILD=true; shift ;;
            --no-smoke)   NO_SMOKE=true; shift ;;
            --no-gate)    NO_GATE=true; shift ;;
            --dry-run)    DRY_RUN=true; shift ;;
            --help|-h)    show_help; exit 0 ;;
            *) err "unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

function validate_arguments() {
    if [[ "$ENV" != "staging" ]]; then
        err "--env must be 'staging' (got '${ENV:-<empty>}'). This entrypoint is staging-only."
        exit 1
    fi
    command -v kubectl >/dev/null 2>&1 || { err "kubectl is required"; exit 1; }
    for s in "$DEPLOY_SCRIPT" "$GATE_SCRIPT"; do
        [[ -f "$s" ]] || { err "required script not found: $s"; exit 1; }
    done
}

# Wrong-cluster guard: the live kube-context must look like staging. Skipped in
# --dry-run so the plan needs no cluster (mirrors aks-deploy.sh / the tests).
function require_staging_context() {
    if [[ "$DRY_RUN" == true ]]; then
        plan "verify current kube-context contains '$EXPECTED_CONTEXT_SUBSTR' (wrong-cluster guard)"
        return 0
    fi
    local ctx
    ctx="$(kubectl config current-context 2>/dev/null || true)"
    if [[ -z "$ctx" ]]; then err "no current kube-context; refusing"; exit 1; fi
    if [[ "$ctx" != *"$EXPECTED_CONTEXT_SUBSTR"* ]]; then
        err "kube-context '$ctx' does not contain '$EXPECTED_CONTEXT_SUBSTR' -- refusing (wrong-cluster guard)."
        exit 1
    fi
    info "context=$ctx env=$ENV namespace=$NS verify=$VERIFY"
}

# Read-only auth-coherence PRE-FLIGHT. The incident's root cause was releasing
# onto an auth-incoherent base: identity had NO shared signing seed, so JWKS
# diverged across replicas. We refuse to release/recover until the seed that
# #1515 requires is actually in place. Identical signal to staging-db-reset.sh's
# verify_auth_seed, but READ-ONLY (we never mutate here).
function preflight_coherence() {
    section "Pre-flight: auth coherence (shared identity signing seed, #1515)"
    if [[ "$DRY_RUN" == true ]]; then
        plan "kubectl get secret $SECRET_NAME -- assert it carries $GENESIS_KEY (sealed envelope) or $SIGNING_KEY"
        plan "kubectl get deploy $IDENTITY_DEPLOY -- assert NOT $EPHEMERAL_OPT_IN=true at >=2 replicas (divergent JWKS)"
        return 0
    fi
    if ! kubectl -n "$NS" get secret "$SECRET_NAME" >/dev/null 2>&1; then
        err "Secret '$SECRET_NAME' not found in '$NS' -- cannot verify the identity signing seed."
        err "seal + apply the genesis envelope first (scripts/secrets/reseal-genesis.sh); see #1515."
        exit 1
    fi
    local secret_keys=""
    # shellcheck disable=SC2016
    secret_keys="$(kubectl -n "$NS" get secret "$SECRET_NAME" \
        -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}' 2>/dev/null || true)"
    if ! printf '%s\n' "$secret_keys" | grep -qxE "$GENESIS_KEY|$SIGNING_KEY"; then
        err "Secret '$SECRET_NAME' carries neither $GENESIS_KEY nor $SIGNING_KEY -- identity would mint"
        err "per-pod keys and JWKS would diverge across replicas (#1515). Reseal the seed before releasing."
        exit 1
    fi
    local replicas eph
    replicas="$(kubectl -n "$NS" get deploy "$IDENTITY_DEPLOY" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
    eph="$(kubectl -n "$NS" get deploy "$IDENTITY_DEPLOY" \
        -o jsonpath="{range .spec.template.spec.containers[*].env[?(@.name=='$EPHEMERAL_OPT_IN')]}{.value}{end}" \
        2>/dev/null || true)"
    if [[ "$eph" == "true" && "${replicas:-0}" -ge 2 ]]; then
        err "identity runs $replicas replicas with $EPHEMERAL_OPT_IN=true -- per-pod keys DIVERGE (#1515)."
        err "remove $EPHEMERAL_OPT_IN (rely on the shared $SIGNING_KEY seed) before releasing."
        exit 1
    fi
    info "  seed present in $SECRET_NAME; identity (${replicas:-?} replicas) on the shared-seed path -- JWKS coherent."
}

# FULL release: delegate to the deploy engine, which already folds in
# apply-identity-first + drift + bff promote (#1520) + functional gate (#1519) +
# smoke + record-validated. We do NOT re-implement any of that here.
function run_full_release() {
    section "Full release -> aks-deploy.sh (build, apply, drift, promote, gate, smoke, record)"
    local -a args=( "--env=$ENV" )
    [[ -n "$VERSION" ]] && args+=( "--version=$VERSION" )
    [[ "$SKIP_BUILD" == true ]] && args+=( "--skip-build" )
    [[ "$NO_SMOKE" == true ]] && args+=( "--no-smoke" )
    [[ "$NO_GATE" == true ]] && args+=( "--no-gate" )
    [[ "$DRY_RUN" == true ]] && args+=( "--dry-run" )
    if [[ "$DRY_RUN" == true ]]; then
        plan "bash $DEPLOY_SCRIPT ${args[*]}"
        return 0
    fi
    info "delegating to: bash $DEPLOY_SCRIPT ${args[*]}"
    # Fail LOUD: a non-zero from the deploy engine (failed migration gate,
    # failed functional gate, ...) must abort here, never fall through to the
    # success line -- the false-green class this whole epic exists to kill.
    if ! bash "$DEPLOY_SCRIPT" "${args[@]}"; then
        err "full release FAILED in aks-deploy.sh -- staging is NOT validated. See the output above."
        exit 1
    fi
}

# RECOVERY / re-validate: promote the (possibly parked) bff Rollout + run the
# functional gate against what is already live, then smoke. No build, no apply.
# This is the 2026-06-16 recovery distilled into one idempotent command.
function run_verify_recovery() {
    section "Recovery: promote bff + functional gate (#1520/#1519) -- no build, no apply"
    local -a gate_args=( "--env=$ENV" )
    [[ "$NO_GATE" == true ]] && gate_args+=( "--no-gate" )
    if [[ "$DRY_RUN" == true ]]; then
        plan "bash $GATE_SCRIPT ${gate_args[*]}    # promote stuck bff, then validate: bff promoted + JWKS coherent + BFF->agent auth"
    else
        info "delegating to: bash $GATE_SCRIPT ${gate_args[*]}"
        if ! bash "$GATE_SCRIPT" "${gate_args[@]}"; then
            err "recovery FAILED the functional gate -- staging is still broken. See the output above."
            exit 1
        fi
    fi

    if [[ "$NO_SMOKE" == true ]]; then
        info "smoke gate skipped (--no-smoke)."
        return 0
    fi
    section "Front-door smoke gate"
    if [[ ! -f "$SMOKE_SCRIPT" ]]; then
        warn "staging-smoke-test.sh not found at $SMOKE_SCRIPT; skipping smoke."
        return 0
    fi
    if [[ "$DRY_RUN" == true ]]; then
        plan "bash $SMOKE_SCRIPT    # TLS+DNS, identity health/JWKS, login surface, ws upgrade, audio route"
    elif ! bash "$SMOKE_SCRIPT"; then
        err "recovery passed the gate but the front-door smoke FAILED. See the output above."
        exit 1
    fi
}

function main() {
    parse_arguments "$@"
    validate_arguments

    echo "========================================="
    echo "memQL staging release ($([ "$VERIFY" = true ] && echo RECOVERY/verify || echo FULL))"
    echo "  env=$ENV  version=${VERSION:-<overlay-pinned>}  dry-run=$DRY_RUN"
    echo "========================================="

    require_staging_context
    preflight_coherence

    if [[ "$VERIFY" == true ]]; then
        run_verify_recovery
    else
        run_full_release
    fi

    echo ""
    if [[ "$DRY_RUN" == true ]]; then
        echo "RESULT: dry-run plan only -- nothing changed."
    else
        echo "RESULT: staging release ($([ "$VERIFY" = true ] && echo recovery || echo full)) completed green-and-verified."
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
