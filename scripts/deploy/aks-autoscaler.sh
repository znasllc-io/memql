#!/usr/bin/env bash
#
# scripts/deploy/aks-autoscaler.sh
# ================================
#
# Declaratively converge the AKS staging nodepool to the cluster-autoscaler
# sizing codified for znasllc-io/memql#614 (DEPLOYMENT_STRATEGY.md §9).
#
# WHY this exists
# ---------------
# Staging's nodepool1 (Standard_B2s) ran the 16-pod mesh at 91-93% CPU
# requests, so a rolling update -- which surges old+new pods simultaneously
# via maxSurge -- could not schedule the surge pods and stalled the rollout
# (the 0.9.6 incident; nodepool was manually scaled to 4 to unblock). The fix
# is the AKS cluster autoscaler: let a roll surge into a temporary extra node
# and scale back down afterwards. This script makes that config DECLARATIVE
# and REPEATABLE (IaC) instead of a one-off `az` command typed by hand.
#
# WHAT it does (idempotent -- safe to re-run any number of times):
#   1. Pre-flight: az present + logged in, the cluster/nodepool reachable.
#   2. Read the nodepool's current autoscaler state.
#   3. Converge: enable the cluster autoscaler with the codified min/max, or
#      -- if it's already enabled -- update the min/max to the codified values
#      (the `az` "enable" verb errors if already enabled, so we branch).
#      Already-at-target is a no-op.
#
# Codified sizing (the SINGLE source of truth for the chosen values; §9):
#   nodepool1 / Standard_B2s : --min-count 2 --max-count 5
# Override per-invocation with --nodepool / --min / --max if a future
# right-sizing changes the floor, but the DEFAULTS here are the committed,
# reviewed values.
#
# OWNER-GATED LIVE CHANGE
# -----------------------
# Enabling the autoscaler with a chosen min/max on SHARED cluster infra is a
# persistent + cost decision. Per #614 the live apply is deferred to the repo
# owner. Run with --dry-run (the default-safe path in CI / review) to print
# the exact plan and mutate nothing; drop --dry-run only when you intend to
# apply the live change.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): pure
# function-based structure -- one responsibility per function, main() at the
# bottom. set -uo pipefail. Supports --help, --dry-run (no Azure mutations),
# and --show (read-only status).

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# Per-env Azure resource group + cluster + nodepool, plus the codified sizing.
# These are the committed, reviewed defaults for STAGING (#614 / §9). The env
# can be switched with --env; min/max/nodepool can be overridden per call.
ENV_DEFAULT="staging"

# Codified staging sizing (DEPLOYMENT_STRATEGY.md §9 -- "Recommended staging
# floor: min 2, max 5 on B2s").
STAGING_RESOURCE_GROUP="rg-memql-staging"
STAGING_CLUSTER="aks-memql-staging"
STAGING_NODEPOOL="nodepool1"
STAGING_MIN=2
STAGING_MAX=5

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function section() { echo ""; echo "===== $* ====="; }
function info()    { echo "INFO: $*"; }
function warn()    { echo "WARNING: $*"; }
function plan()    { echo "  [plan] $*"; }

# run_or_plan CMD... -- execute the command, or in --dry-run just print it.
function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then
        plan "$*"
        return 0
    fi
    "$@"
}

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Declaratively converge the AKS staging nodepool to the cluster-autoscaler
sizing codified for #614 (DEPLOYMENT_STRATEGY.md §9): min ${STAGING_MIN} /
max ${STAGING_MAX} on ${STAGING_NODEPOOL} (Standard_B2s). Idempotent.

Options:
    --env=ENV         Target environment (default: ${ENV_DEFAULT}). Only
                      'staging' carries codified defaults today.
    --resource-group=RG  Override the resource group.
    --cluster=NAME    Override the AKS cluster name.
    --nodepool=NAME   Override the nodepool (default: ${STAGING_NODEPOOL}).
    --min=N           Override the autoscaler min node count (default: ${STAGING_MIN}).
    --max=N           Override the autoscaler max node count (default: ${STAGING_MAX}).
    --show            Print the nodepool's current autoscaler state and exit
                      (read-only; no mutation).
    --dry-run         Print the exact plan and mutate nothing (no Azure writes).
    --help            Show this help.

