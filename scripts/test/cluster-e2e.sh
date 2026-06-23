#!/usr/bin/env bash
# scripts/test/cluster-e2e.sh -- boot the 2-replica staging-parity cluster
# on k3d + ArgoCD and run the cross-replica delivery gate (test/clustere2e,
# memql#1261). Migrated off Docker Compose to k3d in memql#2068/#2088 (Epic 0,
# Argo parity #2061): the same deploy/k8s overlays + ArgoCD reconciliation that
# run in staging now run locally, so there is one cluster substrate everywhere.
#
# The gate asserts the memql#1259 delivery invariant: an utterance produced
# on one bff replica reaches a subscriber anchored on EVERY bff replica,
# exactly once. It went GREEN once the Phase-1 durable backbone landed.
#
# ==========================================================================
# OWNER VALIDATION REQUIRED -- this k3d harness is correct-by-construction but
# has NOT been run end-to-end in CI (the gate SKIPS without the owner secret,
# memql#2088). To actually run it the cluster needs k3d + kubectl on the runner
# AND -- because ArgoCD reconciles the overlay from GitHub, not the local
# checkout -- the targetRevision branch must already be PUSHED to the repo. On
# the deploy trigger (push to main) HEAD is main, which ArgoCD always has.
# For a local run on a feature branch: push the branch first, then invoke with
# MEMQL_K3D_TARGET_REVISION=<branch> (scripts/k3d/up.sh defaults it to the
# current branch). The owner accepts this as unvalidated-in-CI.
# ==========================================================================
#
# Usage:
#   # full run (boot a fresh k3d cluster, seed a JWT, run the gate):
#   bash scripts/test/cluster-e2e.sh
#   # against a cluster that is already up, reusing a known user JWT:
#   MEMQL_E2E_TOKEN=<user JWT> bash scripts/test/cluster-e2e.sh --no-build
#
# The clustere2e Go tests are driven PURELY by two env vars:
#   MEMQL_E2E_ENDPOINT  -- bff gRPC addr (this harness exposes localhost:50051)
#   MEMQL_E2E_TOKEN     -- a user JWT (auto-seeded via the identity OAuth flow)
#
# Per the repo + global Skills+Scripts convention (CLAUDE.md): function-based,
# one responsibility per function, main() at the bottom.
set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
K3D_DIR="$REPO_ROOT/scripts/k3d"

CLUSTER_NAME="${MEMQL_K3D_CLUSTER:-memql}"
NAMESPACE="${MEMQL_K3D_NAMESPACE:-memql}"

# k3d port-forward targets (see scripts/k3d/up.sh): bff gRPC + identity HTTP.
GRPC_LOCAL_PORT="${MEMQL_E2E_GRPC_PORT:-50051}"
IDENTITY_LOCAL_PORT="${MEMQL_E2E_IDENTITY_PORT:-8085}"
GRPC_ENDPOINT="${MEMQL_E2E_ENDPOINT:-localhost:${GRPC_LOCAL_PORT}}"
# Identity serves its HTTP surface over TLS in-cluster (cluster CA; see
# deploy/k8s/base/identity.yaml), so the seed flow speaks https + -k.
IDENTITY_BASE="https://localhost:${IDENTITY_LOCAL_PORT}"

# Number of bff (and every app Deployment) replicas -- 2 = staging parity.
REPLICAS="${MEMQL_E2E_REPLICAS:-2}"
# Minutes to wait for ArgoCD to sync + pods to report Ready.
READY_TIMEOUT="${MEMQL_E2E_READY_TIMEOUT:-15}"

DO_BUILD=true                                   # full bootstrap unless --no-build
DO_TEARDOWN="${MEMQL_E2E_TEARDOWN:-false}"

PF_PIDS=()                                       # port-forward background pids

#=============================================================================
# OUTPUT HELPERS
#=============================================================================

