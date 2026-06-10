#!/usr/bin/env bash
# scripts/test/cluster-e2e.sh -- boot the 2-replica staging-parity cluster
# and run the cross-replica delivery gate (test/clustere2e, memql#1261).
#
# The gate asserts the memql#1259 delivery invariant: an utterance produced
# on one bff replica reaches a subscriber anchored on EVERY bff replica,
# exactly once. It is expected RED on current main and green once the
# Phase-1 durable backbone lands.
#
# Usage:
#   MEMQL_PACKAGES_TOKEN=ghp_... bash scripts/test/cluster-e2e.sh
#   # or, if you already have a cluster user token:
#   MEMQL_E2E_TOKEN=mql_pat_... bash scripts/test/cluster-e2e.sh --no-build
#
# Co-tenant safe: the CI override (docker-compose.cluster.ci.yml) drops the
# host port publishes that would collide with a running full.yml stack, so
# this can run next to single-node dev. Front door stays on :8085 / :50050.
set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_BASE="-f docker/docker-compose.cluster.yml -f docker/docker-compose.cluster.ci.yml"
# The delivery gate is SYNTHETIC (injects utterances over gRPC) -- it only needs
# the cross-replica bff mesh + auth, NOT the SPA/voice/livekit (which need genesis
# secrets a CI runner does not have). Boot just this subset.
GATE_SERVICES="postgres identity bff nginx"
PROJECT="memql-cluster-multinode" # matches `name:` in the base compose
GRPC_ENDPOINT="${MEMQL_E2E_ENDPOINT:-localhost:50050}"
HTTP_FRONT="http://localhost:8085"
SEED_EMAIL="${MEMQL_E2E_SEED_EMAIL:-clustere2e@local.test}"
DO_BUILD=true
DO_TEARDOWN="${MEMQL_E2E_TEARDOWN:-false}"

#=============================================================================
# FUNCTIONS
#=============================================================================

function log()  { echo "INFO: $*"; }
function warn() { echo "WARNING: $*" >&2; }
function die()  { echo "ERROR: $*" >&2; exit 1; }

function parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --no-build) DO_BUILD=false; shift ;;
            --teardown) DO_TEARDOWN=true; shift ;;
            -h|--help)
                grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | head -25
                exit 0 ;;
            *) die "unknown arg: $1" ;;
        esac
    done
}

function check_prereqs() {
    command -v docker >/dev/null || die "docker not found"
    docker compose version >/dev/null 2>&1 || die "docker compose v2 required"
    [[ -d "$REPO_ROOT/../copresent" ]] || warn "../copresent sibling not found -- the SPA build will fail"
    if [[ "$DO_BUILD" == true && -z "${MEMQL_PACKAGES_TOKEN:-}" ]]; then
        warn "MEMQL_PACKAGES_TOKEN unset -- the copresent SPA image build needs it (read:packages, both scopes)"
    fi
}

function ensure_env_local() {
    # The cluster compose has `env_file: ${GENESIS_ENV_FILE:-../.env.local}`, a hard
    # requirement. On a CI runner / co-tenant checkout there is no genesis decrypt,
    # but the synthetic gate needs no genesis secrets (the ci override injects
    # SERVICE_NAME + open registration; the DSN / bootstrap token are inline with
    # defaults). Create a minimal placeholder if absent so the boot does not fail on
    # a missing env file. Never clobber a real local one.
    local f="$REPO_ROOT/.env.local"
    if [[ ! -f "$f" ]]; then
        log "No $f -- writing a minimal placeholder for the gate (no genesis secrets needed)."
        printf '# minimal placeholder for cluster-e2e (memql#1273); see docker-compose.cluster.ci.yml\n' > "$f"
    fi

    # The carrier (bff) build context is the workspace ROOT (../.. from docker/),
    # where the Dockerfile `COPY go.work ./` expects the workspace tie-file. That
    # file is a local-only artifact (not in any repo), so a clean CI checkout lacks
    # it. Write the same 3-module go.work the local workspace uses; the carrier
    # Dockerfile stubs the missing memql-cockpit module itself. Never clobber a real
    # local one.
    local w="$REPO_ROOT/../go.work"
    if [[ ! -f "$w" ]]; then
        log "No $w -- writing the workspace go.work for the carrier build."
        {
            echo "go 1.26.3"
            echo ""
            echo "use ("
            echo "./memql"
            echo "./memql-bff-copresent"
            echo "./memql-cockpit"
            echo ")"
        } > "$w"
    fi
}

function boot_cluster() {
    cd "$REPO_ROOT" || die "cd $REPO_ROOT"
    ensure_env_local
    if [[ "$DO_BUILD" == true ]]; then
        log "Building + starting the delivery subset ($GATE_SERVICES), port-isolated. Heavy on a cold cache."
        # shellcheck disable=SC2086
        docker compose $COMPOSE_BASE up --build -d $GATE_SERVICES || die "cluster bring-up failed"
    else
        log "Starting the delivery subset ($GATE_SERVICES) (no rebuild)."
        # shellcheck disable=SC2086
        docker compose $COMPOSE_BASE up -d --no-build $GATE_SERVICES || die "cluster start failed"
    fi
}

function wait_healthy() {
    log "Waiting for the bff replicas + identity to report healthy..."
    local i
    for i in $(seq 1 60); do
        local healthy=0 cid
        # shellcheck disable=SC2086
        for cid in $(docker compose $COMPOSE_BASE ps -q bff identity 2>/dev/null); do
            [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid" 2>/dev/null)" == "healthy" ]] && healthy=$((healthy+1))
        done
        if [[ "$healthy" -ge 3 ]]; then   # 2 bff replicas + identity
            log "bff replicas + identity healthy after ${i}0s."
            return 0
        fi
        sleep 10
    done
    # shellcheck disable=SC2086
    die "cluster did not become healthy in 10m; check 'docker compose $COMPOSE_BASE logs'"
}