OWNER-GATED: enabling the autoscaler on shared cluster infra is a persistent
cost decision deferred to the repo owner (#614). Use --dry-run in review/CI;
drop it only to apply the live change.

The exact live command this converges to (also runnable by hand):
    az aks nodepool update -g ${STAGING_RESOURCE_GROUP} --cluster-name ${STAGING_CLUSTER} \\
        -n ${STAGING_NODEPOOL} --enable-cluster-autoscaler --min-count ${STAGING_MIN} --max-count ${STAGING_MAX}

Examples:
    $0 --dry-run             # print the plan, change nothing
    $0 --show                # read current autoscaler state
    $0                       # APPLY the codified sizing (owner-gated)
EOF
}

function parse_arguments() {
    ENV="$ENV_DEFAULT"
    RESOURCE_GROUP=""
    CLUSTER=""
    NODEPOOL=""
    MIN=""
    MAX=""
    SHOW=false
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --env=*)            ENV="${1#*=}"; shift ;;
            --resource-group=*) RESOURCE_GROUP="${1#*=}"; shift ;;
            --cluster=*)        CLUSTER="${1#*=}"; shift ;;
            --nodepool=*)       NODEPOOL="${1#*=}"; shift ;;
            --min=*)            MIN="${1#*=}"; shift ;;
            --max=*)            MAX="${1#*=}"; shift ;;
            --show)             SHOW=true; shift ;;
            --dry-run)          DRY_RUN=true; shift ;;
            --help)             show_help; exit 0 ;;
            *)
                echo "ERROR: Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done
}

# Resolve the codified per-env defaults, then layer any explicit overrides on
# top. Only staging is codified today; any other --env REQUIRES explicit
# --resource-group/--cluster so we never silently target the wrong cluster.
function resolve_config() {
    case "$ENV" in
        staging)
            RESOURCE_GROUP="${RESOURCE_GROUP:-$STAGING_RESOURCE_GROUP}"
            CLUSTER="${CLUSTER:-$STAGING_CLUSTER}"
            NODEPOOL="${NODEPOOL:-$STAGING_NODEPOOL}"
            MIN="${MIN:-$STAGING_MIN}"
            MAX="${MAX:-$STAGING_MAX}"
            ;;
        *)
            if [ -z "$RESOURCE_GROUP" ] || [ -z "$CLUSTER" ]; then
                echo "ERROR: env '$ENV' has no codified defaults; pass --resource-group and --cluster (and optionally --nodepool/--min/--max)." >&2
                exit 1
            fi
            NODEPOOL="${NODEPOOL:-$STAGING_NODEPOOL}"
            MIN="${MIN:-$STAGING_MIN}"
            MAX="${MAX:-$STAGING_MAX}"
            ;;
    esac

    if ! [[ "$MIN" =~ ^[0-9]+$ ]] || ! [[ "$MAX" =~ ^[0-9]+$ ]]; then
        echo "ERROR: --min/--max must be non-negative integers (got min='$MIN' max='$MAX')." >&2
        exit 1
    fi
    if [ "$MIN" -gt "$MAX" ]; then
        echo "ERROR: --min ($MIN) must be <= --max ($MAX)." >&2
        exit 1
    fi
}

#=============================================================================
# PRE-FLIGHT
#=============================================================================

function check_prerequisites() {
    # In --dry-run we print the plan without ever calling Azure, so az is not
    # required -- this keeps the dry-run path usable in CI / review.
    if [ "$DRY_RUN" = true ]; then
        return 0
    fi
    if ! command -v az &> /dev/null; then
        echo "ERROR: az (Azure CLI) is required to read/converge the nodepool; use --dry-run to plan without Azure." >&2
        exit 1
    fi
    if ! az account show &> /dev/null; then
        echo "ERROR: not logged in to Azure (az account show failed). Run 'az login' first." >&2
        exit 1
    fi
}

# Echo the nodepool's current autoscaler state to stdout as
# "<enabled> <min> <max>" (enabled = true|false; min/max may be 'null').
# Returns non-zero only if the nodepool can't be read.
function read_autoscaler_state() {
    az aks nodepool show \
        -g "$RESOURCE_GROUP" --cluster-name "$CLUSTER" -n "$NODEPOOL" \
        --query "[enableAutoScaling, minCount, maxCount]" -o tsv 2>/dev/null
}

