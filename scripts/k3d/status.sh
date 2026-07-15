#!/usr/bin/env bash
#
# scripts/k3d/status.sh
# =====================
#
# Capability: k3d.status -- print a parity litmus for the local k3d cluster:
# pod status, per-replica MEMQL_NODE_ID values (cross-node mesh test), and
# ArgoCD sync status.
#
# Backs 'make status'. Primary use: verify the mesh formed across replicas
# and that each pod has a UNIQUE node id (required for cross-node event
# routing). This is a status REPORTER: the human-readable table goes to
# STDERR (so `make status` still shows it inline), and the machine-readable
# litmus RESULT (pods / uniqueNodeIds / meshHealthy) is the single JSON
# envelope on STDOUT.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param | 4 cluster unreachable
#
# Refs: #2067 #2061 #2221

# Reporter: no -e so an individual kubectl failure doesn't abort the report.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.status" "Print local k3d cluster status and mesh litmus."
cap_spec_param "cluster"   "k3d cluster name"
cap_spec_param "namespace" "k8s namespace"
cap_spec_param "app-name"  "ArgoCD Application name to report on"
#=============================================================================
# CONFIGURATION
#=============================================================================

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"
APP_NAME="${MEMQL_K3D_APP_NAME:-memql-local}"

# Litmus accumulators (populated by check_node_ids).
PODS_TOTAL=0
UNIQUE_NODE_IDS=0
MESH_HEALTHY=false

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { cap_info "$*"; }
function warn()  { cap_warn "$*"; }

function section() {
    {
        echo ""
        echo "============================================================"
        echo "  $*"
        echo "============================================================"
    } >&2
}

#=============================================================================
# CLUSTER STATUS
#=============================================================================

function check_cluster() {
    section "k3d cluster '${CLUSTER_NAME}'"

    if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        {
            echo "  STATUS: cluster '${CLUSTER_NAME}' is NOT running."
            echo "  Run:    make up"
        } >&2
        cap_result_set     cluster "$CLUSTER_NAME"
        cap_result_set     namespace "$NAMESPACE"
        cap_fail 4 "k3d cluster '${CLUSTER_NAME}' is not running"
    fi

    k3d cluster list 2>/dev/null | grep -E "^NAME|^${CLUSTER_NAME}" >&2
    kubectl config use-context "k3d-${CLUSTER_NAME}" &>/dev/null || true
}

#=============================================================================
# ARGOCD STATUS
#=============================================================================

function check_argocd() {
    section "ArgoCD Application"

    if ! kubectl get namespace argocd &>/dev/null; then
        echo "  STATUS: ArgoCD namespace not found -- run 'make up' first." >&2
        return 0
    fi

    if kubectl get application "${APP_NAME}" -n argocd &>/dev/null; then
        kubectl get application "${APP_NAME}" -n argocd \
            -o custom-columns=\
"NAME:.metadata.name,\
SYNC:.status.sync.status,\
HEALTH:.status.health.status,\
REVISION:.status.sync.revision" >&2 2>/dev/null || \
        kubectl get application "${APP_NAME}" -n argocd >&2 2>/dev/null
    else
        {
            echo "  STATUS: ${APP_NAME} Application not found."
            echo "  Run:    make up"
        } >&2
    fi
}

#=============================================================================
# POD STATUS
#=============================================================================

function check_pods() {
    section "Pods in namespace '${NAMESPACE}'"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  STATUS: namespace '${NAMESPACE}' not found -- ArgoCD may not have synced yet." >&2
        return 0
    fi

    kubectl get pods -n "${NAMESPACE}" \
        -o wide \
        --sort-by=.metadata.name >&2 2>/dev/null || echo "  (no pods found)" >&2
}

#=============================================================================
# MESH LITMUS: UNIQUE NODE IDS
#=============================================================================

