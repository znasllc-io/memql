#!/usr/bin/env bash
#
# scripts/dev/refresh.sh
# ======================
#
# Single-command "fresh testing stack" for memQL: decrypts the
# operator's ~/.memql/genesis.znas using MEMQL_MASTER_KEY, exports
# every env var into the shell so docker compose can substitute,
# wipes the database, restarts the cluster with the full identity
# flow, then re-seeds manifest-listed entries from the decrypted
# env. Seed + health both authenticate via the operator credential
# (cluster master key) so no `--no-auth` shortcut is needed. Used
# by 'make dev-refresh'.
#
# Per repo convention (CLAUDE.md): function-based structure. Each
# step is its own function; main() invokes them in order.
set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
readonly REPO_ROOT="$( cd "${SCRIPT_DIR}/../.." && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

# -----------------------------------------------------------------
# Steps
# -----------------------------------------------------------------

function step1_load_genesis() {
    require_genesis
    local path="${MEMQL_GENESIS_PATH:-$HOME/.memql/genesis.znas}"
    echo "[1/7] Genesis loaded from $path (decrypted to $GENESIS_ENV_FILE)."
}

function step2_export_running_state() {
    # Genesis is the source of truth for secrets + variables, so the
    # cluster -> yaml backup is gone. Knowledge cache backup still
    # runs -- LLM-seeded chunks aren't in genesis and would be burned
    # by the upcoming wipe otherwise.
    if ! docker ps --filter "name=^memql-db$" --filter "status=running" --format '{{.Names}}' | grep -q "memql-db"; then
        echo "[2/7] No running cluster detected -- skipping knowledge backup."
        return 0
    fi

    echo "[2/7] Backing up LLM-seeded knowledge chunks..."
    bash "${SCRIPT_DIR}/knowledge-export.sh" || \
        echo "  WARNING: knowledge cache export failed; proceeding anyway."
}

function step3_wipe_and_restart() {
    echo "[3/7] Wiping cockpit's cached credentials for 'local'..."
    # The cluster DB wipe below invalidates any owner identity that
    # was registered against the previous boot. Leaving the cached
    # token in place causes memql-cockpit to silently fail the dial
    # post-refresh (token's expiry isn't yet reached, so cockpit
    # treats it as valid and skips the login prompt; server rejects
    # silently; user sees "Connecting to..." forever).
    #
    # Only the 'local' cluster's credentials are touched -- other
    # clusters the user has logged in to (staging, etc.) are
    # unaffected.
    rm -f "${HOME}/.memql/credentials/local.json"

    echo "[3/7] Cleaning up containers from sibling compose modes..."
    cleanup_sibling_compose_modes
    nuke_stray_memql_containers

    echo "[3/7] Stopping full-mode containers + wiping volumes (incl. orphans)..."
    $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON down -v --remove-orphans

    # After the docker stack is down, anything still listening on
    # memQL's host ports is a non-docker process from outside the
    # compose stack (typically `go run main.go` left over from a
    # debugging session). Kill it now so the upcoming `compose up`
    # doesn't die with a bind() collision. See free_memql_host_ports
    # in lib.sh for which ports are touched + why postgres is excluded.
    free_memql_host_ports

    # Refresh the ngrok tunnel BEFORE compose up so bff +
    # the Go voice-agent read the fresh LIVEKIT_PUBLIC_URL from
    # .env.local at first boot. Tearing down + re-creating keeps
    # the tunnel in lockstep with the docker stack; the free-tier
    # URL rotates per process so we'd have to re-publish it
    # anyway. Best-effort -- lib_refresh_ngrok returns non-zero
    # (handled by `|| true`) when ngrok is missing or .env.local
    # isn't shaped right, in which case Anam stays unreachable
    # this session but voice still works in audio-only.
    echo "[3/7] Refreshing ngrok tunnel for LiveKit..."
    lib_refresh_ngrok || true

    echo "[3/7] Rebuilding + starting full identity stack..."
    $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON up -d --build --remove-orphans
}

