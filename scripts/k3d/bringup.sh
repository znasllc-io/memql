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
#                        register the ArgoCD Application (default memql-local;
#                        downstream repos pass --app-name/--overlay-path/
#                        --repo-url to register their own).
#   3. wait           -- block until ArgoCD has created the app Deployments,
#                        so the rebuild step has something to restart.
#   4. dev            -- build engine (+ carrier) images and `k3d image
#                        import` them so pods can start instead of sitting in
#                        ImagePullBackOff (local images are tag `local`,
#                        imagePullPolicy IfNotPresent).
#   5. migrate        -- run the one-shot `memql-migrate` Job (the SAME Job the
#                        cloud deploy runs) and wait for it, so the schema is
#                        present before the mesh is declared healthy -- instead
#                        of racing identity's boot-migration fallback against a
#                        not-yet-ready Postgres (which could leave an empty DB).
#   6. wait healthy   -- block until every Deployment reports Available, then
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
# shellcheck source=../lib/localtls.sh
source "${SCRIPT_DIR}/../lib/localtls.sh"

cap_init "k3d.bringup" "Bring the local k3d + ArgoCD cluster fully up: bootstrap, build + import engine images, wait healthy. --clean nukes first."
cap_spec_param "cluster"         "k3d cluster name"
cap_spec_param "namespace"       "k8s namespace"
cap_spec_param "revision"        "git revision for the ArgoCD Application"
cap_spec_param "servers"         "k3d server node count"
cap_spec_param "agents"          "k3d agent node count"
cap_spec_param "clean"           "tear down the cluster first (clean slate, fresh DB) (flag)"
cap_spec_param "no-secrets"      "skip secret seeding (flag)"
cap_spec_param "repo-url"        "git repo URL for the ArgoCD Application (downstream repos pass their own)"
cap_spec_param "app-name"        "ArgoCD Application name (downstream repos pass their own)"
cap_spec_param "overlay-path"    "kustomize overlay path within the repo"
cap_spec_param "extra-ports"     "additional host:container loadbalancer port mappings, comma-separated"
cap_spec_param "app-project"     "ArgoCD AppProject the Application belongs to"
cap_spec_param "project-manifest" "path to the AppProject manifest to apply (downstream repos pass their own)"
cap_spec_param "carrier-repo"    "downstream carrier repo whose Dockerfile builds the carrier nodes"
cap_spec_param "carrier-nodes"   "node types built from the carrier Dockerfile, comma-separated"
cap_spec_param "carrier-context" "docker build context for carrier builds"
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
REPO_URL=""
APP_NAME=""
OVERLAY_PATH=""
EXTRA_PORTS=""
APP_PROJECT=""
PROJECT_MANIFEST=""
CARRIER_REPO=""
CARRIER_NODES=""
CARRIER_CONTEXT=""
HEALTHY=false
FRONT_DOOR_TLS=false

# Step labels, set in main() once --clean is known (6 steps clean, 5 fresh).
STEP_UP=""
STEP_WAIT=""
STEP_DEV=""
STEP_MIGRATE=""
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
    section "bringup 1/6: tearing down cluster '${CLUSTER_NAME}' (clean slate, purge)"
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
        ${REPO_URL:+--repo-url="${REPO_URL}"} \
        ${APP_NAME:+--app-name="${APP_NAME}"} \
        ${OVERLAY_PATH:+--overlay-path="${OVERLAY_PATH}"} \
        ${EXTRA_PORTS:+--extra-ports="${EXTRA_PORTS}"} \
        ${APP_PROJECT:+--app-project="${APP_PROJECT}"} \
        ${PROJECT_MANIFEST:+--project-manifest="${PROJECT_MANIFEST}"} \
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
        --namespace="${NAMESPACE}" \
        ${CARRIER_REPO:+--carrier-repo="${CARRIER_REPO}"} \
        ${CARRIER_NODES:+--carrier-nodes="${CARRIER_NODES}"} \
        ${CARRIER_CONTEXT:+--carrier-context="${CARRIER_CONTEXT}"} >&2
}

#=============================================================================
# STEP 5 -- WAIT FOR THE MESH TO BE HEALTHY
#=============================================================================

function run_migration() {
    section "bringup ${STEP_MIGRATE}: running the one-shot DB migration (memql-migrate Job)"
    local migrate_yaml="${SCRIPT_DIR}/../../deploy/k8s/base/migrate-job.yaml"
    if [[ ! -f "${migrate_yaml}" ]]; then
        warn "migrate-job.yaml not found (${migrate_yaml}); relying on identity boot-migration"
        return 0
    fi
    info "waiting for postgres to be ready..."
    kubectl -n "${NAMESPACE}" rollout status deploy/postgres --timeout=150s >&2 2>/dev/null \
        || warn "postgres not yet Available; the migrate Job will retry against it"
    kubectl -n "${NAMESPACE}" delete job memql-migrate --ignore-not-found >/dev/null 2>&1 || true
    # The SAME migrate Job the cloud deploy runs (`make deploy`), but with the
    # locally-imported identity image -- so `make up` migrates the schema up
    # front (env parity) instead of relying on identity's racy boot-migration
    # fallback (which lost to a not-yet-ready postgres and left an empty DB).
    perl -pe 's{image: acrmemql\.azurecr\.io/memql-identity:\S+}{image: memql-identity:local\n          imagePullPolicy: Never}' \
        "${migrate_yaml}" | kubectl -n "${NAMESPACE}" apply -f - >/dev/null
    info "waiting for the migration to complete..."
    if kubectl -n "${NAMESPACE}" wait --for=condition=complete --timeout=240s job/memql-migrate >/dev/null 2>&1; then
        info "DB migration complete (schema present)."
    else
        error "migrate Job did not complete. Inspect: kubectl logs -n ${NAMESPACE} job/memql-migrate"
        return 1
    fi
}

