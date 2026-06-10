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
# FRONT DOOR (memql#1313): the cluster now serves at the TLS
#   `*.${IDENTITY_BOOTSTRAP_DOMAIN}` subdomains (https, :443) -- parity
#   with staging's per-subdomain ingress AND the proven dev TLS front
#   door it ported (memql#1311):
#     app.${DOMAIN}       -> copresent SPA
#     identity.${DOMAIN}  -> identity (auth, /admin, /setup, JWKS)
#     bff.${DOMAIN}       -> BFF gRPC + HTTP (/memql/ws, attachments, healthz)
#     agent.${DOMAIN}     -> Agent gRPC (WorkerService.Stream)
#     livekit.${DOMAIN}   -> LiveKit signaling
#   `*.${DOMAIN}` resolves to 127.0.0.1 via real DNS (no /etc/hosts).
#   This script adapts to the TLS front door:
#     - it generates the wildcard mkcert cert (scripts/dev/setup-tls.sh)
#       BEFORE the compose up so nginx has a cert to load;
#     - health-wait probes https://bff.${DOMAIN}/healthz (the front door);
#     - the secrets seed + handshake target the cluster's gRPC front door
#       by exporting MEMQL_GRPC_ENDPOINT=bff.${DOMAIN}:443, so
#       `scripts/secrets {health,seed}` route through connectGRPC() over
#       TLS (auto-selected by the :443 suffix) against the mkcert-trusted
#       cert.
#   See docs/public/operate/reproduce-staging-locally.md for the full
#   parity audit.
#
# NO CO-TENANCY OVERRIDE: this is the owner's own run -- the full genesis
# env supplies SERVICE_NAME etc.; the docker-compose.cluster.ci.yml
# override (which only drops the host-port publishes for a CI / co-tenant
# run) is deliberately NOT pulled in. The cluster binds its real host
# ports (80 / 443 / 7880 / ...).
#
# Per repo convention (CLAUDE.md): function-based structure. Each step
# is its own function; main() invokes them in order. Reuses
# scripts/dev/lib.sh helpers (require_genesis, cleanup_sibling_compose_modes,
# wait_for_memql, check_docker, ...) rather than duplicating logic.
set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

# Cluster TLS front door (memql#1313). bff.${DOMAIN} answers /healthz and
# fronts the bff replicas for MemqlService / NodeService gRPC. The :443
# suffix makes scripts/secrets dial TLS. These are filled in by
# step1b_resolve_front_door() AFTER genesis is loaded (so the genesis
# IDENTITY_BOOTSTRAP_DOMAIN wins), via lib_domain (IDENTITY_BOOTSTRAP_DOMAIN
# > .env.local > local.znas.io).
CLUSTER_DOMAIN=""
CLUSTER_HTTP_FRONT=""
CLUSTER_GRPC_ENDPOINT=""

# -----------------------------------------------------------------
# Steps
# -----------------------------------------------------------------

function step1_load_genesis() {
    require_genesis
    local path="${MEMQL_GENESIS_PATH:-$HOME/.memql/genesis.znas}"
    echo "[1/6] Genesis loaded from $path (decrypted to $GENESIS_ENV_FILE)."
    # Resolve the front-door domain AFTER genesis is loaded so a genesis
    # IDENTITY_BOOTSTRAP_DOMAIN wins over .env.local / the default.
    CLUSTER_DOMAIN="$(lib_domain)"
    CLUSTER_HTTP_FRONT="https://bff.${CLUSTER_DOMAIN}"
    CLUSTER_GRPC_ENDPOINT="bff.${CLUSTER_DOMAIN}:443"
    readonly CLUSTER_DOMAIN CLUSTER_HTTP_FRONT CLUSTER_GRPC_ENDPOINT
    echo "[1/6] Front door -> *.${CLUSTER_DOMAIN} (TLS :443)."
}

function step2_setup_tls() {
    # Generate the wildcard `*.${DOMAIN}` mkcert cert into
    # docker/nginx/certs/dev.{crt,key} BEFORE the compose up, so the nginx
    # container has a cert to load on first boot (it mounts ./nginx/certs).
    # Idempotent -- re-runs overwrite in place. IDENTITY_BOOTSTRAP_DOMAIN is
    # already exported by require_genesis so setup-tls picks the same domain.
    echo "[2/6] Generating the wildcard TLS cert (*.${CLUSTER_DOMAIN}) via mkcert..."
    if ! bash "${SCRIPT_DIR}/setup-tls.sh"; then
        echo "  WARNING: setup-tls.sh failed (mkcert missing?). nginx will fail"
        echo "  to start without docker/nginx/certs/dev.{crt,key}. Install mkcert"
        echo "  (brew install mkcert nss) and re-run 'make dev-cluster-refresh'."
    fi
}

function step3_point_seed_at_cluster() {
    # Route the secrets handshake + seed at the cluster TLS front door.
    # scripts/secrets {health,seed} both go through connectGRPC(), which
    # honours MEMQL_GRPC_ENDPOINT; bff.${DOMAIN}:443 carries the :443 suffix
    # so it auto-selects TLS against the mkcert-trusted cert. Exported into
    # THIS shell so both step5 (health wait) and step6 (seed) see it.
    export MEMQL_GRPC_ENDPOINT="${CLUSTER_GRPC_ENDPOINT}"
    echo "[3/6] Seed + handshake endpoint -> ${CLUSTER_GRPC_ENDPOINT} (cluster gRPC front door)."
}