function log()  { echo "INFO: $*"; }
function warn() { echo "WARNING: $*" >&2; }
function die()  { echo "ERROR: $*" >&2; teardown_portforwards; exit 1; }

#=============================================================================
# ARGUMENTS
#=============================================================================

function parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --no-build) DO_BUILD=false; shift ;;
            --teardown) DO_TEARDOWN=true; shift ;;
            -h|--help)
                grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | head -40
                exit 0 ;;
            *) die "unknown arg: $1" ;;
        esac
    done
}

#=============================================================================
# PREREQUISITES
#=============================================================================

function check_prereqs() {
    command -v docker  >/dev/null || die "docker not found"
    command -v kubectl >/dev/null || die "kubectl not found (brew install kubectl)"
    command -v k3d     >/dev/null || die "k3d not found (brew install k3d)"
    command -v curl    >/dev/null || die "curl not found"
    docker info >/dev/null 2>&1   || die "docker daemon not running"
}

#=============================================================================
# CLUSTER BRING-UP (k3d + ArgoCD + local overlay), then scale to N replicas
#=============================================================================

function boot_cluster() {
    if [[ "$DO_BUILD" != true ]]; then
        log "Reusing the existing k3d cluster '${CLUSTER_NAME}' (--no-build)."
        kubectl config use-context "k3d-${CLUSTER_NAME}" >/dev/null 2>&1 || true
        return 0
    fi
    log "Bootstrapping k3d cluster '${CLUSTER_NAME}' (ArgoCD + ${NAMESPACE} overlay)..."
    bash "$K3D_DIR/up.sh" \
        --cluster="$CLUSTER_NAME" \
        --namespace="$NAMESPACE" \
        || die "k3d bootstrap failed (scripts/k3d/up.sh)"
}

function scale_replicas() {
    log "Scaling app Deployments to ${REPLICAS} replicas (staging parity)..."
    # scale.sh no-ops on Deployments ArgoCD has not created yet; wait_ready
    # re-scales on each pass so late-synced Deployments also reach N.
    bash "$K3D_DIR/scale.sh" "$REPLICAS" \
        --cluster="$CLUSTER_NAME" \
        --namespace="$NAMESPACE" \
        || warn "scale.sh reported an issue (some Deployments may not exist yet)"
}

#=============================================================================
# WAIT FOR ARGOCD SYNC + POD READINESS
#=============================================================================

