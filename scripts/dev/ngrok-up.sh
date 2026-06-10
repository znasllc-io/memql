#!/usr/bin/env bash
#
# scripts/dev/ngrok-up.sh
# =======================
#
# Bring up (or refresh) an ngrok HTTPS tunnel in front of the local
# LiveKit container so external services (Anam's cloud avatar
# engine, etc.) can dial in. Calls `lib_refresh_ngrok` from lib.sh
# -- the same function `make dev-refresh` invokes mid-flow -- so
# the two scripts stay in lockstep.
#
# Use this standalone (outside a full refresh) when the ngrok URL
# rotates mid-session or you forgot to start the tunnel up-front;
# the function tears down any existing ngrok agent and starts a
# fresh tunnel, then this script restarts the affected services so
# the new URL takes hold without a full stack wipe.
#
# Prerequisites:
#   - ngrok CLI installed on the host (brew install ngrok)
#   - ngrok authtoken saved (ngrok config add-authtoken <token>)
#   - polyphon-livekit container running
#
# Idempotent: re-run any time to refresh.

set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

#=============================================================================
# FUNCTIONS
#=============================================================================

function warn_overlay_pending() {
    # The voice/avatar overlay (docker-compose.polyphon.yml, the
    # polyphon-livekit container + voice-agent) rode the retired single-node
    # stack (memql#1311) and is pending re-home onto the parity cluster
    # (memql#1310). The cluster ships its own `livekit` service, but the
    # avatar TURN-relay path this ngrok tunnel feeds is not wired to it yet.
    echo "NOTE: voice/avatar overlay pending re-home onto the cluster -- see memql#1310."
    echo "      This ngrok/TURN path targeted the retired single-node stack; it may"
    echo "      not connect to the cluster's livekit service until #1310 lands."
    echo ""
}

function require_livekit() {
    # The cluster's LiveKit container is `memql-livekit`; the retired
    # overlay used `polyphon-livekit`. Accept either so this keeps working
    # once the overlay is re-homed (memql#1310).
    if ! docker ps --format '{{.Names}}' | grep -qE '^(memql|polyphon)-livekit$'; then
        echo "ERROR: no LiveKit container is running."
        echo "       Start the parity cluster first: make dev-cluster-up"
        echo "       Or run a full refresh: make dev-cluster-refresh"
        exit 1
    fi
}

function refresh_tunnel() {
    # Delegates to the shared lib function. Non-zero return is a
    # known-skipped state (ngrok missing, .env.local not seeded, etc.)
    # and is logged inside the function; treat it as a hard error
    # here since the user explicitly invoked this script asking for
    # a tunnel.
    if ! lib_refresh_ngrok; then
        echo ""
        echo "ERROR: ngrok refresh failed -- see message above."
        exit 1
    fi
}

function restart_dependent_services() {
    echo ""
    echo "Restarting bff + livekit so they pick up the new URL..."
    # The voice-agent service rode the retired overlay (memql#1310, see the
    # note above); restart only the cluster services that exist today.
    $LIB_COMPOSE $LIB_COMPOSE_FILE_CLUSTER restart bff livekit >/dev/null
}

function print_summary() {
    local public_url
    public_url=$(grep -E '^LIVEKIT_PUBLIC_URL=' .env.local | tail -1 | cut -d= -f2-)
    cat <<EOF

========================================
ngrok tunnel up
========================================
Public LiveKit signaling: ${public_url}
Inspector UI:             http://127.0.0.1:4040
Logs:                     ${LIB_NGROK_LOG}

Next: reload the browser, click mic + video. Anam's avatar should
join via this URL.

Static-domain tip: ngrok free accounts get 1 reserved domain.
Reserve it in the ngrok dashboard, then we can extend
lib_refresh_ngrok to pin to it across restarts. Ask when you
want that.
EOF
}

function main() {
    check_docker
    warn_overlay_pending
    require_livekit
    refresh_tunnel
    restart_dependent_services
    print_summary
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
