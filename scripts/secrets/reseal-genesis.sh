#!/usr/bin/env bash
#
# scripts/secrets/reseal-genesis.sh
# =================================
#
# deployment-v2 Phase 5 guardrail (znasllc-io/memql#738; root-cause of #734).
#
# Re-seal the shared-secret genesis envelope and propagate it WITHOUT leaving
# Key Vault and the cluster Secret diverged -- the #734 drift. This is the only
# sanctioned way to rotate a shared secret; never `kubectl set env` and never
# hand-patch only one side.
#
# The drift in #734 happened because the old §4 flow updated the cluster Secret
# AND Key Vault as two independent manual steps; skip or fat-finger one and the
# two sources silently diverge. This script makes divergence impossible to leave
# behind: it writes Key Vault (the canonical source now that ESO owns the
# Secret), drives the propagation to the cluster, and then HARD-VERIFIES that the
# live Secret's MEMQL_GENESIS_B64 hash equals what we pushed before it will roll
# any pod. If they don't converge it aborts non-zero.
#
# Two propagation paths, auto-detected:
#   - ESO present  (ExternalSecret memql-secrets in ns memql): write Key Vault,
#     force an ESO refresh, wait for SecretSynced, verify convergence. ESO is the
#     sole writer of the managed keys, so a manual cluster patch would just be
#     reverted on the next refresh -- we don't do it.
#   - ESO absent   (pre-ESO bootstrap): write Key Vault AND `kubectl patch` the
#     cluster Secret in the same run, then verify convergence.
#
# Usage:
#   MEMQL_MASTER_KEY=... \
#   scripts/secrets/reseal-genesis.sh \
#       --env=staging \
#       --env-file=~/Downloads/staging.genesis.env \
#       [--roll="deployment/identity deployment/bff ..."] \
#       [--no-roll]
#
set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_NS="memql"
DEFAULT_SECRET="memql-secrets"
DEFAULT_KV_KEY="memql-genesis-b64"
DEFAULT_ES_NAME="memql-secrets"   # the ExternalSecret object name

# verify_convergence polling. ESO flips the ExternalSecret "SecretSynced"
# condition BEFORE the underlying Secret data is physically rewritten, so a
# single immediate read can hash a stale value and false-abort (#1498). Poll
# the live Secret until it matches what we pushed before declaring real
# #734 divergence. CONVERGE_TRIES * CONVERGE_DELAY = the bounded wait (~60s).
CONVERGE_TRIES=30
CONVERGE_DELAY=2

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    sed -n '2,46p' "$0" | sed 's/^# \{0,1\}//'
}

function parse_arguments() {
    ENV=""
    ENV_FILE=""
    NS="$DEFAULT_NS"
    SECRET="$DEFAULT_SECRET"
    KV_KEY="$DEFAULT_KV_KEY"
    ES_NAME="$DEFAULT_ES_NAME"
    ROLL_TARGETS=""
    DO_ROLL=true

    while [[ $# -gt 0 ]]; do
        case $1 in
            --env=*)       ENV="${1#*=}"; shift ;;
            --env-file=*)  ENV_FILE="${1#*=}"; shift ;;
            --namespace=*) NS="${1#*=}"; shift ;;
            --roll=*)      ROLL_TARGETS="${1#*=}"; shift ;;
            --no-roll)     DO_ROLL=false; shift ;;
            --help|-h)     show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1" >&2; show_help; exit 1 ;;
        esac
    done
}

function validate_arguments() {
    [[ -n "$ENV" ]] || { echo "ERROR: --env is required (staging|prod)" >&2; exit 1; }
    [[ -n "$ENV_FILE" ]] || { echo "ERROR: --env-file is required" >&2; exit 1; }
    ENV_FILE="${ENV_FILE/#\~/$HOME}"
    [[ -f "$ENV_FILE" ]] || { echo "ERROR: env-file not found: $ENV_FILE" >&2; exit 1; }
    [[ -n "${MEMQL_MASTER_KEY:-}" ]] || {
        echo "ERROR: MEMQL_MASTER_KEY must be exported (must match the cluster's)" >&2; exit 1; }

    VAULT="kv-memql-${ENV}"

    for bin in az kubectl go base64 shasum; do
        command -v "$bin" >/dev/null 2>&1 || { echo "ERROR: $bin not found" >&2; exit 1; }
    done
}

