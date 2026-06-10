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

function boot_cluster() {
    cd "$REPO_ROOT" || die "cd $REPO_ROOT"
    if [[ "$DO_BUILD" == true ]]; then
        log "Building + starting the 2-replica parity cluster (port-isolated). This is heavy on a cold cache."
        # shellcheck disable=SC2086
        docker compose $COMPOSE_BASE up --build -d || die "cluster bring-up failed"
    else
        log "Starting the cluster (no rebuild)."
        # shellcheck disable=SC2086
        docker compose $COMPOSE_BASE up -d --no-build || die "cluster start failed"
    fi
}

function wait_healthy() {
    log "Waiting for the front door + identity to accept..."
    local i
    for i in $(seq 1 60); do
        if curl -fsS "$HTTP_FRONT/healthz" >/dev/null 2>&1; then
            log "Front door healthy after ${i}0s."
            return 0
        fi
        sleep 10
    done
    die "cluster did not become healthy in 10m; check 'docker compose $COMPOSE_BASE logs'"
}

function assert_two_replicas() {
    local n
    n="$(docker compose $COMPOSE_BASE ps -q bff 2>/dev/null | wc -l | tr -d ' ')"
    [[ "$n" -ge 2 ]] || die "expected >=2 bff replicas, found $n -- the gate needs the parity topology"
    log "bff replicas: $n (parity OK)"
}

# seed_token: obtain a cluster user token for the harness.
#
# Preferred: caller exports MEMQL_E2E_TOKEN (a PAT/JWT) and we use it as-is.
#
# Fallback (best-effort; validated on first real cluster boot, memql#1261):
# the CI override runs identity in `open` registration, so a probe user can
# be created via the first-run /setup owner path and a PAT minted with
# `memql pat mint`. The exact wiring is finalised against a live cluster;
# until then, pass MEMQL_E2E_TOKEN explicitly.
function seed_token() {
    if [[ -n "${MEMQL_E2E_TOKEN:-}" ]]; then
        log "Using caller-provided MEMQL_E2E_TOKEN."
        return 0
    fi
    warn "MEMQL_E2E_TOKEN not provided; attempting best-effort seed via the identity container."
    local idc
    idc="$(docker compose $COMPOSE_BASE ps -q identity 2>/dev/null | head -1)"
    [[ -n "$idc" ]] || die "identity container not found for seed"
    # The first-run /setup owner-mint + PAT path is finalised against a live
    # cluster (see memql#1261 design note). Surface a clear actionable error
    # rather than a half-working seed.
    die "auto-seed not yet validated against a live 2-replica cluster. Re-run with MEMQL_E2E_TOKEN=<PAT/JWT>. (memql#1261)"
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
