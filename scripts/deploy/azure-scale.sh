#!/usr/bin/env bash
#
# scripts/deploy/azure-scale.sh
# =============================
#
# Capability: deploy.azureScale -- change the size of ONE MemQL instance:
# the node count of an AKS node pool, and/or the replica count of the mesh
# Deployments.
#
# Backend for the `scaleInstance` deployment action (memql#4466, epic
# memql#4463).
#
# THE TWO AXES ARE NOT THE SAME LEVER, and conflating them is the mistake this
# script exists to prevent.
#
#   * NODES cost money. A node is a VM billed per hour whether or not anything
#     runs on it. Scaling nodes is the cost decision.
#   * REPLICAS cost availability. Below 2, a PodDisruptionBudget declaring
#     minAvailable=1 permits ZERO disruptions, so `kubectl drain` -- and
#     therefore every AKS node image upgrade -- blocks forever. Scaling
#     replicas is the uptime decision, and on an instance with spare node
#     capacity it is FREE.
#
# So the common operation is not "scale up": it is "raise replicas to 2 on the
# nodes already paid for", which costs nothing and is what makes node
# maintenance possible at all.
#
# NO DECISIONS INSIDE. This script does not know which node pool should be how
# big, or when. It applies the numbers it is given (capability-script contract,
# #2221). The judgement -- what to scale, in what order, and what it costs --
# lives in docs/public/operate/scale-runbook.md and in the caller.
#
# REPLICA SCALING IS ORDER-SENSITIVE IN ONE DIRECTION. Scaling replicas UP
# before nodes exist to hold them leaves pods Pending, which is visible and
# harmless. Scaling nodes DOWN before replicas leaves pods Pending too -- but
# on a shrinking cluster, and the eviction that frees them may never come. So
# this script always scales nodes UP first and DOWN last, regardless of the
# order the flags were given.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 3 refused | 4 prerequisite missing | 5 op failed
#
# Refs: memql#4463 memql#4466 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

#=============================================================================
# CAPABILITY SPEC
#=============================================================================

cap_init "deploy.azureScale" \
    "Scale a MemQL instance's AKS node pool and/or its mesh Deployment replicas."

cap_spec_param_required "subscriptionId" "Azure subscription the cluster lives in"
cap_spec_param_required "resourceGroup"  "resource group holding the AKS cluster"
cap_spec_param_required "clusterName"    "AKS cluster name"
cap_spec_param "nodePool"                "node pool to resize (e.g. mesh, db); omit to leave nodes alone"
cap_spec_param "nodeCount"               "target node count for --nodePool"
cap_spec_param "replicas"                "target replica count for the mesh Deployments; omit to leave replicas alone"
cap_spec_param "namespace"               "Kubernetes namespace the Deployments live in (default memql)"
cap_spec_param "deployments"             "comma-separated Deployments to scale (default: the mesh set)"
cap_spec_param "dryRun"                  "plan only; change nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

#=============================================================================
# CONFIGURATION
#=============================================================================

SUBSCRIPTION_ID="$(cap_param subscriptionId "")"
RESOURCE_GROUP="$(cap_param resourceGroup "")"
CLUSTER_NAME="$(cap_param clusterName "")"
NODE_POOL="$(cap_param nodePool "")"
NODE_COUNT="$(cap_param nodeCount "")"
REPLICAS="$(cap_param replicas "")"
NAMESPACE="$(cap_param namespace "memql")"
DEPLOYMENTS="$(cap_param deployments "")"
DRY_RUN="$(cap_bool_str dryRun false)"

# The mesh set: every node-type Deployment that carries request traffic. The
# database is NOT here -- it is a CNPG Cluster, whose instance count is a
# property of that resource and changing it is a failover, not a scale.
readonly DEFAULT_DEPLOYMENTS="bff,identity,agent,planner,workbench,edge"

SCALED_NODES="false"
SCALED_REPLICAS="false"

#=============================================================================
# FUNCTIONS
#=============================================================================

function check_prerequisites() {
    command -v az &>/dev/null || cap_fail 4 "az CLI is not installed or not on PATH"

    local active
    active="$(az account show --query id -o tsv 2>/dev/null || true)"
    [[ -n "$active" ]] || cap_fail 4 "not logged in to Azure -- run 'az login --tenant <tenant>' first"
    if [[ "$active" != "$SUBSCRIPTION_ID" ]]; then
        az account set --subscription "$SUBSCRIPTION_ID" 2>/dev/null \
            || cap_fail 3 "cannot select subscription ${SUBSCRIPTION_ID}"
    fi

    if [[ -n "$REPLICAS" ]]; then
        command -v kubectl &>/dev/null || cap_fail 4 "kubectl is required to scale replicas but is not on PATH"
    fi
}

