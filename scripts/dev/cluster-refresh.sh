#!/usr/bin/env bash
#
# scripts/dev/cluster-refresh.sh
# ==============================
#
# Single-command "fresh testing stack" for the 2-replica
# staging-parity cluster (docker/docker-compose.cluster.yml), the
# cluster-topology sibling of `make dev-refresh`. Mirrors that flow --
# decrypt the operator's ~/.memql/genesis.znas using MEMQL_MASTER_KEY,
# export every env var so docker compose can substitute, wipe the
# database (clean cluster volumes), rebuild + restart, wait healthy,
# then re-seed manifest-listed entries from the decrypted env -- but on
# the BLESSED local topology (memql#1260): 2 replicas per mesh node,
# the copresent SPA, and LiveKit, all behind the single-origin nginx
# front door. Used by 'make dev-cluster-refresh' (memql#1283, epic
# memql#1259 / follows memql#1260).
#
# FRONT-DOOR DIVERGENCE (resolved memql#1283, option a):
#   `dev-refresh` / full.yml use the TLS `*.local.znas.io` nginx front
#   door (https, :443). The 2-replica cluster instead exposes a
#   plain-HTTP single-origin front door at http://localhost:8085 (HTTP)
#   + localhost:50050 (gRPC) -- the deliberate, documented divergence
#   adopted in memql#1260's cluster design. This script adapts to it:
#     - health-wait probes http://localhost:8085/healthz (the front
#       door), NOT the TLS handshake the single-node refresh waits on.
#     - the secrets seed + handshake target the cluster's gRPC front
#       door by exporting MEMQL_GRPC_ENDPOINT=localhost:50050, so
#       `scripts/secrets {health,seed}` route through connectGRPC() to
#       the cluster (plaintext, auto-selected because the endpoint has
#       no :443 suffix) instead of full.yml's bff.local.znas.io:443.
#   See docs/public/operate/reproduce-staging-locally.md for the full
#   divergence audit (the cluster front door is item-level parity with
#   staging's ingress along the routing map; only TLS termination + the
#   :443 vs :8085 host port differ locally).
#
# NO CO-TENANCY OVERRIDE: this is the owner's own run -- the full
# genesis env supplies SERVICE_NAME etc., and the owner tears down
# full.yml first, so the docker-compose.cluster.ci.yml override (which
# only drops the host-port publishes for a CI / co-tenant run alongside
# a live full.yml stack) is deliberately NOT pulled in. The cluster
# binds its real host ports (8085 / 50050 / 7880 / ...).
#
# Per repo convention (CLAUDE.md): function-based structure. Each step
# is its own function; main() invokes them in order. Reuses
# scripts/dev/lib.sh helpers (require_genesis, cleanup_sibling_compose_modes,
# wait_for_memql, check_docker, ...) rather than duplicating logic.
set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

# Cluster front door (memql#1260). The HTTP front door answers /healthz;
# the gRPC front door fronts the bff replicas for MemqlService /
# NodeService. Both are the plain-HTTP single-origin divergence from
# full.yml documented in the header above.
readonly CLUSTER_HTTP_FRONT="http://localhost:8085"
readonly CLUSTER_GRPC_ENDPOINT="localhost:50050"

# -----------------------------------------------------------------
# Steps
# -----------------------------------------------------------------

function step1_load_genesis() {
    require_genesis
    local path="${MEMQL_GENESIS_PATH:-$HOME/.memql/genesis.znas}"
    echo "[1/5] Genesis loaded from $path (decrypted to $GENESIS_ENV_FILE)."
}

function step2_point_seed_at_cluster() {
    # Route the secrets handshake + seed at the CLUSTER front door, not
    # full.yml's TLS bff.local.znas.io:443. scripts/secrets {health,seed}
    # both go through connectGRPC(), which honours MEMQL_GRPC_ENDPOINT;
    # localhost:50050 has no :443 suffix so it auto-selects plaintext --
    # exactly what the cluster's plain-HTTP front door speaks. Exported
    # into THIS shell so both step4 (health wait) and step5 (seed) see it.
    export MEMQL_GRPC_ENDPOINT="${CLUSTER_GRPC_ENDPOINT}"
    echo "[2/5] Seed + handshake endpoint -> ${CLUSTER_GRPC_ENDPOINT} (cluster gRPC front door)."
}

function step3_wipe_and_restart() {
    echo "[3/5] Wiping cockpit's cached credentials for 'local'..."
    # The DB wipe below invalidates any owner identity registered against
    # the previous boot. A stale cached token makes memql-cockpit silently
    # fail the dial post-refresh (token not yet expired -> cockpit skips
    # the login prompt; server rejects silently -> "Connecting..." forever).
    # Only the 'local' cluster's credentials are touched.
    rm -f "${HOME}/.memql/credentials/local.json"

    echo "[3/5] Cleaning up containers from sibling compose modes (full / nemoclaw)..."
    # cleanup_sibling_compose_modes tears down the full + nemoclaw stacks
    # (and the base cluster file) so a previously-running full.yml stack
    # releases the host ports the cluster binds (8085 / 50050 / 7880 /
    # 8081 / 5432). The owner is expected to have torn full.yml down, but
    # this makes the refresh self-sufficient if they forgot.
    cleanup_sibling_compose_modes
    nuke_stray_memql_containers

    echo "[3/5] Stopping the cluster + wiping volumes (incl. orphans) for a clean DB..."
    # down -v drops the named postgres volume so the cluster boots from an
    # empty DB -- the acceptance's "boots from a clean DB" precondition.
    $LIB_COMPOSE $LIB_COMPOSE_FILE_CLUSTER down -v --remove-orphans

    # Trim the BuildKit cache + dangling images to a cap BEFORE the build
    # so an unbounded cache can't fill the disk and kill `up` with "no
    # space left on device". Best-effort.
    echo "[3/5] Pruning stale Docker build cache..."
    lib_prune_docker_build_cache

    echo "[3/5] Rebuilding + starting the 2-replica parity cluster..."
    # This is heavy: 6 carrier/engine images + the copresent SPA. Allow
    # several minutes on a cold cache. The cluster compose reads the
    # decrypted genesis via env_file: ${GENESIS_ENV_FILE:-../.env.local},
    # exactly like full.yml.
    $LIB_COMPOSE $LIB_COMPOSE_FILE_CLUSTER up --build -d --remove-orphans
}

