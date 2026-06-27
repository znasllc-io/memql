#!/usr/bin/env bash
#
# scripts/k3d/refresh.sh
# ======================
#
# Capability: k3d.refresh -- one-command clean-slate local environment: nuke
# and repave the k3d + ArgoCD cluster, then rebuild the engine images so the
# mesh actually comes up. This is the old-muscle-memory `make refresh` -- the
# recovery path when the in-cluster DB or ArgoCD state has drifted and a
# from-scratch boot is faster than untangling it.
#
# Steps (in order):
#   1. down --purge   -- tear down the k3d cluster. This destroys the
#                        in-cluster Postgres, so the DB is wiped by
#                        construction (a fresh DB every refresh).
#   2. up             -- recreate cluster + ArgoCD + seed secrets + register
#                        the memql-local Application.
#   3. wait           -- block until ArgoCD has created the app Deployments,
#                        so the rebuild step has something to restart.
#   4. dev            -- build engine (+ carrier) images and `k3d image
#                        import` them so pods can start instead of sitting in
#                        ImagePullBackOff (local images are tag `local`,
#                        imagePullPolicy IfNotPresent).
#   5. wait healthy   -- block until every Deployment reports Available, then
#                        reuse `up`'s status summary.
#
# Idempotent: running it twice back-to-back yields the same healthy state
# (down is a no-op on an absent cluster; up/dev are themselves idempotent).
#
# Honors the same env overrides as `up`: CLUSTER, SERVERS, AGENTS, REVISION
# (plus NAMESPACE). The ArgoCD Application tracks the current git branch, so
# push your branch before refreshing (ArgoCD cannot read local-only branches).
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Exit codes: 0 ok | 2 bad param
#
# Refs: #2206 #2061 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.refresh" "Nuke and repave the local k3d + ArgoCD cluster, then rebuild engine images."
cap_spec_param "cluster"   "k3d cluster name"                             "CLUSTER"
cap_spec_param "namespace" "k8s namespace"                                "NAMESPACE"
cap_spec_param "revision"  "git revision for the ArgoCD Application"      "REVISION"
cap_spec_param "servers"   "k3d server node count"                        "SERVERS"
cap_spec_param "agents"    "k3d agent node count"                         "AGENTS"

#=============================================================================
# CONFIGURATION (env-resolved defaults; cap_param flags/stdin override in main)
#=============================================================================

CLUSTER_NAME="${CLUSTER:-${MEMQL_K3D_CLUSTER:-memql}}"
NAMESPACE="${NAMESPACE:-${MEMQL_K3D_NAMESPACE:-memql}}"
SERVERS="${SERVERS:-}"
AGENTS="${AGENTS:-}"
REVISION="${REVISION:-}"
HEALTHY=false

# How long to wait for ArgoCD to create the app Deployments after `up`, and
# how long to wait for them to become Available after the image rebuild.
CREATE_TIMEOUT="${MEMQL_REFRESH_CREATE_TIMEOUT:-180}"
HEALTHY_TIMEOUT="${MEMQL_REFRESH_HEALTHY_TIMEOUT:-300}"

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function info()  { cap_info "$*"; }
function warn()  { cap_warn "$*"; }
function error() { cap_error "$*"; }

function section() {
    {
        echo ""
        echo "############################################################"
        echo "#  $*"
        echo "############################################################"
    } >&2
}

#=============================================================================
# STEP 1 -- TEAR DOWN (purge)
#=============================================================================

function nuke() {
    section "refresh 1/5: tearing down cluster '${CLUSTER_NAME}' (purge)"
    PURGE=1 bash "${SCRIPT_DIR}/down.sh" --cluster="${CLUSTER_NAME}" --purge >&2
}

#=============================================================================
# STEP 2 -- BRING UP (cluster + ArgoCD + secrets + Application)
#=============================================================================

function bringup() {
    section "refresh 2/5: recreating cluster + ArgoCD + secrets"
    bash "${SCRIPT_DIR}/up.sh" \
        --cluster="${CLUSTER_NAME}" \
        --namespace="${NAMESPACE}" \
        ${REVISION:+--revision="${REVISION}"} \
        ${SERVERS:+--servers="${SERVERS}"} \
        ${AGENTS:+--agents="${AGENTS}"} >&2
}