# Seal the plaintext env-file into a base64 blob (the value KV/the Secret carry).
function reseal_envelope() {
    local sealed="/tmp/${ENV}.genesis.$$.znas"
    echo "INFO: sealing $ENV_FILE -> envelope" >&2
    go run ./cmd/genesis-seal --env-file="$ENV_FILE" --out="$sealed" \
        --sync-shell=false --sync-source=false >&2
    NEW_BLOB="$(base64 < "$sealed" | tr -d '\n')"
    NEW_HASH="$(printf '%s' "$NEW_BLOB" | shasum -a 256 | cut -d' ' -f1)"
    rm -f "$sealed"
    echo "INFO: new envelope sha256=$NEW_HASH" >&2
}

function push_key_vault() {
    echo "INFO: pushing envelope to Key Vault $VAULT/$KV_KEY" >&2
    az keyvault secret set --vault-name "$VAULT" --name "$KV_KEY" \
        --value "$NEW_BLOB" --output none
}

function eso_present() {
    kubectl -n "$NS" get externalsecret "$ES_NAME" >/dev/null 2>&1
}

# ESO path: force a refresh and wait for SecretSynced.
function propagate_via_eso() {
    echo "INFO: ESO detected -- forcing refresh of ExternalSecret/$ES_NAME" >&2
    kubectl -n "$NS" annotate externalsecret "$ES_NAME" \
        force-sync="$(date +%s 2>/dev/null || echo manual)" --overwrite >/dev/null
    local _i
    for _i in $(seq 1 30); do
        local reason
        reason="$(kubectl -n "$NS" get externalsecret "$ES_NAME" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)"
        [[ "$reason" == "SecretSynced" ]] && { echo "INFO: ExternalSecret SecretSynced" >&2; return 0; }
        sleep 2
    done
    echo "ERROR: ExternalSecret did not reach SecretSynced in time" >&2
    exit 1
}

# Pre-ESO fallback: write the cluster Secret directly.
function propagate_via_patch() {
    echo "INFO: no ESO -- patching cluster Secret $NS/$SECRET directly" >&2
    kubectl -n "$NS" patch secret "$SECRET" --type merge \
        -p "{\"stringData\":{\"MEMQL_GENESIS_B64\":\"$NEW_BLOB\"}}" >/dev/null
}

# The guardrail: the live Secret's MEMQL_GENESIS_B64 MUST equal what we pushed.
function verify_convergence() {
    # Poll, don't single-shot: ESO reports SecretSynced before the Secret data
    # physically lands, so an immediate read can hash a stale value (#1498).
    local live_hash="" _i
    for _i in $(seq 1 "$CONVERGE_TRIES"); do
        live_hash="$(kubectl -n "$NS" get secret "$SECRET" \
            -o jsonpath='{.data.MEMQL_GENESIS_B64}' | base64 -d | shasum -a 256 | cut -d' ' -f1)"
        if [[ "$live_hash" == "$NEW_HASH" ]]; then
            echo "INFO: convergence OK -- Key Vault == cluster Secret (sha256=$NEW_HASH)" >&2
            return 0
        fi
        sleep "$CONVERGE_DELAY"
    done
    echo "ERROR: convergence check FAILED -- cluster Secret diverged from Key Vault" >&2
    echo "       pushed=$NEW_HASH  live=$live_hash" >&2
    echo "       (after polling ~$((CONVERGE_TRIES * CONVERGE_DELAY))s; this is the #734 condition; NOT rolling pods)" >&2
    exit 1
}

function roll_consumers() {
    if [[ "$DO_ROLL" != true ]]; then
        echo "INFO: --no-roll set; pods keep the OLD envelope until next restart" >&2
        return 0
    fi
    local targets="$ROLL_TARGETS"
    if [[ -z "$targets" ]]; then
        # default: every deployment in the namespace consumes memql-secrets via envFrom
        targets="$(kubectl -n "$NS" get deploy -o name | tr '\n' ' ')"
    fi
    echo "INFO: rolling: $targets" >&2
    # shellcheck disable=SC2086
    kubectl -n "$NS" rollout restart $targets >/dev/null
    echo "INFO: rollout restart issued (envFrom re-injected on new pods)" >&2
}

function main() {
    parse_arguments "$@"
    validate_arguments
    reseal_envelope
    push_key_vault
    if eso_present; then
        propagate_via_eso
    else
        propagate_via_patch
    fi
    verify_convergence
    roll_consumers
    echo "INFO: genesis re-seal complete for env=$ENV" >&2
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