function step4_wait_for_ready() {
    # The cluster's front door is plain HTTP on :8085 (the documented
    # divergence), so we wait on its /healthz rather than the TLS gRPC
    # handshake the single-node refresh uses. /healthz answering means
    # nginx is routing to a live bff replica. We then confirm the gRPC
    # front door with the shared wait_for_memql (which now dials
    # MEMQL_GRPC_ENDPOINT=localhost:50050 thanks to step2).
    echo "[4/5] Waiting for the cluster front door (${CLUSTER_HTTP_FRONT}/healthz)..."
    local waited=0
    local front_ok=""
    while [ "$waited" -lt 600 ]; do
        if curl -fsS "${CLUSTER_HTTP_FRONT}/healthz" >/dev/null 2>&1; then
            echo "       front door healthy after ${waited}s."
            front_ok="yes"
            break
        fi
        sleep 10
        waited=$((waited + 10))
    done
    if [ -z "$front_ok" ]; then
        cat <<EOF

  WARNING: the cluster front door did not answer /healthz within 600s.
  This bring-up is heavy (6 images + the SPA). Going to attempt the
  gRPC handshake + seed anyway -- if they fail, check:
      make dev-cluster-logs                                # what's wrong
      make dev-cluster-status                              # per-replica ids
      MEMQL_GRPC_ENDPOINT=${CLUSTER_GRPC_ENDPOINT} go run ./scripts/secrets seed --env-file="$GENESIS_ENV_FILE"

EOF
    fi

    echo "[4/5] Confirming the cluster gRPC handshake on ${CLUSTER_GRPC_ENDPOINT}..."
    if ! wait_for_memql 120 3; then
        cat <<EOF

  WARNING: the cluster gRPC front door did not complete a handshake
  within 120s. Going to attempt the seed anyway -- if it fails, check
  'make dev-cluster-logs', then once the cluster is responsive re-run:
      MEMQL_GRPC_ENDPOINT=${CLUSTER_GRPC_ENDPOINT} go run ./scripts/secrets seed --env-file="$GENESIS_ENV_FILE"

EOF
    fi
}

function step5_seed_and_finish() {
    echo "[5/5] Re-seeding secrets + variables from genesis into the cluster..."
    # MEMQL_GRPC_ENDPOINT is already exported (step2) so the seed targets
    # the cluster front door, not full.yml.
    if ! go run ./scripts/secrets seed --env-file="$GENESIS_ENV_FILE"; then
        cat <<EOF

  Seed failed. The cluster is up (containers running) but the secrets
  push didn't complete. Check 'make dev-cluster-logs', then once the
  cluster is responsive re-run:

      MEMQL_GRPC_ENDPOINT=${CLUSTER_GRPC_ENDPOINT} go run ./scripts/secrets seed --env-file="$GENESIS_ENV_FILE"

EOF
        exit 1
    fi
    print_cluster_status_block
}

# print_cluster_status_block prints the cluster front-door URLs + the
# parity-litmus next step. Mirrors print_dev_status_block (lib.sh) but
# for the cluster's plain-HTTP single-origin front door.
function print_cluster_status_block() {
    cat <<EOF

  -----------------------------------------------------------
  dev-cluster-refresh complete. The 2-replica staging-parity
  cluster is up (clean DB, reseeded from genesis).

  Front door (plain HTTP single origin -- the documented #1260
  divergence from full.yml's TLS *.local.znas.io):
    http://localhost:8085        SPA + same-origin auth + /memql/ws
    http://localhost:8085/healthz  Health probe
    localhost:50050              gRPC (MemqlService / NodeService)
    ws://localhost:7880          LiveKit (dev key 'devkey' / 'secret')
    http://localhost:8081        Identity admin / login (direct)

  Parity litmus (distinct per-replica node ids):
    make dev-cluster-status

  Other handy:
    make dev-cluster-logs    Follow cluster logs
    make dev-cluster-down    Stop the cluster (keeps volumes)

  For the CoPresent frontend the SPA is already served at the front
  door above -- open http://localhost:8085 and sign in.
  -----------------------------------------------------------

EOF
}

# -----------------------------------------------------------------
# Entry
# -----------------------------------------------------------------

function step0_install_deps() {
    # Idempotent + fast when everything's already installed; only the
    # first run on a new machine actually does work.
    echo "[0/5] Verifying dev dependencies..."
    bash "${SCRIPT_DIR}/install-deps.sh" >/dev/null || {
        echo "  ERROR: dependency check failed. Run 'make install-deps' for the full diagnostic."
        exit 1
    }
}

function main() {
    check_docker
    step0_install_deps
    step1_load_genesis
    step2_point_seed_at_cluster
    # memql#374: the DB wipe forces identity to re-fire the owner
    # "Claim ownership" email; the suppress knob writes the clusterSettings
    # row but skips the issue so multi-iteration debug sessions don't stack
    # 5-10+ emails. The operator can still claim ownership via /setup.
    export MAIL_SUPPRESS_OWNER_BOOTSTRAP=1
    step3_wipe_and_restart
    step4_wait_for_ready
    step5_seed_and_finish
}

main "$@"