#=============================================================================
# STEP 3 -- WAIT FOR ARGOCD TO CREATE THE DEPLOYMENTS
#=============================================================================

# `up` returns once ArgoCD is registered, but ArgoCD then syncs the overlay
# asynchronously -- the app Deployments may not exist yet. `dev` restarts
# Deployments by name, so block until at least one app Deployment exists
# before rebuilding. We don't hardcode the node set (it changes as the local
# overlay evolves) -- any app Deployment appearing means the sync is underway.
function wait_for_deployments_created() {
    section "refresh 3/5: waiting for ArgoCD to create the app Deployments"

    local deadline=$((SECONDS + CREATE_TIMEOUT))
    while [ "${SECONDS}" -lt "${deadline}" ]; do
        local count
        count="$(kubectl get deploy -n "${NAMESPACE}" -o name 2>/dev/null | grep -c . || true)"
        if [ "${count:-0}" -gt 0 ]; then
            info "ArgoCD has created ${count} Deployment(s) in '${NAMESPACE}'."
            return 0
        fi
        info "No Deployments yet; waiting for ArgoCD sync... (${SECONDS}s/${CREATE_TIMEOUT}s)"
        sleep 5
    done

    warn "No app Deployments appeared within ${CREATE_TIMEOUT}s."
    warn "ArgoCD may still be syncing (slow first sync / unpushed branch)."
    warn "Inspect: kubectl get apps -n argocd ; kubectl get deploy -n ${NAMESPACE}"
    warn "Continuing to the image rebuild anyway."
}

#=============================================================================
# STEP 4 -- REBUILD + IMPORT IMAGES
#=============================================================================

function rebuild_images() {
    section "refresh 4/5: building + importing engine images (make dev)"
    # No --node filter: rebuild every app node so nothing sits in
    # ImagePullBackOff. dev.sh restarts each Deployment and waits for its
    # rollout; it skips a Deployment that doesn't exist yet.
    bash "${SCRIPT_DIR}/dev.sh" \
        --cluster="${CLUSTER_NAME}" \
        --namespace="${NAMESPACE}" >&2
}

#=============================================================================
# STEP 5 -- WAIT FOR THE MESH TO BE HEALTHY
#=============================================================================

function wait_for_healthy() {
    section "refresh 5/5: waiting for the mesh to become Available"

    if kubectl wait --for=condition=Available deploy --all \
        -n "${NAMESPACE}" \
        --timeout="${HEALTHY_TIMEOUT}s" >&2; then
        info "All Deployments in '${NAMESPACE}' are Available."
        HEALTHY=true
    else
        warn "Not all Deployments became Available within ${HEALTHY_TIMEOUT}s."
        warn "Inspect: kubectl get pods -n ${NAMESPACE}"
        HEALTHY=false
    fi

    info "Refresh complete. Cluster state:"
    kubectl get deploy -n "${NAMESPACE}" >&2 2>/dev/null || true
    info "Mesh litmus:    make status"
    info "Inner loop:     make dev [NODE=<type>]"
    info "Engine gRPC:    kubectl port-forward -n ${NAMESPACE} svc/mcp 50051:50051"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function raw_int_or_null() {
    if [[ -n "$1" ]]; then printf '%s' "$1"; else printf 'null'; fi
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    CLUSTER_NAME="$(cap_param cluster "$CLUSTER_NAME")"
    NAMESPACE="$(cap_param namespace "$NAMESPACE")"
    REVISION="$(cap_param revision "$REVISION")"
    SERVERS="$(cap_param servers "$SERVERS")"
    AGENTS="$(cap_param agents "$AGENTS")"
    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"

    info "memQL local refresh (clean-slate)"
    info "Cluster:   ${CLUSTER_NAME}"
    info "Namespace: ${NAMESPACE}"

    nuke
    bringup
    wait_for_deployments_created
    rebuild_images
    wait_for_healthy

    cap_changed
    cap_result_set     cluster "$CLUSTER_NAME"
    cap_result_set     namespace "$NAMESPACE"
    cap_result_set_raw servers "$(raw_int_or_null "$SERVERS")"
    cap_result_set_raw agents  "$(raw_int_or_null "$AGENTS")"
    cap_result_set_raw rebuilt true
    cap_result_set_raw healthy "$HEALTHY"
    cap_ok
}

main "$@"