function check_node_ids() {
    section "Mesh litmus: MEMQL_NODE_ID per running pod (must be unique)"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  (namespace not ready)" >&2
        return 0
    fi

    local pods
    pods=$(kubectl get pods -n "${NAMESPACE}" \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)

    if [ -z "$pods" ]; then
        echo "  (no running pods found)" >&2
        return 0
    fi

    # Node ids are read from the POD SPEC, not `kubectl exec` (#2380): the
    # pure-engine images are FROM scratch (no shell), so exec-based reads
    # reported every engine pod as unset and the litmus false-failed the
    # first live deploy. A fieldRef metadata.name env resolves to the pod
    # name BY CONSTRUCTION; an explicit value is read verbatim; pods that
    # do not declare the var at all (infra: postgres/azurite) are exempt
    # from the mesh litmus.
    local node_ids=()
    for pod in $pods; do
        local node_id
        node_id=$(kubectl get pod -n "${NAMESPACE}" "${pod}" -o jsonpath='{range .spec.containers[0].env[?(@.name=="MEMQL_NODE_ID")]}{.value}{.valueFrom.fieldRef.fieldPath}{end}' 2>/dev/null)
        case "$node_id" in
            "")            echo "  ${pod}: (no MEMQL_NODE_ID declared -- infra pod, exempt)" >&2; continue ;;
            metadata.name) node_id="$pod" ;;
        esac
        echo "  ${pod}: MEMQL_NODE_ID=${node_id}" >&2
        node_ids+=("${node_id}")
    done

    if [ ${#node_ids[@]} -eq 0 ]; then
        echo "  (no mesh pods declare MEMQL_NODE_ID)" >&2
        PODS_TOTAL=0
        UNIQUE_NODE_IDS=0
        MESH_HEALTHY=false
        return 0
    fi

    # Check uniqueness.
    local unique_count
    unique_count=$(printf '%s\n' "${node_ids[@]}" | sort -u | wc -l | tr -d ' ')
    local total_count=${#node_ids[@]}

    PODS_TOTAL="${total_count}"
    UNIQUE_NODE_IDS="${unique_count}"

    echo "" >&2
    if [ "${unique_count}" -eq "${total_count}" ]; then
        echo "  PASS: all ${total_count} pod(s) have unique MEMQL_NODE_ID values." >&2
        MESH_HEALTHY=true
    else
        warn "FAIL: ${total_count} pods but only ${unique_count} unique MEMQL_NODE_ID values."
        warn "Shared node ids cause silent event-routing failures (mesh #1388 class of bugs)."
        MESH_HEALTHY=false
    fi
}

#=============================================================================
# SECRET STATUS
#=============================================================================

function check_secrets() {
    section "k8s Secrets"

    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        echo "  (namespace not ready)" >&2
        return 0
    fi

    for secret in memql-secrets memql-local-db-creds livekit-secrets telephony-secrets; do
        if kubectl get secret "${secret}" -n "${NAMESPACE}" &>/dev/null; then
            echo "  PRESENT: ${secret}" >&2
        else
            echo "  MISSING: ${secret}  (run: make secrets)" >&2
        fi
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "$CLUSTER_NAME")"
    NAMESPACE="$(cap_param namespace "$NAMESPACE")"
    APP_NAME="$(cap_param app-name "$APP_NAME")"

    check_cluster
    check_argocd
    check_pods
    check_secrets
    check_node_ids

    {
        echo ""
        echo "  Tear down:  make down"
        echo "  Inner loop: make dev [NODE=<type>]"
        echo ""
    } >&2

    cap_result_set     cluster "$CLUSTER_NAME"
    cap_result_set     namespace "$NAMESPACE"
    cap_result_set_raw pods "$PODS_TOTAL"
    cap_result_set_raw uniqueNodeIds "$UNIQUE_NODE_IDS"
    cap_result_set_raw meshHealthy "$MESH_HEALTHY"
    cap_ok
}

main "$@"
