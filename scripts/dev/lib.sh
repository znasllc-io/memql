#!/usr/bin/env bash
#
# scripts/dev/lib.sh
# ==================
#
# Shared function library for memQL's dev-workflow scripts. Every
# script under scripts/dev/ sources this and uses its functions
# rather than inlining ad-hoc shell. Per the repo convention (see
# CLAUDE.md): bash scripts use function-based structure -- one
# function per responsibility, main() calls them in order.

# -----------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------

readonly LIB_COMPOSE="docker compose"
readonly LIB_COMPOSE_FILE_FULL="-f docker/docker-compose.full.yml"
readonly LIB_COMPOSE_FILE_CLUSTER="-f docker/docker-compose.cluster.yml"
readonly LIB_COMPOSE_FILE_POLYPHON="-f docker/docker-compose.full.yml -f docker/docker-compose.polyphon.yml"
readonly LIB_COMPOSE_FILE_NEMOCLAW="-f docker/docker-compose.full.yml -f docker/docker-compose.nemoclaw.yml"

# Domain anchor for dev URLs. Lets `lib_grpc_url` and the printed
# status block stay in sync with whatever IDENTITY_BOOTSTRAP_DOMAIN
# the operator runs the cluster under (default local.znas.io).
function lib_domain() {
    local d="${IDENTITY_BOOTSTRAP_DOMAIN:-}"
    if [ -z "$d" ] && [ -f .env.local ]; then
        d=$(grep -E '^IDENTITY_BOOTSTRAP_DOMAIN=' .env.local 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")
    fi
    echo "${d:-local.znas.io}"
}

# Host ports we attempt to free of stray processes after
# `compose down`. Scope: dev-tooling ports only -- the kind a
# developer might still have bound from a `go run main.go` started
# outside the docker stack. Containers handle their own port
# release on `compose down`; this set is a belt-and-suspenders
# cleanup for non-docker stragglers.
#
# Deliberately EXCLUDED:
#   :80, :443  -- privileged / conventional. macOS routinely binds
#                 :443 from system services (Personal Web Sharing,
#                 sometimes AirTunes/AirPlay). Killing those would
#                 break the user's machine, not their dev loop.
#                 nginx releases these cleanly on `compose down`.
#   :5432      -- a dev might run a homebrew postgres for other
#                 projects on the same port. Conflicts surface as
#                 a clean compose-up error.
readonly LIB_MEMQL_HOST_PORTS=("8088" "8081")

# -----------------------------------------------------------------
# Functions
# -----------------------------------------------------------------

# check_docker errors out with friendly guidance if the Docker
# daemon isn't running. Exits 0 (NOT 1) from the calling script so
# `make` doesn't flag it as a build error -- the situation is "you
# haven't started Docker yet", not "your code is broken".
function check_docker() {
    if docker info >/dev/null 2>&1; then
        return 0
    fi
    cat <<'EOF'

  Docker isn't running.

  This command needs the Docker daemon up so docker compose can
  build + run the memQL containers. On macOS: open Docker Desktop.
  On Linux: 'systemctl start docker' (or your distro's equivalent).

  Once Docker is up, re-run this command.

EOF
    exit 0
}

# require_master_key extracts the master key from
# ~/.memql/dev-secrets.yaml via 'go run ./scripts/secrets master-key'
# and exports it as MEMQL_MASTER_KEY. If the yaml has no masterKey
# field, prints first-run guidance and exits 0.
function require_master_key() {
    local key
    key=$(go run ./scripts/secrets master-key 2>/dev/null || true)
    if [ -z "$key" ]; then
        cat <<'EOF'

  Looks like this is your first run.

  'make dev-refresh' wipes the local memQL database and restores
  secrets + variables from your personal yaml at
  ~/.memql/dev-secrets.yaml. That yaml needs a master encryption
  key (used to seal secret values) before any of this can run.

  Please run this first:

      make secrets-init

  It walks you through generating the master key (or pasting one
  from another machine) and filling in the API keys + variables
  this dev environment needs. Once that's done, 'make dev-refresh'
  will work.

EOF
        exit 0
    fi
    export MEMQL_MASTER_KEY="$key"
}

# cleanup_sibling_compose_modes runs `docker compose down
# --remove-orphans` against the cluster / nemoclaw / base-full compose
# configs. dev-refresh now targets docker-compose.full.yml +
# docker-compose.polyphon.yml together (the LIB_COMPOSE_FILE_POLYPHON
# combo) so voice testing works out of the box; we don't list polyphon
# as a sibling because that's the active mode. The base-full file is
# listed so a dev who previously ran the basic stack (no overlay) gets
# its container set torn down before the polyphon stack starts.
# No-ops if a given mode has nothing running.
function cleanup_sibling_compose_modes() {
    local sibling
    for sibling in \
        "$LIB_COMPOSE_FILE_CLUSTER" \
        "$LIB_COMPOSE_FILE_FULL" \
        "$LIB_COMPOSE_FILE_NEMOCLAW" \
    ; do
        $LIB_COMPOSE $sibling down --remove-orphans 2>/dev/null || true
    done
}

# nuke_stray_memql_containers is the belt-and-suspenders cleanup for
# the rare case where containers got started outside any compose
# project (`docker run` directly, leftovers from a renamed compose
# project, etc.). Force-removes anything matching `memql-*` that
# isn't currently part of an active compose project. Quiet on success;
# tolerates "no matches" gracefully.
function nuke_stray_memql_containers() {
    local strays
    strays=$(docker ps -a --filter "name=^memql-" --format "{{.ID}}" 2>/dev/null || true)
    if [ -n "$strays" ]; then
        # shellcheck disable=SC2086
        docker rm -f $strays >/dev/null 2>&1 || true
    fi
}

# wait_for_memql polls 'go run ./scripts/secrets health' until it
# reports "ok" or the timeout expires. Returns 0 on success, 1 on
# timeout. Callers decide whether to bail or proceed.
#
# Surfaces the underlying error from each attempt every 15s so a
# stuck handshake doesn't just look like a silent hang. The error
# tells you whether DNS is missing, the cert chain is wrong, the
# bff isn't accepting yet, or the operator credential is being
# rejected.
#
# Args:
#   $1 = total timeout in seconds (default 120)
#   $2 = poll interval in seconds (default 3)
function wait_for_memql() {
    local timeout=${1:-120}
    local interval=${2:-3}
    local waited=0
    local last_diag=""
    while [ "$waited" -lt "$timeout" ]; do
        local out
        out=$(go run ./scripts/secrets health 2>&1)
        if echo "$out" | grep -q "^ok$"; then
            echo "       memQL gRPC handshake ok after ${waited}s."
            return 0
        fi
        # Print the live error every 15s so the user can see what's
        # wrong while the loop continues. The first failure prints
        # at t=0 too.
        if [ "$waited" = "0" ] || [ $((waited % 15)) -eq 0 ]; then
            local diag
            diag=$(echo "$out" | tail -1)
            if [ "$diag" != "$last_diag" ]; then
                echo "       still waiting (${waited}s) -- last error: ${diag}"
                last_diag="$diag"
            fi
        fi
        sleep "$interval"
        waited=$((waited + interval))
    done
    return 1
}

# print_dev_status_block prints URLs + next-step commands. Called
# at the end of a successful dev-refresh.
function print_dev_status_block() {
    local domain
    domain=$(lib_domain)
    cat <<EOF

  -----------------------------------------------------------
  dev-refresh complete. Stack is up with full identity flow.

  Single TLS entry point (nginx :443):
    https://app.${domain}        SPA   (Vite proxy on host:8080)
    https://identity.${domain}   Auth, /admin, /pair, JWKS
    https://bff.${domain}        BFF   (gRPC + WS bridge)
    https://agent.${domain}      Agent (WorkerService.Stream)
    Health probe:                https://bff.${domain}/healthz

  Other handy:
    make dev-status       Status snapshot (this stack)
    make dev-logs         Follow logs
    make dev-stop         Stop the stack
  -----------------------------------------------------------

EOF
}

# print_section prints a labelled section header for the dev-status
# script. Centralized so all sections format the same way.
function print_section() {
    local title="$1"
    echo "$title"
}

# section_docker prints the docker-daemon status section.
function section_docker() {
    print_section "Docker daemon:"
    if docker info >/dev/null 2>&1; then
        echo "  running"
    else
        echo "  NOT RUNNING (start Docker Desktop / systemctl start docker)"
    fi
    echo ""
}

# section_grpc prints the memQL gRPC handshake status section.
function section_grpc() {
    local domain
    domain=$(lib_domain)
    print_section "memQL gRPC (https://bff.${domain}):"
    local health
    health=$(go run ./scripts/secrets health 2>&1)
    echo "  $health"
    echo ""
}

# section_containers prints the docker-compose ps output.
function section_containers() {
    print_section "Compose containers (full + polyphon stack):"
    if ! $LIB_COMPOSE $LIB_COMPOSE_FILE_POLYPHON ps 2>/dev/null; then
        echo "  (compose not reachable)"
    fi
    echo ""
}

# section_urls prints the dev URL reference list.
function section_urls() {
    local domain
    domain=$(lib_domain)
    cat <<EOF
URLs (when memQL is up):
  SPA:                     https://app.${domain}
  Identity / admin:        https://identity.${domain}
  BFF (gRPC + WS):         https://bff.${domain}
  Agent (gRPC):            https://agent.${domain}
  bff direct (debug):      http://localhost:8088
  identity direct (debug): http://localhost:8081
  Postgres:                localhost:5432  user=memql password=memql_dev

For the CoPresent frontend, run:
  cd ../copresent && make dev-refresh
Then open the SPA URL above.
EOF
}

# port_in_use returns 0 if something is currently listening on the
# given TCP port (any address), 1 otherwise. Prefers `lsof`
# (default on macOS), falls back to `nc -z` on Linux distros that
# ship lsof behind a separate package. Returns 1 ("treat as free")
# if neither tool is available.
function port_in_use() {
    local port="$1"
    if command -v lsof >/dev/null 2>&1; then
        lsof -i ":${port}" -sTCP:LISTEN >/dev/null 2>&1
        return $?
    fi
    if command -v nc >/dev/null 2>&1; then
        nc -z localhost "$port" >/dev/null 2>&1
        return $?
    fi
    return 1
}

# pids_listening_on returns whitespace-separated PIDs of every
# process currently bound LISTEN to the given TCP port -- EXCLUDING
# Docker Desktop's own forwarder process. Empty string when nothing
# (else) is bound.
#
# Why the exclusion: on macOS, Docker Desktop uses a single backend
# process (com.docker.backend) to vend host port forwarding for every
# running container. lsof -i shows that process bound to whatever
# host port the container exposes. We DO NOT want to kill it -- doing
# so takes Docker Desktop down. We resolve each PID's full command
# path via `ps -o command=` and skip anything matching Docker. If
# Docker is fully shut down, no listeners remain for those ports
# anyway, so this filter is a no-op in the common case.
#
# Helper callers (kill_pids_gracefully, free_memql_host_ports,
# free_dev_ports) only see "real" stray PIDs -- a leftover `go run`,
# a stray vite, etc.
function pids_listening_on() {
    local port="$1"
    if ! command -v lsof >/dev/null 2>&1; then
        return 0
    fi
    local raw_pids
    raw_pids=$(lsof -ti ":${port}" -sTCP:LISTEN 2>/dev/null)
    if [ -z "$raw_pids" ]; then
        return 0
    fi
    local pid command result=""
    for pid in $raw_pids; do
        command=$(ps -p "$pid" -o command= 2>/dev/null || true)
        # Skip Docker Desktop's host-port forwarder. Pattern covers
        # macOS (/Applications/Docker.app/Contents/MacOS/com.docker.*)
        # and Linux (docker-proxy binary, dockerd).
        if [[ "$command" == *"com.docker"* ]] \
            || [[ "$command" == *"/Docker.app/"* ]] \
            || [[ "$command" == *"docker-proxy"* ]] \
            || [[ "$command" == *"dockerd"* ]]; then
            continue
        fi
        result="${result}${pid} "
    done
    echo "$result"
}

# kill_pids_gracefully sends SIGTERM, waits up to 3 seconds for
# the processes to exit, then escalates to SIGKILL. Quiet on
# nothing-to-do; loud on actual kills so the user isn't surprised
# when their other terminal's `go run` / stray process dies.
function kill_pids_gracefully() {
    local pids="$1"
    local label="${2:-process}"
    if [ -z "$(echo "$pids" | tr -d ' ')" ]; then
        return 0
    fi
    echo "[dev-refresh] stopping existing ${label} (pids:$pids)..."
    # shellcheck disable=SC2086
    kill -TERM $pids 2>/dev/null || true
    local waited=0
    while [ $waited -lt 30 ]; do
        local still_alive=""
        local pid
        for pid in $pids; do
            if kill -0 "$pid" 2>/dev/null; then
                still_alive="${still_alive} ${pid}"
            fi
        done
        if [ -z "$(echo "$still_alive" | tr -d ' ')" ]; then
            return 0
        fi
        sleep 0.1
        waited=$((waited + 1))
    done
    # shellcheck disable=SC2086
    kill -KILL $pids 2>/dev/null || true
    sleep 0.2
}

# free_memql_host_ports kills any non-docker process still bound
# to the memql-specific host ports AFTER `docker compose down` has
# removed the docker containers and their port forwarding. Anything
# still bound at that point is a stray process the developer
# launched outside the compose stack -- typically `go run main.go`
# from a previous test session. The whole point of `make dev-refresh`
# is "give me a fresh, known-good stack", so we kill them rather
# than leaving the next compose-up to fail with bind() errors.
#
# Postgres (5432) is excluded from the kill set because a developer
# might run a homebrew postgres for other projects on the same
# port; if it conflicts, compose-up will error visibly and the dev
# can decide what to stop manually.
function free_memql_host_ports() {
    local any_killed=""
    local port pids
    for port in "${LIB_MEMQL_HOST_PORTS[@]}"; do
        pids=$(pids_listening_on "$port")
        if [ -n "$(echo "$pids" | tr -d ' ')" ]; then
            kill_pids_gracefully "$pids" "process on port ${port}"
            any_killed="yes"
        fi
    done
    if [ -z "$any_killed" ]; then
        return 0
    fi
    # Verify all targeted ports are actually free now. Give the OS a
    # brief grace window to release sockets in TIME_WAIT.
    local waited=0
    while [ $waited -lt 20 ]; do
        local still_bound=""
        for port in "${LIB_MEMQL_HOST_PORTS[@]}"; do
            if port_in_use "$port"; then
                still_bound="${still_bound} ${port}"
            fi
        done
        if [ -z "$(echo "$still_bound" | tr -d ' ')" ]; then
            return 0
        fi
        sleep 0.1
        waited=$((waited + 1))
    done
    cat <<EOF

  Couldn't free memQL host ports automatically. Some processes are
  still bound after kill -- run 'lsof -i :8088 -i :8081' to
  inspect, then kill them by hand and re-run 'make dev-refresh'.

EOF
    exit 1
}

# -----------------------------------------------------------------
# ngrok: shared with refresh.sh + ngrok-up.sh
# -----------------------------------------------------------------

readonly LIB_NGROK_LIVEKIT_PORT=7880
readonly LIB_NGROK_API="http://127.0.0.1:4040/api/tunnels"
readonly LIB_NGROK_LOG="/tmp/ngrok-livekit.log"

# lib_refresh_ngrok tears down any existing ngrok HTTP tunnel and
# starts a fresh one in front of localhost:LIB_NGROK_LIVEKIT_PORT
# (the polyphon-livekit container's published port), then rewrites
# LIVEKIT_PUBLIC_URL + POLYPHON_LIVEKIT_PUBLIC_URL in .env.local
# in-place so the rest of the stack picks up the new URL on the
# next service start.
#
# The function is best-effort: returns 1 (non-fatal) when ngrok
# is unavailable or .env.local isn't shaped right, so refresh.sh
# can keep going without poisoning the whole dev-refresh flow.
# Real terminal failures (ngrok crashes mid-startup) still bubble
# up via set -e on the calling script.
#
# Used by scripts/dev/ngrok-up.sh (interactive, plus restarts
# services) and scripts/dev/refresh.sh (between docker down + up
# so services come up with the new URL on first boot).
function lib_refresh_ngrok() {
    if ! command -v ngrok >/dev/null 2>&1; then
        echo "  INFO: ngrok CLI not found -- skipping tunnel refresh."
        echo "        Install: brew install ngrok, then re-run."
        return 1
    fi
    if ! ngrok config check >/dev/null 2>&1; then
        echo "  INFO: ngrok config invalid -- skipping tunnel refresh."
        echo "        Run: ngrok config add-authtoken <token>"
        return 1
    fi
    if [ ! -f .env.local ]; then
        echo "  INFO: .env.local not found -- skipping ngrok refresh."
        return 1
    fi
    if ! grep -qE '^LIVEKIT_PUBLIC_URL=' .env.local; then
        echo "  INFO: .env.local has no LIVEKIT_PUBLIC_URL line -- skipping ngrok refresh."
        echo "        Add an empty placeholder if you want ngrok integration."
        return 1
    fi

    # Tear down any prior ngrok agent. Free tier allows one running
    # agent per machine and reuses port 4040 for its local API, so
    # a stale process would shadow the new tunnel's URL when we
    # poll the API. pkill returns non-zero if no match -- that's
    # fine, we just want a clean slate.
    if pgrep -f "ngrok http" >/dev/null 2>&1; then
        pkill -f "ngrok http" || true
        sleep 1
    fi

    echo "  Starting ngrok HTTPS tunnel for localhost:${LIB_NGROK_LIVEKIT_PORT}..."
    nohup ngrok http "${LIB_NGROK_LIVEKIT_PORT}" --log=stdout > "${LIB_NGROK_LOG}" 2>&1 &
    disown

    # Poll until the API is up AND the tunnel is registered with a
    # public URL. The two states aren't simultaneous: the inspector
    # API on :4040 comes up before ngrok finishes negotiating the
    # tunnel with their edge, so an "is the API responsive" check
    # exits the loop too early and we read back an empty tunnel
    # list. ngrok typically completes negotiation in ~1-2s; allow
    # 20s before giving up so a slow connection doesn't false-fail.
    local attempts=0
    local https_url=""
    while [ -z "$https_url" ]; do
        attempts=$((attempts + 1))
        if [ "$attempts" -gt 40 ]; then
            echo "  WARNING: ngrok tunnel didn't register within 20s. Tail:"
            tail -10 "${LIB_NGROK_LOG}" 2>/dev/null | sed 's/^/    /' || true
            return 1
        fi
        if curl -fs "${LIB_NGROK_API}" >/dev/null 2>&1; then
            https_url=$(curl -s "${LIB_NGROK_API}" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for t in data.get('tunnels', []):
    if t.get('proto') == 'https' and t.get('public_url'):
        print(t['public_url'])
        break
" 2>/dev/null || true)
        fi
        if [ -z "$https_url" ]; then
            sleep 0.5
        fi
    done

    local wss_url="${https_url/https:/wss:}"

    # In-place rewrite. macOS sed needs `-i ''`, GNU sed accepts
    # `-i` alone -- the .bak form below works on both.
    sed -i.bak \
        -e "s|^LIVEKIT_PUBLIC_URL=.*|LIVEKIT_PUBLIC_URL=${wss_url}|" \
        -e "s|^POLYPHON_LIVEKIT_PUBLIC_URL=.*|POLYPHON_LIVEKIT_PUBLIC_URL=${wss_url}|" \
        .env.local
    rm -f .env.local.bak

    echo "  ngrok up: ${https_url}"
    echo "  .env.local updated: LIVEKIT_PUBLIC_URL + POLYPHON_LIVEKIT_PUBLIC_URL=${wss_url}"
    return 0
}
