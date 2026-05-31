#!/usr/bin/env bash
#
# scripts/deploy/aks-rollback.sh
# ==============================
#
# Roll the memQL node mesh back to its previous (or a specific) revision
# (znasllc-io/memql#554, epic #549). The companion to aks-deploy.sh: when a
# deploy goes bad, this reverts every node Deployment via `kubectl rollout
# undo`. aks-deploy.sh's smoke GATE calls the same operation automatically;
# this script is the manual / targeted entry point (`make deploy-rollback`).
#
# `kubectl rollout undo` swaps the Deployment back to a prior ReplicaSet --
# it does NOT touch the database (managed Tiger Cloud, outside the cluster)
# and is a no-op for a Deployment that has no prior revision.
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -uo pipefail (no
# -e -- one node's undo failing must not abort the rest). --help + --dry-run.

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

NAMESPACE="memql"

# Every Deployment aks-deploy.sh rolls -- the full revert set.
readonly ALL_DEPLOYMENTS=(identity bff cognition voice agent planner workbench copresent)

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info() { echo "INFO: $*"; }
function warn() { echo "WARNING: $*"; }
function plan() { echo "  [plan] $*"; }

function run_or_plan() {
    if [ "$DRY_RUN" = true ]; then plan "$*"; return 0; fi
    "$@"
}

#=============================================================================
# ARGS
#=============================================================================

function show_help() {
    cat << EOF
Usage: $0 [options]

Roll the memQL node mesh back to its previous (or a specific) revision.

Options:
    --env=ENV            Environment label for log context (staging|production).
                         The target cluster is the current kubectl context.
    --to-revision=N      Roll back to a specific revision number (applied to
                         every node). Default: the immediately previous one.
    --only=a,b,c         Comma-separated node names to roll back (default: all).
                         Names: ${ALL_DEPLOYMENTS[*]}
    --dry-run            Print what would happen and change nothing.
    --help               Show this help.

Inspect history first with:
    kubectl rollout history deployment/<node> -n $NAMESPACE

Examples:
    $0                          # revert every node to its previous revision
    $0 --only=bff,cognition     # revert just these two
    $0 --to-revision=7          # pin every node back to revision 7
EOF
}

function parse_arguments() {
    ENV="staging"
    TO_REVISION=""
    ONLY=""
    DRY_RUN=false

    while [[ $# -gt 0 ]]; do
        case $1 in
            --env=*)          ENV="${1#*=}"; shift ;;
            --to-revision=*)  TO_REVISION="${1#*=}"; shift ;;
            --only=*)         ONLY="${1#*=}"; shift ;;
            --dry-run)        DRY_RUN=true; shift ;;
            --help)           show_help; exit 0 ;;
            *)
                echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

#=============================================================================
# CORE
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &> /dev/null; then
        echo "ERROR: kubectl is not installed"; exit 1
    fi
    if [ "$DRY_RUN" = false ] && ! kubectl cluster-info &> /dev/null; then
        echo "ERROR: no reachable Kubernetes cluster in the current context."; exit 1
    fi
}

# Resolve the target node list from --only (validated) or the full set.
function resolve_targets() {
    if [ -z "$ONLY" ]; then
        TARGETS=("${ALL_DEPLOYMENTS[@]}")
        return
    fi
    TARGETS=()
    local name
    IFS=',' read -ra requested <<< "$ONLY"
    for name in "${requested[@]}"; do
        name="$(echo "$name" | tr -d '[:space:]')"
        [ -z "$name" ] && continue
        if [[ " ${ALL_DEPLOYMENTS[*]} " == *" $name "* ]]; then
            TARGETS+=("$name")
        else
            echo "ERROR: unknown node '$name' (valid: ${ALL_DEPLOYMENTS[*]})"; exit 1
        fi
    done
}

function rollback_one() {
    local nt="$1"
    local -a cmd=(kubectl rollout undo "deployment/${nt}" -n "$NAMESPACE")
    [ -n "$TO_REVISION" ] && cmd+=(--to-revision="$TO_REVISION")
    info "rolling back ${nt}${TO_REVISION:+ to revision $TO_REVISION}..."
    run_or_plan "${cmd[@]}"
}

function execute() {
    echo "========================================="
    echo "memQL AKS rollback"
    echo "  Env:        $ENV"
    echo "  Context:    $([ "$DRY_RUN" = true ] && echo '(dry-run)' || kubectl config current-context 2>/dev/null || echo unknown)"
    echo "  Namespace:  $NAMESPACE"
    echo "  Targets:    ${TARGETS[*]}"
    echo "  To revision:${TO_REVISION:- previous}"
    echo "  Dry run:    $DRY_RUN"
    echo "========================================="

    local nt
    for nt in "${TARGETS[@]}"; do
        rollback_one "$nt"
    done

    echo ""
    info "rollback issued. Watch: kubectl get pods -n $NAMESPACE -w"
    info "verify: bash scripts/deploy/staging-smoke-test.sh"
}

function main() {
    parse_arguments "$@"
    check_prerequisites
    resolve_targets
    execute
}

main "$@"
