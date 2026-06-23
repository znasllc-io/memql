#!/usr/bin/env bash
#
# scripts/k3d/status.sh
# =====================
#
# Print a parity litmus for the local k3d cluster: pod status, per-replica
# MEMQL_NODE_ID values (cross-node mesh test), and ArgoCD sync status.
#
# Backs 'make k3d-status'. Primary use: verify the mesh formed across
# replicas and that each pod has a UNIQUE node id (required for
# cross-node event routing).
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -euo pipefail.
#
# Refs: #2067 #2061

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { echo "INFO:  $*"; }
function warn()  { echo "WARN:  $*"; }

function section() {
    echo ""
    echo "============================================================"
    echo "  $*"
    echo "============================================================"
}

#=============================================================================
# CLUSTER STATUS
#=============================================================================

function check_cluster() {
    section "k3d cluster '${CLUSTER_NAME}'"

    if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        echo "  STATUS: cluster '${CLUSTER_NAME}' is NOT running."
        echo "  Run:    make up"
        return 1
    fi

    k3d cluster list 2>/dev/null | grep -E "^NAME|^${CLUSTER_NAME}"
    kubectl config use-context "k3d-${CLUSTER_NAME}" &>/dev/null || true
}

#=============================================================================
# ARGOCD STATUS
#=============================================================================

function check_argocd() {
    section "ArgoCD Application"

    if ! kubectl get namespace argocd &>/dev/null; then
        echo "  STATUS: ArgoCD namespace not found -- run 'make up' first."
        return 0
    fi

    if kubectl get application memql-local -n argocd &>/dev/null; then
        kubectl get application memql-local -n argocd \
            -o custom-columns=\
"NAME:.metadata.name,\
SYNC:.status.sync.status,\
HEALTH:.status.health.status,\
REVISION:.status.sync.revision" 2>/dev/null || \
        kubectl get application memql-local -n argocd 2>/dev/null
    else
        echo "  STATUS: memql-local Application not found."
        echo "  Run:    make up"
    fi
}

#=============================================================================
# POD STATUS
#=============================================================================

function check_pods() {
    section "Pods in namespace '${NAMESPACE}'"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  STATUS: namespace '${NAMESPACE}' not found -- ArgoCD may not have synced yet."
        return 0
    fi

    kubectl get pods -n "${NAMESPACE}" \
        -o wide \
        --sort-by=.metadata.name 2>/dev/null || echo "  (no pods found)"
}

#=============================================================================
# MESH LITMUS: UNIQUE NODE IDS
#=============================================================================

function check_node_ids() {
    section "Mesh litmus: MEMQL_NODE_ID per running pod (must be unique)"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  (namespace not ready)"
        return 0
    fi

    local pods
    pods=$(kubectl get pods -n "${NAMESPACE}" \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)

    if [ -z "$pods" ]; then
        echo "  (no running pods found)"
        return 0
    fi

    local node_ids=()
    for pod in $pods; do
        local node_id
        node_id=$(kubectl exec -n "${NAMESPACE}" "${pod}" \
            -- sh -c 'echo ${MEMQL_NODE_ID:-UNSET}' 2>/dev/null || echo "ERROR")
        echo "  ${pod}: MEMQL_NODE_ID=${node_id}"
        node_ids+=("${node_id}")
    done

    # Check uniqueness.
    local unique_count
    unique_count=$(printf '%s\n' "${node_ids[@]}" | sort -u | wc -l | tr -d ' ')
    local total_count=${#node_ids[@]}

    echo ""
    if [ "${unique_count}" -eq "${total_count}" ]; then
        echo "  PASS: all ${total_count} pod(s) have unique MEMQL_NODE_ID values."
    else
        warn "FAIL: ${total_count} pods but only ${unique_count} unique MEMQL_NODE_ID values."
        warn "Shared node ids cause silent event-routing failures (mesh #1388 class of bugs)."
    fi
}

#=============================================================================
# SECRET STATUS
#=============================================================================

function check_secrets() {
    section "k8s Secrets"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  (namespace not ready)"
        return 0
    fi

    for secret in memql-secrets memql-local-db-creds livekit-secrets telephony-secrets; do
        if kubectl get secret "${secret}" -n "${NAMESPACE}" &>/dev/null; then
            echo "  PRESENT: ${secret}"
        else
            echo "  MISSING: ${secret}  (run: make k3d-secrets)"
        fi
    done
}

#=============================================================================
# PARSE ARGUMENTS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Print local k3d cluster status and mesh litmus.

Options:
    --cluster=NAME   k3d cluster name (default: ${CLUSTER_NAME})
    --namespace=NS   k8s namespace (default: ${NAMESPACE})
    --help           Show this help message
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cluster=*)   CLUSTER_NAME="${1#*=}"; shift ;;
            --namespace=*) NAMESPACE="${1#*=}";    shift ;;
            --help)        show_help; exit 0 ;;
            *) echo "ERROR: Unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"

    check_cluster
    check_argocd
    check_pods
    check_secrets
    check_node_ids

    echo ""
    echo "  Tear down:  make down"
    echo "  Inner loop: make dev [NODE=<type>]"
    echo ""
}

main "$@"
