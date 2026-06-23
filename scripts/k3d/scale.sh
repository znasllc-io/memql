#!/usr/bin/env bash
#
# scripts/k3d/scale.sh
# ====================
#
# Scale all memQL app Deployments to a target replica count.
# Used to switch between single-node (replicas=1) and multi-node
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
# Usage
# -----
#   make k3d-scale N=2    # scale to 2 replicas per Deployment (multi-node)
#   make k3d-scale N=1    # scale back to 1 (single-node, default)
#   make k3d-status       # litmus check: verify unique node ids per pod
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

TARGET_REPLICAS="${1:-}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { echo "INFO:  $*"; }
function warn()  { echo "WARN:  $*"; }
function error() { echo "ERROR: $*" >&2; }

#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v kubectl &>/dev/null; then
        error "kubectl is required but not installed."
        exit 1
    fi
    if ! kubectl get namespace "${NAMESPACE}" &>/dev/null; then
        error "Namespace '${NAMESPACE}' not found. Run 'make k3d-up' first."
        exit 1
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
                -n "${NAMESPACE}"
            info "  ${deployment}: scaled to ${n}"
        else
            warn "  ${deployment}: not found (may not be deployed yet)"
        fi
    done

    echo ""
    info "Done. Current pod state:"
    kubectl get pods -n "${NAMESPACE}" \
        --sort-by=.metadata.name \
        -o wide 2>/dev/null | head -30 || true

    if [ "${n}" -gt 1 ]; then
        echo ""
        info "Multi-node mode active (replicas=${n})."
        info "Run 'make k3d-status' to verify each pod has a UNIQUE MEMQL_NODE_ID."
        info "Unique ids are required for cross-node mesh routing to work (#1388 class)."
    fi
}

#=============================================================================
# PARSE ARGUMENTS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 <replicas> [options]

Scale all memQL app Deployments to a target replica count.

Positional:
    replicas    Target replica count (e.g. 1 for single-node, 2 for multi-node)

Options:
    --cluster=NAME   k3d cluster name (default: ${CLUSTER_NAME})
    --namespace=NS   k8s namespace (default: ${NAMESPACE})
    --help           Show this help

Examples:
    $0 2              # scale to multi-node (2 replicas each)
    $0 1              # scale back to single-node
    make k3d-scale N=2
    make k3d-scale N=1
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cluster=*)   CLUSTER_NAME="${1#*=}"; shift ;;
            --namespace=*) NAMESPACE="${1#*=}";    shift ;;
            --help)        show_help; exit 0 ;;
            [0-9]*)        TARGET_REPLICAS="$1";   shift ;;
            *) error "Unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"

    if [ -z "${TARGET_REPLICAS}" ]; then
        error "Replica count is required."
        show_help
        exit 1
    fi

    if ! [[ "${TARGET_REPLICAS}" =~ ^[1-9][0-9]*$ ]]; then
        error "Replica count must be a positive integer, got: '${TARGET_REPLICAS}'"
        exit 1
    fi

    check_prerequisites
    scale_all "${TARGET_REPLICAS}"
}

main "$@"
