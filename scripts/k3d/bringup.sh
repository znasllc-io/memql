#!/usr/bin/env bash
#
# scripts/k3d/bringup.sh
# ======================
#
# Capability: k3d.bringup -- one-command fully-running local environment:
# bootstrap the k3d + ArgoCD cluster, build + import the engine images, and
# wait for the mesh to become Available. This is `make up`. With --clean it
# nukes the cluster first (wiping the in-cluster DB by construction) before
# repaving -- that is `make up-refresh`, the recovery path when the DB or
# ArgoCD state has drifted and a from-scratch boot is faster than untangling
# it.
#
# Steps (in order; step 1 only with --clean):
#   1. down --purge   -- tear down the k3d cluster. Destroys the in-cluster
#                        Postgres, so the DB is wiped by construction.
#   2. up             -- create/converge cluster + ArgoCD + seed secrets +
#                        register the memql-local Application.
#   3. wait           -- block until ArgoCD has created the app Deployments,
#                        so the rebuild step has something to restart.
#   4. dev            -- build engine (+ carrier) images and `k3d image
#                        import` them so pods can start instead of sitting in
#                        ImagePullBackOff (local images are tag `local`,
#                        imagePullPolicy IfNotPresent).
#   5. wait healthy   -- block until every Deployment reports Available, then
#                        print the status summary.
#
# Idempotent: running it twice back-to-back yields the same healthy state
# (down is a no-op on an absent cluster; up/dev are themselves idempotent).
#
# Honors the same env overrides as the underlying scripts: CLUSTER, SERVERS,
# AGENTS, REVISION (plus NAMESPACE). The ArgoCD Application tracks the current
# git branch, so push your branch before bringing up (ArgoCD cannot read
# local-only branches).
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

cap_init "k3d.bringup" "Bring the local k3d + ArgoCD cluster fully up: bootstrap, build + import engine images, wait healthy. --clean nukes first."
cap_spec_param "cluster"    "k3d cluster name"
cap_spec_param "namespace"  "k8s namespace"
cap_spec_param "revision"   "git revision for the ArgoCD Application"
cap_spec_param "servers"    "k3d server node count"
cap_spec_param "agents"     "k3d agent node count"
cap_spec_param "clean"      "tear down the cluster first (clean slate, fresh DB) (flag)"
cap_spec_param "no-secrets" "skip secret seeding (flag)"
#=============================================================================
# CONFIGURATION (env-resolved defaults; cap_param flags/stdin override in main)
#=============================================================================

CLUSTER_NAME="${CLUSTER:-${MEMQL_K3D_CLUSTER:-memql}}"
NAMESPACE="${NAMESPACE:-${MEMQL_K3D_NAMESPACE:-memql}}"
SERVERS="${SERVERS:-}"
AGENTS="${AGENTS:-}"
REVISION="${REVISION:-}"
CLEAN=""
NO_SECRETS=""
HEALTHY=false

# Step labels, set in main() once --clean is known (5 steps clean, 4 fresh).
STEP_UP=""
STEP_WAIT=""
STEP_DEV=""
STEP_HEALTHY=""

# How long to wait for ArgoCD to create the app Deployments after `up`, and
# how long to wait for them to become Available after the image rebuild.
CREATE_TIMEOUT="${MEMQL_BRINGUP_CREATE_TIMEOUT:-180}"
HEALTHY_TIMEOUT="${MEMQL_BRINGUP_HEALTHY_TIMEOUT:-300}"

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
# STEP 1 -- TEAR DOWN (clean slate only, purge)
#=============================================================================

function nuke() {
    section "bringup 1/5: tearing down cluster '${CLUSTER_NAME}' (clean slate, purge)"
    bash "${SCRIPT_DIR}/down.sh" --cluster="${CLUSTER_NAME}" --purge >&2
}

#=============================================================================
# STEP 2 -- BRING UP (cluster + ArgoCD + secrets + Application)
#=============================================================================

function bringup() {
    section "bringup ${STEP_UP}: creating cluster + ArgoCD + secrets"
    bash "${SCRIPT_DIR}/up.sh" \
        --cluster="${CLUSTER_NAME}" \
        --namespace="${NAMESPACE}" \
        ${REVISION:+--revision="${REVISION}"} \
        ${SERVERS:+--servers="${SERVERS}"} \
        ${AGENTS:+--agents="${AGENTS}"} \
        ${NO_SECRETS:+--no-secrets} >&2
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
    section "bringup ${STEP_WAIT}: waiting for ArgoCD to create the app Deployments"

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
    section "bringup ${STEP_DEV}: building + importing engine images (make dev)"
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
    section "bringup ${STEP_HEALTHY}: waiting for the mesh to become Available"

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

    info "Bring-up complete. Cluster state:"
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
    CLEAN="$(cap_flag clean)"
    NO_SECRETS="$(cap_flag no-secrets)"
    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"

    if [[ -n "$CLEAN" ]]; then
        info "memQL local bring-up (clean slate)"
        STEP_UP="2/5"; STEP_WAIT="3/5"; STEP_DEV="4/5"; STEP_HEALTHY="5/5"
    else
        info "memQL local bring-up (fresh)"
        STEP_UP="1/4"; STEP_WAIT="2/4"; STEP_DEV="3/4"; STEP_HEALTHY="4/4"
    fi
    info "Cluster:   ${CLUSTER_NAME}"
    info "Namespace: ${NAMESPACE}"

    if [[ -n "$CLEAN" ]]; then
        nuke
    fi
    bringup
    wait_for_deployments_created
    rebuild_images
    wait_for_healthy

    cap_changed
    cap_result_set     cluster "$CLUSTER_NAME"
    cap_result_set     namespace "$NAMESPACE"
    cap_result_set_raw servers "$(raw_int_or_null "$SERVERS")"
    cap_result_set_raw agents  "$(raw_int_or_null "$AGENTS")"
    cap_result_set_raw cleaned "$([[ -n "$CLEAN" ]] && printf 'true' || printf 'false')"
    cap_result_set_raw rebuilt true
    cap_result_set_raw healthy "$HEALTHY"
    cap_ok
}

main "$@"
