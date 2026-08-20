#!/usr/bin/env bash
#
# scripts/k3d/scale.sh
# ====================
#
# Capability: k3d.scale -- scale all MemQL app Deployments to a target replica
# count.
#
# Why not use ArgoCD for this?
# ----------------------------
# Replica count is a runtime dimension as well as a committed value. The overlay
# pins the count a fresh reconcile establishes; scaling away from it -- up to 2
# for cross-node mesh testing, down to 0 to park an idle cluster -- is a runtime
# override, not a manifest change. The ArgoCD Application excludes
# /spec/replicas from drift detection (deploy/argocd/apps/memql.yaml), so
# selfHeal will NOT revert what this writes and no manual repair follows a
# scale-to-zero.
#
# ONE namespace (epic memql#3943)
# --------------------------------
# This took an `--env` naming which of two namespaces to scale, back when one
# cluster carried two environments. MemQL ships one installation shape: the
# namespace is `memql`, and a cluster whose namespace is named something else
# passes --namespace. MEMQL_K3D_NAMESPACE is honoured as the default because it
# is the knob `make up` itself uses to decide what to create.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   make scale N=2                          # 2 replicas per Deployment
#   make scale N=0                          # park the cluster (costs storage only)
#   scripts/k3d/scale.sh --replicas=2
#   scripts/k3d/scale.sh --replicas=2 --namespace=memql
#   scripts/k3d/scale.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (kubectl/ns absent)
#
# Refs: #2067 #2061 #2221 #3943

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.scale" "Scale all MemQL app Deployments to a target replica count."
cap_spec_param "replicas"  "replica count per Deployment (0 parks the cluster)"
cap_spec_param "cluster"   "k3d cluster name"
cap_spec_param "namespace" "k8s namespace (default: memql, or MEMQL_K3D_NAMESPACE)"
#=============================================================================
# CONFIGURATION
#=============================================================================

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"

# The namespace `make up` creates, and the one the cloud overlay reconciles
# into. There is ONE installation shape (epic memql#3943), so there is no
# environment to resolve a namespace from -- a cluster whose namespace is named
# differently passes --namespace.
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"

# All Deployments the mesh runs. Excludes postgres + azurite, which are
# local-only infrastructure and not part of the mesh.
APP_DEPLOYMENTS=(
    identity
    voice
    mcp
    bff
    cognition
    agent
    planner
    workbench
    voice-agent
    edge
)

SCALED_COUNT=0

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { cap_info "$*"; }
function warn()  { cap_warn "$*"; }
function error() { cap_error "$*"; }

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &>/dev/null; then
        cap_fail 4 "kubectl is required but not installed."
    fi
    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        cap_fail 4 "Namespace '${NAMESPACE}' does not exist on the current kubectl context. Locally it is created by 'make up'; on a cloud cluster it is created by the ArgoCD Application."
    fi
}

#=============================================================================
# SCALE DEPLOYMENTS
#=============================================================================

function scale_all() {
    local n="$1"

    info "Scaling all app Deployments to replicas=${n} in namespace '${NAMESPACE}'..."

    for deployment in "${APP_DEPLOYMENTS[@]}"; do
        if kubectl get deployment "${deployment}" -n "${NAMESPACE}" &>/dev/null; then
            kubectl scale deployment "${deployment}" \
                --replicas="${n}" \
                -n "${NAMESPACE}" >&2
            info "  ${deployment}: scaled to ${n}"
            SCALED_COUNT=$((SCALED_COUNT + 1))
        else
            warn "  ${deployment}: not found (may not be deployed yet)"
        fi
    done

    info "Done. Current pod state:"
    kubectl get pods -n "${NAMESPACE}" \
        --sort-by=.metadata.name \
        -o wide 2>/dev/null | head -30 >&2 || true

    if [ "${n}" -eq 0 ]; then
        info "The cluster is parked -- it costs storage, not compute."
        info "Bring it back with 'make scale N=1'. ArgoCD ignores /spec/replicas, so nothing scales it back on its own."
    fi
    if [ "${n}" -gt 1 ]; then
        info "Multi-node mode active (replicas=${n})."
        info "Run 'make status' to verify each pod has a UNIQUE MEMQL_NODE_ID."
        info "Unique ids are required for cross-node mesh routing to work (#1388 class)."
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local replicas
    replicas="$(cap_param replicas "${N:-}")"
    CLUSTER_NAME="$(cap_param cluster "$CLUSTER_NAME")"
    NAMESPACE="$(cap_param namespace "$NAMESPACE")"

    cap_require replicas "$replicas"
    # ZERO IS A VALID COUNT and used to be rejected here. Parking an idle
    # cluster at 0 is what makes it cost storage rather than compute, so the
    # pattern admits it.
    if ! [[ "${replicas}" =~ ^(0|[1-9][0-9]*)$ ]]; then
        cap_fail 2 "Replica count must be a non-negative integer, got: '${replicas}'"
    fi

    check_prerequisites
    scale_all "${replicas}"

    cap_changed
    cap_result_set_raw replicas "$replicas"
    cap_result_set     namespace "$NAMESPACE"
    cap_result_set_raw deploymentsScaled "$SCALED_COUNT"
    cap_ok
}

main "$@"