#=============================================================================
# SHOW (read-only)
#=============================================================================

function show_state() {
    section "Autoscaler state: ${CLUSTER}/${NODEPOOL} (${ENV})"
    if [ "$DRY_RUN" = true ]; then
        plan "az aks nodepool show -g $RESOURCE_GROUP --cluster-name $CLUSTER -n $NODEPOOL --query '[enableAutoScaling, minCount, maxCount]'"
        info "codified target: min ${MIN} / max ${MAX}"
        return 0
    fi
    local state
    if ! state="$(read_autoscaler_state)"; then
        echo "ERROR: could not read nodepool $NODEPOOL on $CLUSTER (-g $RESOURCE_GROUP)." >&2
        exit 1
    fi
    local enabled cur_min cur_max
    enabled="$(echo "$state" | awk '{print $1}')"
    cur_min="$(echo "$state" | awk '{print $2}')"
    cur_max="$(echo "$state" | awk '{print $3}')"
    echo "  autoscaler enabled : ${enabled:-false}"
    echo "  current min/max    : ${cur_min:-null} / ${cur_max:-null}"
    echo "  codified target    : ${MIN} / ${MAX}"
}

#=============================================================================
# CONVERGE (declarative apply -- owner-gated live change)
#=============================================================================

# Converge the nodepool to (enabled, MIN, MAX). The az verbs differ by state:
#   - disabled        -> `--enable-cluster-autoscaler --min-count --max-count`
#   - enabled, drift  -> `--update-cluster-autoscaler --min-count --max-count`
#   - enabled, at-tgt -> no-op
# In --dry-run we don't read Azure; we print the canonical enable form (the §9
# command) since that is what a fresh staging nodepool needs.
function converge_autoscaler() {
    section "Converge cluster autoscaler -> min ${MIN} / max ${MAX}"

    if [ "$DRY_RUN" = true ]; then
        plan "az aks nodepool update -g $RESOURCE_GROUP --cluster-name $CLUSTER -n $NODEPOOL --enable-cluster-autoscaler --min-count $MIN --max-count $MAX"
        info "(dry-run) no Azure changes made. Drop --dry-run to apply (owner-gated)."
        return 0
    fi

    local state enabled cur_min cur_max
    if ! state="$(read_autoscaler_state)"; then
        echo "ERROR: could not read nodepool $NODEPOOL on $CLUSTER (-g $RESOURCE_GROUP)." >&2
        exit 1
    fi
    enabled="$(echo "$state" | awk '{print $1}')"
    cur_min="$(echo "$state" | awk '{print $2}')"
    cur_max="$(echo "$state" | awk '{print $3}')"

    if [ "$enabled" = "true" ] && [ "$cur_min" = "$MIN" ] && [ "$cur_max" = "$MAX" ]; then
        info "already converged: autoscaler enabled at min ${MIN} / max ${MAX}. No change."
        return 0
    fi

    if [ "$enabled" = "true" ]; then
        info "autoscaler already enabled (min ${cur_min}/max ${cur_max}); updating min/max to ${MIN}/${MAX}..."
        az aks nodepool update \
            -g "$RESOURCE_GROUP" --cluster-name "$CLUSTER" -n "$NODEPOOL" \
            --update-cluster-autoscaler --min-count "$MIN" --max-count "$MAX"
    else
        info "enabling cluster autoscaler at min ${MIN} / max ${MAX}..."
        az aks nodepool update \
            -g "$RESOURCE_GROUP" --cluster-name "$CLUSTER" -n "$NODEPOOL" \
            --enable-cluster-autoscaler --min-count "$MIN" --max-count "$MAX"
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"
    resolve_config
    check_prerequisites

    echo "========================================="
    echo "memQL AKS autoscaler (#614 / §9)"
    echo "  env=$ENV  cluster=$CLUSTER  nodepool=$NODEPOOL"
    echo "  target: min=$MIN max=$MAX  dry-run=$DRY_RUN"
    echo "========================================="

    if [ "$SHOW" = true ]; then
        show_state
        return 0
    fi

    converge_autoscaler
    info "done."
}

main "$@"