function step4_wipe_and_restart() {
    echo "[4/6] Wiping cockpit's cached credentials for 'local'..."
    # The DB wipe below invalidates any owner identity registered against
    # the previous boot. A stale cached token makes memql-cockpit silently
    # fail the dial post-refresh (token not yet expired -> cockpit skips
    # the login prompt; server rejects silently -> "Connecting..." forever).
    # Only the 'local' cluster's credentials are touched.
    rm -f "${HOME}/.memql/credentials/local.json"

    echo "[4/6] Cleaning up containers from the retired single-node stack..."
    # cleanup_sibling_compose_modes tears down the retired single-node
    # project (by name) so any leftover container releases the host ports
    # the cluster binds (80 / 443 / 7880 / 5432). The owner is expected to
    # have torn it down, but this makes the refresh self-sufficient.
    cleanup_sibling_compose_modes
    nuke_stray_memql_containers

    echo "[4/6] Stopping the cluster + wiping volumes (incl. orphans) for a clean DB..."
    # down -v drops the named postgres volume so the cluster boots from an
    # empty DB -- the acceptance's "boots from a clean DB" precondition.
    $LIB_COMPOSE $LIB_COMPOSE_FILE_CLUSTER down -v --remove-orphans

    # Trim the BuildKit cache + dangling images to a cap BEFORE the build
    # so an unbounded cache can't fill the disk and kill `up` with "no
    # space left on device". Best-effort.
    echo "[4/6] Pruning stale Docker build cache..."
    lib_prune_docker_build_cache

    echo "[4/6] Rebuilding + starting the 2-replica parity cluster..."
    # This is heavy: 6 carrier/engine images + the copresent SPA. Allow
    # several minutes on a cold cache. The cluster compose reads the
    # decrypted genesis via env_file: ${GENESIS_ENV_FILE:-../.env.local}.
    $LIB_COMPOSE $LIB_COMPOSE_FILE_CLUSTER up --build -d --remove-orphans
}

function step5_wait_for_ready() {
    # The cluster's front door is TLS on :443 (bff.${DOMAIN}), so we wait on
    # its /healthz. /healthz answering means nginx is routing to a live bff
    # replica. We then confirm the gRPC front door with the shared
    # wait_for_memql (which dials MEMQL_GRPC_ENDPOINT=bff.${DOMAIN}:443 over
    # TLS thanks to step3).
    echo "[5/6] Waiting for the cluster front door (${CLUSTER_HTTP_FRONT}/healthz)..."
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

    echo "[5/6] Confirming the cluster gRPC handshake on ${CLUSTER_GRPC_ENDPOINT}..."
    if ! wait_for_memql 120 3; then
        cat <<EOF

  WARNING: the cluster gRPC front door did not complete a handshake
  within 120s. Going to attempt the seed anyway -- if it fails, check
  'make dev-cluster-logs', then once the cluster is responsive re-run:
      MEMQL_GRPC_ENDPOINT=${CLUSTER_GRPC_ENDPOINT} go run ./scripts/secrets seed --env-file="$GENESIS_ENV_FILE"

EOF
    fi
}

function step6_seed_and_finish() {
    echo "[6/6] Re-seeding secrets + variables from genesis into the cluster..."
    # MEMQL_GRPC_ENDPOINT is already exported (step3) so the seed targets
    # the cluster TLS front door.
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
# for the cluster's TLS subdomain front door (memql#1313).
function print_cluster_status_block() {
    cat <<EOF

  -----------------------------------------------------------
  dev-cluster-refresh complete. The 2-replica staging-parity
  cluster is up (clean DB, reseeded from genesis).

  Front door (TLS *.${CLUSTER_DOMAIN} subdomains, :443 -- parity
  with staging's per-subdomain ingress):
    https://app.${CLUSTER_DOMAIN}        SPA + auth + /memql/ws
    https://bff.${CLUSTER_DOMAIN}/healthz  Health probe
    https://bff.${CLUSTER_DOMAIN}        BFF gRPC + HTTP (MemqlService / NodeService)
    https://identity.${CLUSTER_DOMAIN}   Identity admin / login / JWKS
    https://agent.${CLUSTER_DOMAIN}      Agent (WorkerService.Stream)
    ws://localhost:7880          LiveKit (dev key 'devkey' / 'secret')

  Parity litmus (distinct per-replica node ids):
    make dev-cluster-status

  Other handy:
    make dev-cluster-logs    Follow cluster logs
    make dev-cluster-down    Stop the cluster (keeps volumes)

  For the CoPresent frontend the SPA is already served at the front
  door above -- open https://app.${CLUSTER_DOMAIN} and sign in.
  -----------------------------------------------------------

EOF
}

# -----------------------------------------------------------------
# Entry
# -----------------------------------------------------------------

function step0_install_deps() {
    # Idempotent + fast when everything's already installed; only the
    # first run on a new machine actually does work.
    echo "[0/6] Verifying dev dependencies..."
    bash "${SCRIPT_DIR}/install-deps.sh" >/dev/null || {
        echo "  ERROR: dependency check failed. Run 'make install-deps' for the full diagnostic."
        exit 1
    }
}

function main() {
    check_docker
    step0_install_deps
    step1_load_genesis
    step2_setup_tls
    step3_point_seed_at_cluster
    # memql#374: the DB wipe forces identity to re-fire the owner
    # "Claim ownership" email; the suppress knob writes the clusterSettings
    # row but skips the issue so multi-iteration debug sessions don't stack
    # 5-10+ emails. The operator can still claim ownership via /setup.
    export MAIL_SUPPRESS_OWNER_BOOTSTRAP=1
    step4_wipe_and_restart
    step5_wait_for_ready
    step6_seed_and_finish
}

main "$@"