function assert_two_replicas() {
    local n
    n="$(docker compose $COMPOSE_BASE ps -q bff 2>/dev/null | wc -l | tr -d ' ')"
    [[ "$n" -ge 2 ]] || die "expected >=2 bff replicas, found $n -- the gate needs the parity topology"
    log "bff replicas: $n (parity OK)"
}

CLUSTER_NET="${PROJECT}_memql-cluster" # compose network for the curl sidecar

# curl_id runs curl inside a throwaway container on the cluster network so it
# can reach identity by service name (its host port is dropped for co-tenancy).
function curl_id() { docker run --rm --network "$CLUSTER_NET" curlimages/curl:latest -s "$@"; }

# scrape_magic_link returns the most recent /auth/complete?ml=... path from
# the identity logs (the dev LogSender prints the email body, link included).
function scrape_magic_link() {
    docker compose $COMPOSE_BASE logs identity --since 40s 2>&1 \
        | grep -oE "/auth/complete\?ml=[A-Za-z0-9_.:-]+" | tail -1
}

# seed_token: obtain a cluster user JWT for the harness.
#
# Preferred: caller exports MEMQL_E2E_TOKEN (a user JWT) and we use it as-is.
# PATs do NOT work -- the bff stream interceptor doesn't wire the PAT path; it
# needs a JWT. So the auto-seed drives the real OAuth flow against identity
# (which the CI override runs in `open` registration):
#   1. ensure an owner exists  -- first-run POST /setup + redeem (idempotent:
#      /setup 404s once bootstrapped),
#   2. mint a JWT              -- POST /login (OAuth ctx) -> redeem the magic
#      link -> capture the auth code -> POST /oauth/token.
function seed_token() {
    if [[ -n "${MEMQL_E2E_TOKEN:-}" ]]; then
        log "Using caller-provided MEMQL_E2E_TOKEN."
        return 0
    fi
    log "Seeding a probe user JWT via the identity OAuth flow..."
    local idbase="http://identity:8081"
    local r=$RANDOM
    local email="clustere2e+${r:-x}@local.test"
    local rURI="http://localhost:8085/auth/callback"
    local state="e2e${r:-x}"

    # 1. Ensure an owner exists (first-run only; harmless 404 afterward).
    local setup_code
    setup_code=$(curl_id -o /dev/null -w "%{http_code}" -X POST "$idbase/setup" \
        --data-urlencode "domain=local.test" \
        --data-urlencode "owner_email=$email" \
        --data-urlencode "owner_first_name=Cluster" \
        --data-urlencode "owner_last_name=E2E")
    if [[ "$setup_code" == "303" || "$setup_code" == "302" ]]; then
        local sml; sml=$(scrape_magic_link)
        [[ -n "$sml" ]] && curl_id -o /dev/null "${idbase}${sml}&state=setup" >/dev/null 2>&1
        log "Owner provisioned via /setup."
    else
        log "/setup returned $setup_code (cluster already bootstrapped); using existing owner login."
    fi

    # 2. Mint a JWT via the OAuth code flow.
    curl_id -o /dev/null -X POST "$idbase/login" \
        --data-urlencode "email=$email" \
        --data-urlencode "client_id=copresent" \
        --data-urlencode "redirect_uri=$rURI" \
        --data-urlencode "state=$state"
    local ml; ml=$(scrape_magic_link)
    [[ -n "$ml" ]] || die "seed: no magic link found in identity logs (login may have failed)"
    local code
    code=$(curl_id -D - -o /dev/null "${idbase}${ml}&state=${state}" 2>&1 \
        | grep -i "^location:" | grep -oE "code=[A-Za-z0-9_.:-]+" | head -1 | cut -d= -f2)
    [[ -n "$code" ]] || die "seed: no auth code in /auth/complete redirect"
    local jwt
    jwt=$(curl_id -X POST "$idbase/oauth/token" -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code\",\"client_id\":\"copresent\",\"redirect_uri\":\"$rURI\"}" \
        | tr ',' '\n' | grep -oE "\"access_token\":\"[^\"]+\"" | head -1 | sed 's/"access_token":"//;s/"//')
    [[ -n "$jwt" ]] || die "seed: /oauth/token returned no access_token"
    export MEMQL_E2E_TOKEN="$jwt"
    log "Seeded user JWT (len ${#jwt})."
}

function run_gate() {
    log "Running the cross-replica delivery gate..."
    cd "$REPO_ROOT" || die "cd $REPO_ROOT"
    MEMQL_E2E_ENDPOINT="$GRPC_ENDPOINT" \
        GOWORK=off go test -tags clustere2e -count=1 -v -timeout=300s ./test/clustere2e/...
    local rc=$?
    if [[ "$rc" -ne 0 ]]; then
        warn "Gate FAILED (rc=$rc). On current main this is EXPECTED -- it reproduces the memql#1259 delivery drop."
    else
        log "Gate PASSED -- exactly-once cross-replica delivery."
    fi
    return $rc
}

function teardown() {
    if [[ "$DO_TEARDOWN" == true ]]; then
        log "Tearing down the cluster."
        # shellcheck disable=SC2086
        docker compose $COMPOSE_BASE down
    fi
}

function main() {
    parse_args "$@"
    check_prereqs
    boot_cluster
    wait_healthy
    assert_two_replicas
    seed_token
    local rc=0
    run_gate || rc=$?
    teardown
    return $rc
}

main "$@"
