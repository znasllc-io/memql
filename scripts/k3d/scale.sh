#!/usr/bin/env bash
#
# scripts/k3d/scale.sh
# ====================
#
# Capability: k3d.scale -- scale all memQL app Deployments to a target replica
# count. Used to switch between single-node (replicas=1) and multi-node
# (replicas=2) modes without modifying the GitOps overlay.
#
# Why not use ArgoCD for this?
# ----------------------------
# The local overlay pins replicas=1 in its Kustomize patches for
# resource-constrained dev laptops (default mode). Scaling to 2 for
# cross-node mesh testing is a runtime override -- not a manifest
# change. ArgoCD's ignoreDifferences config already excludes
# /spec/replicas from drift detection (same as staging), so scaling
# up here will NOT be reverted by selfHeal.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   make scale N=2                          # scale to 2 replicas per Deployment
#   make scale N=1                          # scale back to 1 (single-node)
#   scripts/k3d/scale.sh --replicas=2
#   scripts/k3d/scale.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (kubectl/ns absent)
#
# Refs: #2067 #2061 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.scale" "Scale all memQL app Deployments to a target replica count."
cap_spec_param "replicas"  "replica count per Deployment"
cap_spec_param "cluster"   "k3d cluster name"
cap_spec_param "namespace" "k8s namespace"
#=============================================================================
# CONFIGURATION
#=============================================================================

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"

# All Deployments managed by the local overlay (excludes postgres + azurite
# which are infra-only and don't form the memQL mesh).
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
        cap_fail 4 "Namespace '${NAMESPACE}' not found. Run 'make up' first."
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
    if ! [[ "${replicas}" =~ ^[1-9][0-9]*$ ]]; then
        cap_fail 2 "Replica count must be a positive integer, got: '${replicas}'"
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
