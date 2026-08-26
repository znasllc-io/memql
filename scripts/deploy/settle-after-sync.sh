#!/usr/bin/env bash
#
# scripts/deploy/settle-after-sync.sh
# ===================================
#
# Capability: deploy.settleAfterSync -- wait for the database, clear the
# accumulated CrashLoopBackOff on the mesh, and only THEN report sync health.
#
# Backend for the `settleAfterSync` deployment action (memql#4475, epic
# memql#4490).
#
# THE FIRST SYNC LEGITIMATELY FAILS, AND THAT IS NOT A DEFECT. On a real
# bring-up the first ArgoCD sync fails while the database is still initialising.
# Mesh pods CrashLoopBackOff until Postgres is up -- correctly, they cannot run
# without it -- and then they need a RESTART, because Kubernetes will not retry
# promptly once the backoff delay has grown. A pod that has reached a five
# minute backoff sits there for five minutes after the thing it was waiting for
# became available.
#
# WHY THIS NEEDS ITS OWN STEP RATHER THAN A LONGER TIMEOUT. A lifecycle step
# that reads the first `Degraded` as failure is wrong on ALMOST EVERY
# SUCCESSFUL bring-up, and the natural reaction to that -- retrying the sync --
# does not help, because what is needed is TIME plus a RESTART, and a re-sync
# supplies neither. Retrying is the wrong instinct arrived at honestly, which
# is exactly the kind of thing worth encoding once.
#
# The order is therefore fixed and each part earns its place:
#
#   1. WAIT for the CNPG Cluster to report a ready instance. Not for a pod
#      Running -- CNPG's initdb runs inside a Running pod, and a database that
#      is still bootstrapping accepts no connections.
#   2. RESTART the mesh Deployments. This resets the backoff clock; it is the
#      part a longer timeout cannot substitute for.
#   3. WAIT for the rollouts.
#   4. ONLY THEN report health.
#
# IT REPORTS, IT DOES NOT DECIDE. `passed`, `health` and `sync` come back in
# the envelope and the automation's logic branches on them (contract rule 7). A
# non-zero exit means the settle itself could not run.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok (settle ran; read `passed`) | 2 bad param | 4 prerequisite missing | 5 the settle failed
#
# Refs: memql#4490 memql#4475 memql#2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.settleAfterSync" \
    "Wait for the database, restart the mesh to clear backoff, then report sync health."

cap_spec_param "namespace"      "namespace the instance runs in (default memql)"
cap_spec_param "dbCluster"      "CNPG Cluster to wait for (default memql-db)"
cap_spec_param "argoNamespace"  "namespace ArgoCD runs in (default argocd)"
cap_spec_param "app"            "ArgoCD Application to report health for (default memql)"
cap_spec_param "deployments"    "comma-separated mesh Deployments to restart (default: the mesh set)"
cap_spec_param "dbTimeoutSeconds"    "how long to wait for the database (default 900 -- initdb plus a first WAL archive is minutes, not seconds)"
cap_spec_param "rolloutTimeoutSeconds" "how long to wait for each restarted Deployment (default 300)"
cap_spec_param "dryRun"         "plan only; wait for nothing and restart nothing"

cap_handle_meta "$@"
cap_parse_flags "$@"

NAMESPACE="$(cap_param namespace "memql")"
DB_CLUSTER="$(cap_param dbCluster "memql-db")"
ARGO_NS="$(cap_param argoNamespace "argocd")"
APP="$(cap_param app "memql")"
DEPLOYMENTS="$(cap_param deployments "")"
DB_TIMEOUT="$(cap_param dbTimeoutSeconds "900")"
ROLLOUT_TIMEOUT="$(cap_param rolloutTimeoutSeconds "300")"
DRY_RUN="$(cap_bool_str dryRun false)"

readonly DEFAULT_DEPLOYMENTS="identity,bff,cognition,agent,planner,workbench,edge"

DB_READY="false"
RESTARTED=""
NOT_READY=""
HEALTH=""
SYNC=""

function check_prerequisites() {
    command -v kubectl &>/dev/null || cap_fail 4 "kubectl is not installed or not on PATH"
    kubectl cluster-info &>/dev/null || cap_fail 4 "no reachable Kubernetes API -- fetch a kubeconfig first"
}

function validate_arguments() {
    [[ "$DB_TIMEOUT"      =~ ^[0-9]+$ ]] || cap_fail 2 "--dbTimeoutSeconds must be an integer, got ${DB_TIMEOUT}"
    [[ "$ROLLOUT_TIMEOUT" =~ ^[0-9]+$ ]] || cap_fail 2 "--rolloutTimeoutSeconds must be an integer, got ${ROLLOUT_TIMEOUT}"
}

# ---- 1. the database ---------------------------------------------------------

