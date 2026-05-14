#!/usr/bin/env bash
#
# scripts/dev/refresh.sh
# ======================
#
# Single-command "fresh testing stack" for memQL: backs up running
# state into ~/.memql/dev-secrets.yaml, wipes the database, restarts
# the cluster with the full identity flow, then re-seeds from the
# yaml. Seed + health both authenticate via the operator credential
# (cluster master key) so no `--no-auth` shortcut is needed. Used
# by 'make dev-refresh'.
#
# Per repo convention (CLAUDE.md): function-based structure. Each
# step is its own function; main() invokes them in order.
set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

# -----------------------------------------------------------------
# Steps
# -----------------------------------------------------------------

function step1_load_master_key() {
    require_master_key
    echo "[1/6] Master key loaded from yaml."
}

function step2_export_running_state() {
    # First-run / cold-start: no postgres container, no cluster to
    # back up. Print one clean line and skip both substeps. The
    # secrets `export` and knowledge-export.sh both handle the
    # missing-cluster case themselves, but their messages read like
    # warnings -- catching it here means a clean cold-start log.
    if ! docker ps --filter "name=^memql-db$" --filter "status=running" --format '{{.Names}}' | grep -q "memql-db"; then
        echo "[2/6] No running cluster detected -- skipping state + knowledge backup."
        return 0
    fi

    echo "[2/6] Backing up running memQL state into yaml..."
    go run ./scripts/secrets export

    # Knowledge cache: pulls LLM-seeded chunks + their vectors into
    # ~/.memql/dev-knowledge.sql so the upcoming wipe doesn't burn
    # them. Best-effort -- if Postgres isn't responding we skip and
    # the user gets the wipe anyway. See scripts/dev/knowledge-export.sh.
    echo "[2/6] Backing up LLM-seeded knowledge chunks..."
    bash "${SCRIPT_DIR}/knowledge-export.sh" || \
        echo "  WARNING: knowledge cache export failed; proceeding anyway."
}

function step3_wipe_and_restart() {
    echo "[3/6] Cleaning up containers from sibling compose modes..."
    cleanup_sibling_compose_modes
    nuke_stray_memql_containers

    echo "[3/6] Stopping full-mode containers + wiping volumes (incl. orphans)..."
    $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON down -v --remove-orphans

    # After the docker stack is down, anything still listening on
    # memQL's host ports is a non-docker process from outside the
    # compose stack (typically `go run main.go` left over from a
    # debugging session). Kill it now so the upcoming `compose up`
    # doesn't die with a bind() collision. See free_memql_host_ports
    # in lib.sh for which ports are touched + why postgres is excluded.
    free_memql_host_ports

    # Refresh the ngrok tunnel BEFORE compose up so bff +
    # voice-agent read the fresh LIVEKIT_PUBLIC_URL from
    # .env.local at first boot. Tearing down + re-creating keeps
    # the tunnel in lockstep with the docker stack; the free-tier
    # URL rotates per process so we'd have to re-publish it
    # anyway. Best-effort -- lib_refresh_ngrok returns non-zero
    # (handled by `|| true`) when ngrok is missing or .env.local
    # isn't shaped right, in which case Anam stays unreachable
    # this session but voice still works in audio-only.
    echo "[3/6] Refreshing ngrok tunnel for LiveKit..."
    lib_refresh_ngrok || true

    echo "[3/6] Rebuilding + starting full identity stack..."
    $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON up -d --build --remove-orphans
}

function step4_wait_for_ready() {
    local domain
    domain=$(lib_domain)
    echo "[4/6] Waiting for memQL gRPC handshake on https://bff.${domain}..."
    if ! wait_for_memql 120 3; then
        cat <<'EOF'

  WARNING: memQL did not respond to a gRPC handshake within 120s.
  Going to attempt the seed anyway -- if it fails, run:
      make dev-logs       # see what's wrong
      make secrets-seed   # seed once it comes up

EOF
    fi
}

function step5_seed_and_finish() {
    echo "[5/6] Re-seeding secrets + variables from yaml..."
    if ! go run ./scripts/secrets seed; then
        cat <<'EOF'

  Seed failed. memQL is up (containers running) but the secrets
  push didn't complete. Check 'make dev-logs' and re-run
  'make secrets-seed' once the issue is resolved.

EOF
        exit 1
    fi
}

function step6_restore_knowledge() {
    # Restore the LLM-seeded knowledge cache that step2 captured.
    # Idempotent -- chunk ids are deterministic, so a no-op on a
    # fresh run that never seeded anything yet (cache file absent).
    # Best-effort -- if it fails we still print the dev status
    # block; the user can re-run 'make knowledge-restore' manually.
    echo "[6/6] Restoring LLM-seeded knowledge chunks from cache..."
    bash "${SCRIPT_DIR}/knowledge-import.sh" || \
        echo "  WARNING: knowledge cache import failed; the dev stack is up but seed knowledge isn't restored."
    print_dev_status_block
}

# -----------------------------------------------------------------
# Entry
# -----------------------------------------------------------------

function main() {
    check_docker
    step1_load_master_key
    step2_export_running_state
    step3_wipe_and_restart
    step4_wait_for_ready
    step5_seed_and_finish
    step6_restore_knowledge
}

main "$@"