# "Every Deployment is Available" is not the whole of "the cluster is up": the
# CLIENT-FACING edge is the traefik front door, and both local ingresses
# (cockpit-front-door / identity-front-door) name the local-znas-tls secret in
# their spec.tls. When that secret is absent traefik does not fail -- it
# silently serves its own "TRAEFIK DEFAULT CERT" for both hosts, so browsers
# warn and Node clients (the VS Code extension host among them) refuse the
# connection outright, while every readiness probe in the mesh stays green.
#
# That gap is memql#3384: the run reported healthy and the front door was not
# actually a front door. So the secret is asserted here, beside the Deployments
# check, and its absence FAILS the bring-up rather than scrolling past as a
# WARN. It is a deterministic condition, not a slow-start one -- there is
# nothing to wait for and nothing to re-check.
function verify_front_door_tls() {
    if kubectl get secret "${MEMQL_LOCAL_TLS_SECRET}" -n "${NAMESPACE}" &>/dev/null; then
        info "Front-door TLS secret '${MEMQL_LOCAL_TLS_SECRET}' present in '${NAMESPACE}'."
        FRONT_DOOR_TLS=true
        return 0
    fi
    FRONT_DOOR_TLS=false
    error "Front-door TLS secret '${MEMQL_LOCAL_TLS_SECRET}' is MISSING in '${NAMESPACE}'."
    error "  Both front-door ingresses reference it; without it traefik serves its own"
    error "  self-signed cert and every TLS client sees an untrusted edge."
    error "  Re-seed it with:  make secrets    (it issues the pair with mkcert)"
}

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

    verify_front_door_tls

    info "Bring-up complete. Cluster state:"
    kubectl get deploy -n "${NAMESPACE}" >&2 2>/dev/null || true
    info "Mesh litmus:    make status"
    info "Inner loop:     make dev [NODE=<type>]"
    info "Client edge:    kubectl port-forward -n ${NAMESPACE} svc/bff 50051:50051  (Cockpit / SDKs; mcp is the MCP-protocol head)"
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
    REPO_URL="$(cap_param repo-url "${MEMQL_K3D_REPO_URL:-}")"
    APP_NAME="$(cap_param app-name "${MEMQL_K3D_APP_NAME:-}")"
    OVERLAY_PATH="$(cap_param overlay-path "${MEMQL_K3D_OVERLAY_PATH:-}")"
    EXTRA_PORTS="$(cap_param extra-ports "${MEMQL_K3D_EXTRA_PORTS:-}")"
    APP_PROJECT="$(cap_param app-project "${MEMQL_K3D_APP_PROJECT:-}")"
    PROJECT_MANIFEST="$(cap_param project-manifest "${MEMQL_K3D_PROJECT_MANIFEST:-}")"
    CARRIER_REPO="$(cap_param carrier-repo "${MEMQL_CARRIER_REPO:-}")"
    CARRIER_NODES="$(cap_param carrier-nodes "${MEMQL_CARRIER_NODES:-}")"
    CARRIER_CONTEXT="$(cap_param carrier-context "${MEMQL_CARRIER_CONTEXT:-}")"
    cap_require cluster "$CLUSTER_NAME"
    cap_require namespace "$NAMESPACE"

    if [[ -n "$CLEAN" ]]; then
        info "memQL local bring-up (clean slate)"
        STEP_UP="2/6"; STEP_WAIT="3/6"; STEP_DEV="4/6"; STEP_MIGRATE="5/6"; STEP_HEALTHY="6/6"
    else
        info "memQL local bring-up (fresh)"
        STEP_UP="1/5"; STEP_WAIT="2/5"; STEP_DEV="3/5"; STEP_MIGRATE="4/5"; STEP_HEALTHY="5/5"
    fi
    info "Cluster:   ${CLUSTER_NAME}"
    info "Namespace: ${NAMESPACE}"

    if [[ -n "$CLEAN" ]]; then
        nuke
    fi
    bringup
    wait_for_deployments_created
    rebuild_images
    run_migration
    wait_for_healthy

    cap_changed
    cap_result_set     cluster "$CLUSTER_NAME"
    cap_result_set     namespace "$NAMESPACE"
    cap_result_set_raw servers "$(raw_int_or_null "$SERVERS")"
    cap_result_set_raw agents  "$(raw_int_or_null "$AGENTS")"
    cap_result_set_raw cleaned "$([[ -n "$CLEAN" ]] && printf 'true' || printf 'false')"
    cap_result_set_raw rebuilt true
    cap_result_set_raw healthy "$HEALTHY"
    cap_result_set_raw frontDoorTls "$FRONT_DOOR_TLS"
    if [[ "$FRONT_DOOR_TLS" != "true" ]]; then
        cap_fail 5 "front-door TLS secret '${MEMQL_LOCAL_TLS_SECRET}' is missing in namespace '${NAMESPACE}' -- the local front door is serving traefik's default self-signed certificate, so https://cockpit.local.znas.io and https://identity.local.znas.io are untrusted. Run 'make secrets' to issue and seed the mkcert pair."
    fi
    cap_ok
}

# Run main only when EXECUTED, so the health assertions can be sourced and
# exercised on their own. The steps around them (nuke / up / image rebuild /
# migrate) are far too heavy to drive from a test, which is exactly how the
# front-door TLS gap went unnoticed: nothing could check the post-bring-up
# checks. Executed directly this is identical to the bare `main "$@"` every
# sibling capability script ends with.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
