#!/usr/bin/env bash
#
# scripts/k3d/down.sh
# ===================
#
# Tear down the local k3d cluster.  Deletes the cluster and optionally
# removes the kubeconfig entry.
#
# Usage:
#   make k3d-down           # delete cluster, keep kubeconfig context
#   make k3d-down PURGE=1   # delete cluster + purge kubeconfig context
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom. set -euo pipefail.
#
# Refs: #2063 #2061

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
PURGE="${PURGE:-}"

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
    if ! command -v k3d &>/dev/null; then
        error "k3d is not installed. Nothing to tear down."
        exit 1
    fi
}

#=============================================================================
# TEARDOWN
#=============================================================================

function delete_cluster() {
    if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER_NAME}[[:space:]]"; then
        info "Cluster '${CLUSTER_NAME}' does not exist -- nothing to delete."
        return 0
    fi

    info "Deleting k3d cluster '${CLUSTER_NAME}'..."
    k3d cluster delete "${CLUSTER_NAME}"
    info "Cluster '${CLUSTER_NAME}' deleted."
}

function remove_kubeconfig() {
    if [ -z "${PURGE}" ]; then
        info "kubeconfig context 'k3d-${CLUSTER_NAME}' kept (set PURGE=1 to remove)."
        return 0
    fi

    if command -v kubectl &>/dev/null; then
        info "Removing kubeconfig context 'k3d-${CLUSTER_NAME}'..."
        kubectl config delete-context "k3d-${CLUSTER_NAME}" 2>/dev/null || true
        kubectl config delete-cluster "k3d-${CLUSTER_NAME}" 2>/dev/null || true
        kubectl config unset "users.k3d-${CLUSTER_NAME}" 2>/dev/null || true
        info "kubeconfig context removed."
    fi
}

#=============================================================================
# PARSE ARGUMENTS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Tear down the local k3d cluster.

Options:
    --cluster=NAME   k3d cluster name (default: ${CLUSTER_NAME})
    --purge          Also remove the kubeconfig context
    --help           Show this help message

Environment overrides:
    MEMQL_K3D_CLUSTER   cluster name
    PURGE               set to any non-empty value to also purge kubeconfig
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cluster=*) CLUSTER_NAME="${1#*=}"; shift ;;
            --purge)     PURGE=1;                shift ;;
            --help)      show_help; exit 0 ;;
            *) error "Unknown option: $1"; show_help; exit 2 ;;
        esac
    done
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_arguments "$@"

    check_prerequisites
    delete_cluster
    remove_kubeconfig

    info "Done. Run 'make k3d-up' to recreate the cluster."
}

main "$@"