function step4_mint_voice_agent_token() {
    # The Go voice-agent crash-loops on bring-up because
    # VOICE_AGENT_TOKEN is empty -- the compose env-interpolation
    # has no shell value to pull yet. Mint one against the
    # freshly-up identity service and recreate the voice-agent
    # container so the new shell-env value lands. The Go agent reads
    # this exact env var first in integrations/voice/agent/bootstrap.go
    # (ResolveVoiceAgentToken). Production injects it from the deploy
    # pipeline's secret store the same way (see
    # docs/auth/voice-agent-jwt.md).
    echo "[4/7] Waiting for identity service to be healthy..."
    if ! wait_for_identity 90 2; then
        cat <<'EOF'

  WARNING: identity service did not report healthy within 90s.
  Skipping voice-agent-token mint; polyphon-voice-agent will
  crash-loop until you re-run 'make voice-agent-token' + recreate
  the service yourself.

EOF
        return 0
    fi

    echo "[4/7] Minting class=voice_agent JWT for polyphon-voice-agent..."
    local token
    token=$(mint_voice_agent_token "voice-agent-local" 2>/dev/null) || token=""
    if [ -z "$token" ]; then
        cat <<'EOF'

  WARNING: voice-agent-token mint returned empty. Skipping
  voice-agent recreate; the service will keep crash-looping.
  Diagnose with:
      docker exec memql-identity /app/memql voice-agent-token mint \
          --instance-id=voice-agent-local

EOF
        return 0
    fi

    # Export into THIS shell so the upcoming `up -d` re-evaluates
    # the ${VOICE_AGENT_TOKEN:-} interpolation in
    # docker-compose.polyphon.yml with the fresh value. `restart`
    # would not work -- compose bakes env interpolation at `up` /
    # `recreate` time, not at restart.
    export VOICE_AGENT_TOKEN="$token"

    echo "[4/7] Recreating polyphon-voice-agent with the fresh token..."
    $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON up -d --no-deps --force-recreate voice-agent
}

function step4b_mint_node_tokens() {
    # Same chicken-and-egg as voice-agent-token: the cluster-node
    # binaries (bff / voice / cognition / agent / planner) come up
    # with MEMQL_NODE_TOKEN empty because the per-service
    # ${MEMQL_<TYPE>_NODE_TOKEN:-} interpolation has no shell value
    # yet. Without it the receiving NodeService.Stream interceptor
    # rejects every peer call with `authorization header missing`
    # and chat / greet / dispatch all silently fail. memql#338.
    #
    # Mint per-node tokens via the identity service, source the
    # written snippet into THIS shell, and recreate the cluster
    # nodes so the upcoming `up -d` re-evaluates the
    # `${MEMQL_<TYPE>_NODE_TOKEN:-}` interpolation with the fresh
    # values. The identity service is already healthy from step4's
    # wait_for_identity check.
    echo "[4b/7] Minting class=node JWTs for cluster-node services..."
    if ! bash "${SCRIPT_DIR}/mint-node-tokens.sh"; then
        cat <<'EOF'

  WARNING: node-token mint failed. The cluster nodes will keep
  rejecting peer calls (`authorization header missing`); chat and
  greet-on-join won't work until you re-run the mint manually:
      make dev-node-tokens-bootstrap
      source .env.local.node-tokens
      docker compose -f docker/docker-compose.full.yml \
          up -d --force-recreate bff voice cognition agent planner

EOF
        return 0
    fi

    # Export the freshly-minted tokens into THIS shell so the
    # upcoming `up -d` re-evaluates the
    # `${MEMQL_<TYPE>_NODE_TOKEN:-}` interpolation with the new
    # values. `restart` would not work -- compose bakes env
    # interpolation at `up` / `recreate` time, not at restart.
    # shellcheck disable=SC1091
    set -a
    source "${REPO_ROOT}/.env.local.node-tokens"
    set +a

    echo "[4b/7] Recreating cluster nodes with the fresh tokens..."
    $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON up -d --force-recreate \
        bff voice cognition agent planner
}