function wait_for_database() {
    if [[ "$DRY_RUN" == "true" ]]; then
        cap_info "DRY RUN: would wait up to ${DB_TIMEOUT}s for CNPG Cluster ${DB_CLUSTER} to report a ready instance"
        DB_READY="true"
        return 0
    fi
    if ! kubectl get cluster.postgresql.cnpg.io "$DB_CLUSTER" -n "$NAMESPACE" -o name &>/dev/null; then
        cap_warn "CNPG Cluster ${DB_CLUSTER} does not exist in ${NAMESPACE} yet -- the sync may not have created it. Not waiting."
        return 0
    fi

    cap_step "waiting up to ${DB_TIMEOUT}s for ${DB_CLUSTER} to report a ready instance"
    local deadline=$((SECONDS + DB_TIMEOUT)) ready phase
    while (( SECONDS < deadline )); do
        # readyInstances, not pod phase: CNPG's initdb runs inside a pod that is
        # already Running, and a database mid-bootstrap accepts no connections.
        ready="$(kubectl get cluster.postgresql.cnpg.io "$DB_CLUSTER" -n "$NAMESPACE" \
            -o jsonpath='{.status.readyInstances}' 2>/dev/null || echo "")"
        if [[ -n "$ready" && "$ready" -ge 1 ]]; then
            DB_READY="true"
            cap_info "${DB_CLUSTER} reports ${ready} ready instance(s)"
            return 0
        fi
        phase="$(kubectl get cluster.postgresql.cnpg.io "$DB_CLUSTER" -n "$NAMESPACE" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")"
        cap_info "database not ready yet (phase: ${phase:-unknown}); waiting"
        sleep 10
    done
    cap_warn "${DB_CLUSTER} did not report a ready instance within ${DB_TIMEOUT}s. Restarting the mesh now would only rebuild the same backoff, so the restart is skipped."
}

# ---- 2 + 3. clear the backoff -----------------------------------------------

function restart_mesh() {
    if [[ "$DB_READY" != "true" ]]; then
        cap_info "skipping the mesh restart: the thing the pods are waiting for is not up"
        return 0
    fi

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
        # A Deployment deliberately at zero replicas (a lane held off) has no
        # backoff to clear, and rolling it would report a rollout that never
        # completes.
        local want
        want="$(kubectl get deployment "$d" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)"
        if [[ "${want:-0}" == "0" ]]; then
            cap_info "deployment ${d} is at 0 replicas (deliberately off); nothing to clear"
            continue
        fi
        if [[ "$DRY_RUN" == "true" ]]; then
            cap_info "DRY RUN: would restart ${d} to clear accumulated backoff"
            continue
        fi
        cap_step "restarting ${d} to clear accumulated backoff"
        kubectl rollout restart "deployment/${d}" -n "$NAMESPACE" >/dev/null \
            || cap_fail 5 "failed to restart deployment ${d}"
        RESTARTED="${RESTARTED:+${RESTARTED},}${d}"
        cap_changed
    done

    [[ "$DRY_RUN" == "true" ]] && return 0
    for d in ${RESTARTED//,/ }; do
        if ! kubectl -n "$NAMESPACE" rollout status "deployment/${d}" --timeout="${ROLLOUT_TIMEOUT}s" >/dev/null 2>&1; then
            NOT_READY="${NOT_READY:+${NOT_READY},}${d}"
            cap_warn "deployment ${d} did not become available within ${ROLLOUT_TIMEOUT}s after the restart"
        fi
    done
}

# ---- 4. and only NOW look at health -----------------------------------------

function read_sync_health() {
    if [[ "$DRY_RUN" == "true" ]]; then
        HEALTH="DryRun"; SYNC="DryRun"
        return 0
    fi
    if ! kubectl get application "$APP" -n "$ARGO_NS" -o name &>/dev/null; then
        cap_warn "ArgoCD Application ${APP} not found in ${ARGO_NS}; reporting no health"
        return 0
    fi
    HEALTH="$(kubectl get application "$APP" -n "$ARGO_NS" -o jsonpath='{.status.health.status}' 2>/dev/null || echo "")"
    SYNC="$(kubectl get application "$APP" -n "$ARGO_NS" -o jsonpath='{.status.sync.status}' 2>/dev/null || echo "")"
    cap_info "after settling: sync=${SYNC:-unknown} health=${HEALTH:-unknown}"
}

function collect_result() {
    local passed="false"
    # Healthy is not enough on its own: an Application can report Healthy while
    # OutOfSync, pinned to a revision that no longer exists, reconciling
    # nothing. Synced is the field that answers the question being asked.
    if [[ "$DB_READY" == "true" && "$HEALTH" == "Healthy" && "$SYNC" == "Synced" && -z "$NOT_READY" ]]; then
        passed="true"
    fi
    [[ "$DRY_RUN" == "true" ]] && passed="true"

    cap_result_set_raw "passed"   "$passed"
    cap_result_set_raw "dbReady"  "$DB_READY"
    cap_result_set "health"       "$HEALTH"
    cap_result_set "sync"         "$SYNC"
    cap_result_set "restarted"    "$RESTARTED"
    cap_result_set "notReady"     "$NOT_READY"
    cap_result_set "dryRun"       "$DRY_RUN"
    return 0
}

function main() {
    validate_arguments
    check_prerequisites

    cap_info "settling ${NAMESPACE} after the first sync -- the first Degraded is normal, not a broken install"
    wait_for_database
    restart_mesh
    read_sync_health

    collect_result
    cap_ok
}

main "$@"