function validate_arguments() {
    [[ -n "$SUBSCRIPTION_ID" ]] || cap_fail 2 "--subscriptionId is required"
    [[ -n "$RESOURCE_GROUP"  ]] || cap_fail 2 "--resourceGroup is required"
    [[ -n "$CLUSTER_NAME"    ]] || cap_fail 2 "--clusterName is required"

    # Asking for neither axis is a caller bug, not a no-op worth hiding.
    [[ -n "$NODE_POOL" || -n "$REPLICAS" ]] \
        || cap_fail 2 "nothing to do -- give --nodePool with --nodeCount, or --replicas, or both"

    if [[ -n "$NODE_POOL" ]]; then
        [[ -n "$NODE_COUNT" ]] || cap_fail 2 "--nodePool ${NODE_POOL} given without --nodeCount"
        [[ "$NODE_COUNT" =~ ^[0-9]+$ ]] || cap_fail 2 "--nodeCount must be an integer, got ${NODE_COUNT}"
    fi
    if [[ -n "$NODE_COUNT" && -z "$NODE_POOL" ]]; then
        cap_fail 2 "--nodeCount given without --nodePool -- which pool?"
    fi
    if [[ -n "$REPLICAS" ]]; then
        [[ "$REPLICAS" =~ ^[0-9]+$ ]] || cap_fail 2 "--replicas must be an integer, got ${REPLICAS}"
    fi
}

function current_node_count() {
    az aks nodepool show --cluster-name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" \
        --name "$NODE_POOL" --query count -o tsv 2>/dev/null || true
}

function scale_nodes() {
    [[ -n "$NODE_POOL" ]] || return 0

    local current
    current="$(current_node_count)"
    [[ -n "$current" ]] \
        || cap_fail 5 "node pool ${NODE_POOL} not found on cluster ${CLUSTER_NAME}"

    if [[ "$current" == "$NODE_COUNT" ]]; then
        cap_info "node pool ${NODE_POOL} is already at ${NODE_COUNT} nodes"
        return 0
    fi

    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would scale node pool ${NODE_POOL} from ${current} to ${NODE_COUNT}"
        return 0
    fi

    cap_step "scaling node pool ${NODE_POOL}: ${current} -> ${NODE_COUNT}"
    az aks nodepool scale --cluster-name "$CLUSTER_NAME" --resource-group "$RESOURCE_GROUP" \
        --name "$NODE_POOL" --node-count "$NODE_COUNT" -o none \
        || cap_fail 5 "failed to scale node pool ${NODE_POOL} to ${NODE_COUNT}"
    SCALED_NODES="true"
    cap_changed
}

function scale_replicas() {
    [[ -n "$REPLICAS" ]] || return 0

    local list="${DEPLOYMENTS:-$DEFAULT_DEPLOYMENTS}"
    local -a names
    IFS=',' read -r -a names <<< "$list"

    local d
    for d in "${names[@]}"; do
        d="$(printf '%s' "$d" | tr -d '[:space:]')"
        [[ -n "$d" ]] || continue

        if ! kubectl get deployment "$d" -n "$NAMESPACE" -o name &>/dev/null; then
            cap_info "deployment ${d} not present in ${NAMESPACE}; skipping"
            continue
        fi

        local current
        current="$(kubectl get deployment "$d" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")"
        if [[ "$current" == "$REPLICAS" ]]; then
            cap_info "deployment ${d} is already at ${REPLICAS} replicas"
            continue
        fi

        if [[ "$DRY_RUN" == "true" ]]; then
            cap_info "DRY RUN: would scale ${d} from ${current} to ${REPLICAS}"
            continue
        fi

        cap_step "scaling ${d}: ${current} -> ${REPLICAS}"
        kubectl scale deployment "$d" -n "$NAMESPACE" --replicas="$REPLICAS" >/dev/null \
            || cap_fail 5 "failed to scale deployment ${d}"
        SCALED_REPLICAS="true"
        cap_changed
    done
}

function collect_result() {
    cap_result_set "clusterName" "$CLUSTER_NAME"
    cap_result_set "dryRun"      "$DRY_RUN"
    cap_result_set_raw "scaledNodes"    "$SCALED_NODES"
    cap_result_set_raw "scaledReplicas" "$SCALED_REPLICAS"
    [[ -n "$NODE_POOL" ]] && { cap_result_set "nodePool" "$NODE_POOL"; cap_result_set "nodeCount" "$NODE_COUNT"; }
    [[ -n "$REPLICAS"  ]] && cap_result_set "replicas" "$REPLICAS"
    # UNCONDITIONAL, and not decoration. A function whose LAST statement is a
    # `[[ ... ]] && cmd` returns 1 when the test is false, and under `set -e`
    # that aborts the caller BEFORE cap_ok is ever reached -- so the envelope
    # reads "aborted (exit 1) without an explicit result" on a run that did
    # everything right, with changed:true beside it. Measured on both scripts
    # in this directory (memql#4490).
    return 0
}

function main() {
    validate_arguments
    check_prerequisites

    # Nodes UP before replicas; replicas down before nodes DOWN. See the header.
    local growing="false"
    if [[ -n "$NODE_POOL" ]]; then
        local current
        current="$(current_node_count)"
        [[ -n "$current" && "$NODE_COUNT" -gt "$current" ]] && growing="true"
    fi

    if [[ "$growing" == "true" ]]; then
        scale_nodes
        scale_replicas
    else
        scale_replicas
        scale_nodes
    fi

    collect_result
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
