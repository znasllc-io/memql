#!/usr/bin/env bash
#
# scripts/k3d/down.sh
# ===================
#
# Capability: k3d.down -- tear down the local k3d cluster.
#
# Deletes the cluster and optionally removes the kubeconfig entry. Idempotent:
# deleting an absent cluster is a successful no-op (changed=false).
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   make down                       # delete cluster, keep kubeconfig context
#   make down PURGE=1               # delete cluster + purge kubeconfig context
#   scripts/k3d/down.sh --cluster=memql --purge
#   scripts/k3d/down.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 4 prerequisite missing (k3d absent)
#
# Refs: #2063 #2061 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.down" "Tear down the local k3d cluster."
cap_spec_param "cluster" "k3d cluster name"
cap_spec_param "purge"   "also remove the kubeconfig context (flag)"
#=============================================================================
# PREREQUISITE CHECKS
#=============================================================================

function check_prerequisites() {
    if ! command -v k3d &>/dev/null; then
        cap_fail 4 "k3d is not installed; nothing to tear down"
    fi
}

#=============================================================================
# TEARDOWN
#=============================================================================

function cluster_exists() {
    k3d cluster list 2>/dev/null | grep -q "^${1}[[:space:]]"
}

# delete_cluster <cluster> -- returns 0 if it deleted, 1 if it was already absent.
function delete_cluster() {
    local cluster="$1"
    if ! cluster_exists "$cluster"; then
        cap_info "Cluster '${cluster}' does not exist -- nothing to delete."
        return 1
    fi
    cap_info "Deleting k3d cluster '${cluster}'..."
    k3d cluster delete "$cluster" >&2
    cap_info "Cluster '${cluster}' deleted."
    return 0
}

function remove_kubeconfig() {
    local cluster="$1" purge="$2"
    if [[ -z "$purge" ]]; then
        cap_info "kubeconfig context 'k3d-${cluster}' kept (set --purge to remove)."
        return
    fi
    if command -v kubectl &>/dev/null; then
        cap_info "Removing kubeconfig context 'k3d-${cluster}'..."
        kubectl config delete-context "k3d-${cluster}" 2>/dev/null >&2 || true
        kubectl config delete-cluster "k3d-${cluster}"  2>/dev/null >&2 || true
        kubectl config unset "users.k3d-${cluster}"     2>/dev/null >&2 || true
        cap_info "kubeconfig context removed."
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local cluster purge
    cluster="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    purge="$(cap_flag purge)"
    [[ -z "$purge" && -n "${PURGE:-}" ]] && purge="$PURGE"
    cap_require cluster "$cluster"

    check_prerequisites

    local deleted=false
    if delete_cluster "$cluster"; then
        deleted=true
        cap_changed
    fi
    remove_kubeconfig "$cluster" "$purge"

    cap_info "Done. Run 'make up' to recreate the cluster."
    cap_result_set     cluster "$cluster"
    cap_result_set_raw deleted "$deleted"
    cap_result_set_raw purged  "$( [[ -n "$purge" ]] && echo true || echo false )"
    cap_ok
}

main "$@"