function step5_wait_for_ready() {
    local domain
    domain=$(lib_domain)
    echo "[5/7] Waiting for memQL gRPC handshake on https://bff.${domain}..."
    if ! wait_for_memql 120 3; then
        cat <<'EOF'

  WARNING: memQL did not respond to a gRPC handshake within 120s.
  Going to attempt the seed anyway -- if it fails, check:
      make dev-logs                                       # what's wrong
      go run ./scripts/secrets seed --env-file=...        # once memQL is up

EOF
    fi
}

function step6_seed_and_finish() {
    echo "[6/7] Re-seeding secrets + variables from genesis..."
    if ! go run ./scripts/secrets seed --env-file="$GENESIS_ENV_FILE"; then
        cat <<EOF

  Seed failed. memQL is up (containers running) but the secrets
  push didn't complete. Check 'make dev-logs', then once memQL is
  responsive re-run:

      go run ./scripts/secrets seed --env-file="$GENESIS_ENV_FILE"

EOF
        exit 1
    fi
}

function step7_restore_knowledge() {
    # Restore the LLM-seeded knowledge cache that step2 captured.
    # Idempotent -- chunk ids are deterministic, so a no-op on a
    # fresh run that never seeded anything yet (cache file absent).
    # Best-effort -- if it fails we still print the dev status
    # block; the user can re-run 'make knowledge-restore' manually.
    echo "[7/7] Restoring LLM-seeded knowledge chunks from cache..."
    bash "${SCRIPT_DIR}/knowledge-import.sh" || \
        echo "  WARNING: knowledge cache import failed; the dev stack is up but seed knowledge isn't restored."
    print_dev_status_block
}

# -----------------------------------------------------------------
# Entry
# -----------------------------------------------------------------

function step0_install_deps() {
    # Idempotent + fast when everything's already installed; only the
    # first run on a new machine actually does work. See
    # scripts/dev/install-deps.sh -- installs protoc + protoc-gen-go
    # plugins, verifies go/docker/mkcert are present.
    echo "[0/7] Verifying dev dependencies..."
    bash "${SCRIPT_DIR}/install-deps.sh" >/dev/null || {
        echo "  ERROR: dependency check failed. Run 'make install-deps' for the full diagnostic."
        exit 1
    }
}

function main() {
    check_docker
    step0_install_deps
    step1_load_genesis
    step2_export_running_state
    # memql#374: dev-refresh wipes the postgres volume on every run,
    # which forces identity to re-fire the owner "Claim ownership" email.
    # Multi-iteration debug sessions stack 5-10+ emails in the inbox.
    # The MAIL_SUPPRESS_OWNER_BOOTSTRAP knob in attemptAutoBootstrap
    # writes the clusterSettings row but skips the mlIssuer.Issue call;
    # the operator can still claim ownership via /setup if they need to.
    export MAIL_SUPPRESS_OWNER_BOOTSTRAP=1
    # copresent#237: auto-stamp the per-user Assistant's avatar persona on first
    # materialization (i.e. on login, since dev-refresh wipes the DB + suppresses
    # owner bootstrap) so the assistant's Anam avatar renders with zero manual
    # steps. The SeedMaterializer resolves this NAME against the
    # v1:agents:avatarPersona catalog at materialization time, so it stays
    # deployment-agnostic (minted persona ids vary per vendor account). Override
    # by exporting MEMQL_DEV_DEFAULT_AVATAR_PERSONA before `make dev-refresh`;
    # set it empty to opt out.
    export MEMQL_DEV_DEFAULT_AVATAR_PERSONA="${MEMQL_DEV_DEFAULT_AVATAR_PERSONA:-Ava}"
    step3_wipe_and_restart
    step4_mint_voice_agent_token
    step4b_mint_node_tokens
    step5_wait_for_ready
    step6_seed_and_finish
    step7_restore_knowledge
}

main "$@"