function wait_ready() {
    log "Waiting up to ${READY_TIMEOUT}m for ArgoCD to sync + bff/identity Ready..."
    local deadline=$(( $(date +%s) + READY_TIMEOUT * 60 ))
    while (( $(date +%s) < deadline )); do
        # Re-scale on each pass so newly-synced Deployments also reach N.
        bash "$K3D_DIR/scale.sh" "$REPLICAS" \
            --cluster="$CLUSTER_NAME" --namespace="$NAMESPACE" >/dev/null 2>&1 || true

        local bff_ready id_ready
        bff_ready=$(kubectl get deployment bff -n "$NAMESPACE" \
            -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
        id_ready=$(kubectl get deployment identity -n "$NAMESPACE" \
            -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
        bff_ready=${bff_ready:-0}; id_ready=${id_ready:-0}

        if [[ "$bff_ready" -ge "$REPLICAS" && "$id_ready" -ge 1 ]]; then
            log "bff ready=${bff_ready}/${REPLICAS}, identity ready=${id_ready}. Cluster up."
            return 0
        fi
        log "  waiting (bff ready=${bff_ready}/${REPLICAS}, identity ready=${id_ready})..."
        sleep 15
    done
    kubectl get pods -n "$NAMESPACE" 2>/dev/null || true
    die "cluster did not become Ready within ${READY_TIMEOUT}m"
}

function assert_two_replicas() {
    local n
    n=$(kubectl get deployment bff -n "$NAMESPACE" \
        -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
    [[ "${n:-0}" -ge "$REPLICAS" ]] || die "expected >=${REPLICAS} ready bff replicas, found ${n:-0}"
    log "bff ready replicas: ${n} (parity OK)"
}

#=============================================================================
# PORT-FORWARDS (bff gRPC + identity HTTP -> localhost)
#=============================================================================

function start_portforwards() {
    log "Starting port-forwards: bff gRPC :${GRPC_LOCAL_PORT}, identity :${IDENTITY_LOCAL_PORT}..."
    kubectl port-forward -n "$NAMESPACE" svc/bff \
        "${GRPC_LOCAL_PORT}:50051" >/dev/null 2>&1 &
    PF_PIDS+=("$!")
    kubectl port-forward -n "$NAMESPACE" svc/identity \
        "${IDENTITY_LOCAL_PORT}:8085" >/dev/null 2>&1 &
    PF_PIDS+=("$!")
    # Give the forwards a moment to bind.
    local _attempt
    for _attempt in $(seq 1 20); do
        if curl -sk -o /dev/null "${IDENTITY_BASE}/healthz" 2>/dev/null; then
            log "Port-forwards live (identity /healthz reachable)."
            return 0
        fi
        sleep 1
    done
    warn "identity /healthz not reachable yet; continuing (seed will retry)."
}

function teardown_portforwards() {
    local pid
    for pid in "${PF_PIDS[@]:-}"; do
        [[ -n "$pid" ]] && kill "$pid" >/dev/null 2>&1 || true
    done
    PF_PIDS=()
}

#=============================================================================
# OAUTH SEED -- mint a user JWT via the identity magic-link/OAuth flow
#=============================================================================

# idcurl: curl against the port-forwarded identity over TLS (-k: dev cluster CA).
function idcurl() { curl -sk "$@"; }

# scrape_magic_link returns the most recent /auth/complete?ml=... path from
# the identity pod logs (the dev LogSender prints the email body inline).
function scrape_magic_link() {
    kubectl logs -n "$NAMESPACE" deploy/identity --since=40s --all-containers 2>/dev/null \
        | grep -oE "/auth/complete\?ml=[A-Za-z0-9_.:-]+" | tail -1
}

# seed_token: obtain a cluster user JWT for the harness.
#
# Preferred: caller exports MEMQL_E2E_TOKEN (a user JWT) and we use it as-is.
# PATs do NOT work -- the bff stream interceptor needs a JWT. So the auto-seed
# drives the real OAuth flow against identity (open registration in dev):
#   1. ensure an owner exists  -- first-run POST /setup + redeem (idempotent:
#      /setup 404s/409s once bootstrapped),
#   2. mint a JWT              -- POST /login (OAuth ctx) -> redeem the magic
#      link -> capture the auth code -> POST /oauth/token.
function seed_token() {
    if [[ -n "${MEMQL_E2E_TOKEN:-}" ]]; then
        log "Using caller-provided MEMQL_E2E_TOKEN."
        return 0
    fi
    log "Seeding a probe user JWT via the identity OAuth flow..."
    local r=$RANDOM
    local email="clustere2e+${r:-x}@local.test"
    # Local overlay registers client "app" with this callback (see
    # deploy/k8s/overlays/local/patches/identity-local-config.yaml).
    local client_id="app"
    local rURI="http://localhost:8080/auth/callback"
    local state="e2e${r:-x}"

    # 1. Ensure an owner exists (first-run only; harmless on a bootstrapped
    # cluster). Remember the setup magic link so the login scrape below can
    # tell the (already-redeemed) setup link apart from the fresh login link.
    local consumed_ml="" setup_code
    setup_code=$(idcurl -o /dev/null -w "%{http_code}" -X POST "${IDENTITY_BASE}/setup" \
        --data-urlencode "domain=local.test" \
        --data-urlencode "owner_email=$email" \
        --data-urlencode "owner_first_name=Cluster" \
        --data-urlencode "owner_last_name=E2E")
    if [[ "$setup_code" == "303" || "$setup_code" == "302" ]]; then
        local sml; sml=$(scrape_magic_link)
        [[ -n "$sml" ]] && idcurl -o /dev/null "${IDENTITY_BASE}${sml}&state=setup" >/dev/null 2>&1
        consumed_ml="$sml"
        log "Owner provisioned via /setup."
    else
        log "/setup returned $setup_code (cluster already bootstrapped); using existing owner login."
    fi

    # 2. Mint a JWT via the OAuth code flow. Retried: the magic-link email is
    # written to the identity logs asynchronously, so poll for a link that is
    # NOT the one we already consumed, and retry the whole login->redeem cycle.
    local attempt ml code="" jwt=""
    for attempt in 1 2 3; do
        idcurl -o /dev/null -X POST "${IDENTITY_BASE}/login" \
            --data-urlencode "email=$email" \
            --data-urlencode "client_id=$client_id" \
            --data-urlencode "redirect_uri=$rURI" \
            --data-urlencode "state=$state"
        ml=""
        local _attempt
        for _attempt in $(seq 1 15); do
            ml=$(scrape_magic_link)
            [[ -n "$ml" && "$ml" != "$consumed_ml" ]] && break
            ml=""
            sleep 1
        done
        if [[ -z "$ml" ]]; then
            warn "seed attempt $attempt: no fresh magic link in identity logs; retrying login"
            continue
        fi
        consumed_ml="$ml"
        code=$(idcurl -D - -o /dev/null "${IDENTITY_BASE}${ml}&state=${state}" 2>&1 \
            | grep -i "^location:" | grep -oE "code=[A-Za-z0-9_.:-]+" | head -1 | cut -d= -f2)
        [[ -n "$code" ]] && break
        warn "seed attempt $attempt: no auth code in /auth/complete redirect; retrying login"
    done
    [[ -n "$code" ]] || die "seed: no auth code after 3 login attempts"
    jwt=$(idcurl -X POST "${IDENTITY_BASE}/oauth/token" -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code\",\"client_id\":\"$client_id\",\"redirect_uri\":\"$rURI\"}" \
        | tr ',' '\n' | grep -oE "\"access_token\":\"[^\"]+\"" | head -1 | sed 's/"access_token":"//;s/"//')
    [[ -n "$jwt" ]] || die "seed: /oauth/token returned no access_token"
    export MEMQL_E2E_TOKEN="$jwt"
    log "Seeded user JWT (len ${#jwt})."
}

#=============================================================================
# RUN THE GATE
#=============================================================================

function run_gate() {
    log "Running the cross-replica delivery gate against ${GRPC_ENDPOINT}..."
    cd "$REPO_ROOT" || die "cd $REPO_ROOT"
    MEMQL_E2E_ENDPOINT="$GRPC_ENDPOINT" \
        GOWORK=off go test -tags clustere2e -count=1 -v -timeout=300s ./test/clustere2e/...
    local rc=$?
    if [[ "$rc" -ne 0 ]]; then
        warn "Gate FAILED (rc=$rc)."
    else
        log "Gate PASSED -- exactly-once cross-replica delivery."
    fi
    return $rc
}

#=============================================================================
# TEARDOWN
#=============================================================================

function teardown() {
    teardown_portforwards
    if [[ "$DO_TEARDOWN" == true ]]; then
        log "Tearing down the k3d cluster '${CLUSTER_NAME}'."
        bash "$K3D_DIR/down.sh" --cluster="$CLUSTER_NAME" || true
    fi
}

#=============================================================================
# ENTRY POINT
#=============================================================================

function main() {
    parse_args "$@"
    check_prereqs
    boot_cluster
    scale_replicas
    wait_ready
    assert_two_replicas
    start_portforwards
    seed_token
    local rc=0
    run_gate || rc=$?
    teardown
    return $rc
}

main "$@"
